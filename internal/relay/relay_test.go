package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pcunning/uv5r-ble-relay/internal/bleconn"
	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
	"github.com/pcunning/uv5r-ble-relay/internal/translate"
)

// duplexEnd implements io.ReadWriteCloser by composing two io.Pipe pairs.
type duplexEnd struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	closeFn func() error
}

func (e *duplexEnd) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *duplexEnd) Write(p []byte) (int, error) { return e.w.Write(p) }
func (e *duplexEnd) Close() error                { return e.closeFn() }

// newDuplex returns two endpoints connected back-to-back: bytes written to
// one come out of the other.
func newDuplex() (relayEnd, hostEnd *duplexEnd) {
	aR, aW := io.Pipe() // host -> relay
	bR, bW := io.Pipe() // relay -> host
	closeAll := func() error {
		_ = aR.Close()
		_ = aW.Close()
		_ = bR.Close()
		_ = bW.Close()
		return nil
	}
	return &duplexEnd{r: aR, w: bW, closeFn: closeAll},
		&duplexEnd{r: bR, w: aW, closeFn: closeAll}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// startRelay wires a fake radio + duplex pty replacement to relay.Run and
// returns the host-side endpoint, the fake, and a cleanup function.
func startRelay(t *testing.T, fragmented bool) (host *duplexEnd, fake *bleconn.FakeTransport, done <-chan error, cancel func()) {
	t.Helper()
	relayEnd, hostEnd := newDuplex()
	f := bleconn.NewFakeTransport()
	f.SetFragmented(fragmented)

	ctx, c := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, relayEnd, f, quietLogger())
	}()
	return hostEnd, f, errCh, c
}

func writeAll(t *testing.T, w io.Writer, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readN(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()
	buf := make([]byte, n)
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		got, err := io.ReadFull(r, buf)
		ch <- res{got, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			t.Fatalf("read %d bytes: got %d, err=%v", n, x.n, x.err)
		}
		return buf
	case <-time.After(timeout):
		t.Fatalf("timeout reading %d bytes", n)
		return nil
	}
}

func TestRelay_IdentificationHandshake(t *testing.T) {
	host, _, done, cancel := startRelay(t, false)
	defer func() {
		cancel()
		<-done
	}()

	// 1. PROGRAM
	writeAll(t, host, []byte(protocol.IdentRequest))
	if got := readN(t, host, 1, 2*time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("PROGRAM ack=0x%X", got[0])
	}
	// 2. F
	writeAll(t, host, []byte{protocol.CmdF})
	got := readN(t, host, protocol.FDescriptorLen, 2*time.Second)
	if !bytes.Equal(got, protocol.FDescriptor) {
		t.Fatalf("F descriptor mismatch: %x", got)
	}
	// 3. M
	writeAll(t, host, []byte{protocol.CmdM})
	got = readN(t, host, protocol.MModelLen, 2*time.Second)
	if !bytes.Equal(got, []byte(protocol.MModel)) {
		t.Fatalf("M reply mismatch: %q", got)
	}
	// 4. SEND!
	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	writeAll(t, host, send)
	if got := readN(t, host, 1, 2*time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("SEND! ack=0x%X", got[0])
	}
}

func TestRelay_FullUploadSequence(t *testing.T) {
	host, _, done, cancel := startRelay(t, false)
	defer func() {
		cancel()
		<-done
	}()

	// Background reader counts bytes received.
	var ackCount atomic.Int32
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 1024)
		for {
			n, err := host.Read(buf)
			if n > 0 {
				ackCount.Add(int32(n))
			}
			if err != nil {
				return
			}
		}
	}()

	// Ident handshake (2 ACKs).
	writeAll(t, host, []byte(protocol.IdentRequest))
	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	writeAll(t, host, send)

	// Walk MEM_STARTS / MEM_SIZES in 128-byte blocks (floor).
	wblocks := 0
	for i, start := range protocol.MemStarts {
		size := int(protocol.MemSizes[i])
		nblocks := size / int(protocol.WriteBlockSize)
		for j := 0; j < nblocks; j++ {
			addr := uint32(start) + uint32(j)*uint32(protocol.WriteBlockSize)
			cmd := make([]byte, 4+int(protocol.WriteBlockSize))
			cmd[0] = protocol.OpWrite
			binary.BigEndian.PutUint16(cmd[1:3], uint16(addr))
			cmd[3] = protocol.WriteBlockSize
			writeAll(t, host, cmd)
			wblocks++
		}
	}
	expected := int32(2 + wblocks)
	if expected != 261 {
		t.Fatalf("test bug: expected count=%d, want 261", expected)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ackCount.Load() == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d ACK bytes, want %d", ackCount.Load(), expected)
}

