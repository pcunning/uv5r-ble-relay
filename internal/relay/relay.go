// Package relay shuttles bytes between an io.ReadWriteCloser PTY and a
// bleconn.Transport. It is the only place that wires the two halves together.
package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// Run pumps bytes both ways between pty and transport until ctx is cancelled
// or either side returns an error. On exit it closes both ends so the peer
// observes EOF.
func Run(ctx context.Context, pty io.ReadWriteCloser, t io.ReadWriteCloser, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		err := pump(gctx, pty, t, "pty->transport", logger)
		_ = t.Close()
		return err
	})
	g.Go(func() error {
		err := pump(gctx, t, pty, "transport->pty", logger)
		_ = pty.Close()
		return err
	})

	// Shutdown watcher: when the context is cancelled, forcibly unblock both
	// sides. tripDeadline wakes a blocked PTY read on macOS; Close unblocks a
	// blocked NotifyQueue/Translator read. We then wait up to 3 s for the
	// pumps to exit; after that we close everything again in case a goroutine
	// is still stuck.
	shutdownCh := make(chan struct{})
	go func() {
		select {
		case <-gctx.Done():
		case <-shutdownCh:
			return
		}
		logger.Debug("relay: context done, closing both sides")
		tripDeadline(pty)
		tripDeadline(t)
		_ = pty.Close()
		_ = t.Close()

		// Hard-close after 3 s if pumps still haven't returned.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-shutdownCh:
		case <-timer.C:
			logger.Warn("relay: shutdown timed out, forcing close")
			_ = pty.Close()
			_ = t.Close()
		}
	}()

	err := g.Wait()
	close(shutdownCh)
	if err != nil && !isExpectedShutdown(err, ctx) {
		return err
	}
	return nil
}

func pump(ctx context.Context, src io.Reader, dst io.Writer, name string, logger *slog.Logger) error {
	type readResult struct {
		data []byte
		err  error
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Run each Read in a goroutine so we can select on context cancellation.
		// This is necessary on Linux where os.NewFile-wrapped PTY fds are not
		// registered with the netpoller and Close/SetReadDeadline from another
		// goroutine does not interrupt a blocked Read.
		ch := make(chan readResult, 1)
		go func() {
			buf := make([]byte, 4096)
			n, err := src.Read(buf)
			ch <- readResult{buf[:n], err}
		}()

		var r readResult
		select {
		case <-ctx.Done():
			return nil
		case r = <-ch:
		}

		if len(r.data) > 0 {
			logger.Debug("relay: read chunk", "dir", name, "bytes", len(r.data))
			if _, werr := dst.Write(r.data); werr != nil {
				logger.Debug("relay: write error", "dir", name, "err", werr)
				return werr
			}
			logger.Debug("relay: forwarded chunk", "dir", name, "bytes", len(r.data))
		}
		if r.err != nil {
			if r.err == io.EOF {
				logger.Debug("relay: EOF", "dir", name)
				return nil
			}
			logger.Debug("relay: read error", "dir", name, "err", r.err)
			return r.err
		}
	}
}

func isExpectedShutdown(err error, ctx context.Context) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctx.Err() != nil {
		// Context was cancelled — read/write errors that follow are expected.
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return false
}

// tripDeadline sets a past read deadline on rwc when supported, which is the
// only reliable way to interrupt a blocked Read on a PTY master on macOS.
func tripDeadline(rwc io.ReadWriteCloser) {
	if d, ok := rwc.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Unix(1, 0))
	}
}