func TestRelay_FullDownloadSequence(t *testing.T) {
	host, _, done, cancel := startRelay(t, false)
	defer func() {
		cancel()
		<-done
	}()

	var rxCount atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := host.Read(buf)
			if n > 0 {
				rxCount.Add(int32(n))
			}
			if err != nil {
				return
			}
		}
	}()

	// Walk all read addresses in 64-byte increments.
	totalReads := 0
	for i, start := range protocol.MemStarts {
		size := int(protocol.MemSizes[i])
		nblocks := size / int(protocol.ReadBlockSize)
		for j := 0; j < nblocks; j++ {
			addr := uint32(start) + uint32(j)*uint32(protocol.ReadBlockSize)
			cmd := []byte{protocol.OpRead, 0, 0, protocol.ReadBlockSize}
			binary.BigEndian.PutUint16(cmd[1:3], uint16(addr))
			writeAll(t, host, cmd)
			totalReads++
		}
	}
	if totalReads != 521 {
		t.Fatalf("test bug: totalReads=%d want 521", totalReads)
	}

	expected := int32(protocol.TotalMemBytes + 4*521)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rxCount.Load() == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d bytes, want %d", rxCount.Load(), expected)
}

func TestRelay_FragmentedNotifications(t *testing.T) {
	host, _, done, cancel := startRelay(t, true)
	defer func() {
		cancel()
		<-done
	}()

	// Single 0x52 read should yield 4+64=68 bytes delivered as 20-byte frags.
	writeAll(t, host, []byte{protocol.OpRead, 0x12, 0x34, protocol.ReadBlockSize})
	got := readN(t, host, 68, 2*time.Second)
	if got[0] != protocol.OpRead || got[1] != 0x12 || got[2] != 0x34 || got[3] != protocol.ReadBlockSize {
		t.Fatalf("header mismatch: %x", got[:4])
	}
}

func TestRelay_PtyClosePropagates(t *testing.T) {
	host, fake, done, _ := startRelay(t, false)
	// Close the host side — simulates CHIRP closing the PTY → relay sees EOF.
	_ = host.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after pty close")
	}
	// The transport should be closed too: Read returns EOF.
	buf := make([]byte, 1)
	if _, err := fake.Read(buf); err != io.EOF {
		t.Fatalf("transport not closed: err=%v", err)
	}
}

func TestRelay_Shutdown(t *testing.T) {
	host, _, done, cancel := startRelay(t, false)
	defer host.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Run did not return within 1s of cancel")
	}
}

// (no extra exports)

func TestRelay_WithTranslator_MainlineUpload(t *testing.T) {
	relayEnd, hostEnd := newDuplex()
	fake := bleconn.NewFakeTransport()
	tr := translate.New(fake, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, relayEnd, tr, quietLogger()) }()
	defer func() {
		cancel()
		<-errCh
	}()

	// Drain ACKs from the host side.
	var acks atomic.Int32
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := hostEnd.Read(buf)
			if n > 0 {
				acks.Add(int32(n))
			}
			if err != nil {
				return
			}
		}
	}()

	// Ident handshake (PROGRAM ack + F + M + SEND! ack = 1+16+15+1 = 33 bytes).
	writeAll(t, hostEnd, []byte(protocol.IdentRequest))
	writeAll(t, hostEnd, []byte{protocol.CmdF})
	writeAll(t, hostEnd, []byte{protocol.CmdM})
	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	writeAll(t, hostEnd, send)

	// Two consecutive 64-byte uploads at 0x0000 and 0x0040 → one 128-byte
	// inner write, two CHIRP-side ACKs.
	cmd := func(addr uint16, fill byte) []byte {
		out := []byte{protocol.OpWrite, byte(addr >> 8), byte(addr), byte(protocol.ReadBlockSize)}
		out = append(out, bytes.Repeat([]byte{fill}, int(protocol.ReadBlockSize))...)
		return out
	}
	writeAll(t, hostEnd, cmd(0x0000, 0xAA))
	writeAll(t, hostEnd, cmd(0x0040, 0xBB))

	want := int32(1 + 16 + 15 + 1 + 2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acks.Load() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d ack/reply bytes, want %d", acks.Load(), want)
}
