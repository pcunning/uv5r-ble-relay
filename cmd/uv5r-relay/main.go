// Command uv5r-relay bridges CHIRP to a Baofeng UV-5R Mini over BLE by
// exposing a local pseudo-terminal that CHIRP opens as a regular serial
// port.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/pcunning/uv5r-ble-relay/internal/bleconn"
	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
	"github.com/pcunning/uv5r-ble-relay/internal/ptylink"
	"github.com/pcunning/uv5r-ble-relay/internal/relay"
	"github.com/pcunning/uv5r-ble-relay/internal/translate"
)

// version is overwritten via -ldflags by release builds.
var version = "dev"

// Exit codes per SPEC §5.
const (
	exitOK             = 0
	exitRuntime        = 1
	exitNoBLE          = 2
	exitDeviceNotFound = 3
	exitBadArgs        = 64
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stderr, os.Stdout))
}

// watchSignals forwards the first SIGINT/SIGTERM to cancel and prints a hint.
// A second signal (or any signal after shutdownDone is closed) calls os.Exit(1)
// immediately so the process never hangs.
func watchSignals(stderr io.Writer, cancel context.CancelFunc, shutdownDone <-chan struct{}) {
	ch := make(chan os.Signal, 3)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			select {
			case <-shutdownDone:
				signal.Stop(ch)
				return
			case sig := <-ch:
				_ = sig
				fmt.Fprintln(stderr, "\n[relay] shutting down… (press Ctrl+C again to force quit)")
				cancel()
				// After the first signal, route any further signals straight to exit.
				go func() {
					select {
					case <-shutdownDone:
					case <-ch:
						fmt.Fprintln(stderr, "[relay] force quit")
						os.Exit(1)
					}
				}()
				return
			}
		}
	}()
}

type cliFlags struct {
	address     string
	name        string
	service     string
	char        string
	mtu         int
	pace        time.Duration
	scanTimeout time.Duration
	link        string
	trace       bool
	verbose     bool
	fake        bool
	passthrough bool
	scanList    bool
	showVersion bool
}

func parseFlags(args []string, stderr io.Writer) (*cliFlags, error) {
	fs := flag.NewFlagSet("uv5r-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &cliFlags{}
	fs.StringVar(&c.address, "address", "", "BLE peer address or CoreBluetooth UUID (skip scan)")
	fs.StringVar(&c.name, "name", "5R", "Substring match on advertised name")
	fs.StringVar(&c.service, "service", protocol.ServiceShort, "Service UUID hex")
	fs.StringVar(&c.char, "char", protocol.CharShort, "Characteristic UUID hex")
	fs.IntVar(&c.mtu, "mtu", 247, "Requested MTU (chunkSize = mtu-3)")
	fs.DurationVar(&c.pace, "pace", 0, "Inter-chunk delay")
	fs.DurationVar(&c.scanTimeout, "scan-timeout", 15*time.Second, "Scan window")
	fs.StringVar(&c.link, "link", "/tmp/ttyBLE", "Symlink to create for the PTY slave")
	fs.BoolVar(&c.trace, "trace", false, "Hex-dump every byte both directions to stderr")
	fs.BoolVar(&c.verbose, "verbose", false, "Verbose logging")
	fs.BoolVar(&c.fake, "fake", false, "Use in-memory fake radio instead of BLE")
	fs.BoolVar(&c.passthrough, "passthrough", false, "Disable the mainline-CHIRP translator (raw 128-byte uploads)")
	fs.BoolVar(&c.scanList, "scan-list", false, "Scan and list all nearby BLE devices then exit (useful for finding radio name/address)")
	fs.BoolVar(&c.showVersion, "version", false, "Print version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return c, nil
}

func run(parent context.Context, args []string, stderr, stdout io.Writer) int {
	flags, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintln(stderr, err)
		return exitBadArgs
	}
	if flags.showVersion {
		fmt.Fprintf(stdout, "uv5r-relay %s\n", version)
		return exitOK
	}

	level := slog.LevelInfo
	if flags.verbose || flags.trace {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	if flags.scanList {
		return runScanList(flags, stderr, logger)
	}

	pty, err := ptylink.Open(flags.link)
	if err != nil {
		logger.Error("pty open failed", "err", err)
		return exitRuntime
	}
	defer pty.Close()
	fmt.Fprintf(stderr, "[relay] PTY slave: %s\n", pty.SlavePath())
	if flags.link != "" {
		fmt.Fprintf(stderr, "[relay] symlink:   %s -> %s\n", flags.link, pty.SlavePath())
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	watchSignals(stderr, cancel, shutdownDone)

	var transport bleconn.Transport
	if flags.fake {
		fmt.Fprintln(stderr, "[relay] fake transport (no BLE)")
		transport = bleconn.NewFakeTransport()
	} else {
		fmt.Fprintf(stderr, "[relay] scanning for %q with service 0x%s…\n", flags.name, flags.service)
		t, err := bleconn.Connect(bleconn.BLEConfig{
			Address:     flags.address,
			NameFilter:  flags.name,
			ServiceUUID: flags.service,
			CharUUID:    flags.char,
			MTU:         flags.mtu,
			Pace:        flags.pace,
			ScanTimeout: flags.scanTimeout,
			Logger:      logger,
		})
		if err != nil {
			logger.Error("BLE connect failed", "err", err)
			if isDeviceNotFound(err) {
				return exitDeviceNotFound
			}
			return exitNoBLE
		}
		transport = t
	}
	if flags.trace {
		transport = newTracer(transport, stderr)
	}

	var chirpFacing io.ReadWriteCloser = transport
	if !flags.passthrough {
		fmt.Fprintln(stderr, "[relay] mainline-CHIRP translator enabled (use --passthrough to disable)")
		chirpFacing = translate.New(transport, logger)
	}

	fmt.Fprintln(stderr, "[relay] waiting for CHIRP to open the port…")
	if err := relay.Run(ctx, pty.Master(), chirpFacing, logger); err != nil {
		logger.Error("relay error", "err", err)
		return exitRuntime
	}
	return exitOK
}

func runScanList(flags *cliFlags, stderr io.Writer, logger *slog.Logger) int {
	fmt.Fprintf(stderr, "[relay] scanning all BLE devices for %s…\n", flags.scanTimeout)
	devices, err := bleconn.ScanAll(flags.scanTimeout, logger)
	if err != nil {
		fmt.Fprintf(stderr, "scan error: %v\n", err)
		return exitNoBLE
	}
	if len(devices) == 0 {
		fmt.Fprintln(stderr, "No BLE devices found. Is Bluetooth on?")
		return exitDeviceNotFound
	}
	fmt.Fprintf(stderr, "\n%-40s %-25s %5s  %s\n", "ADDRESS", "NAME", "RSSI", "HAS-FFE0")
	fmt.Fprintf(stderr, "%s\n", "────────────────────────────────────────────────────────────────────────────────")
	for _, d := range devices {
		name := d.Name
		if name == "" {
			name = "(unknown)"
		}
		ffe0 := ""
		if d.HasFfE0 {
			ffe0 = "yes"
		}
		fmt.Fprintf(stderr, "%-40s %-25s %5d  %s\n", d.Address, name, d.RSSI, ffe0)
	}
	fmt.Fprintf(stderr, "\nTip: use --name <substring> or --address <addr> to connect to a specific device.\n")
	return exitOK
}

func isDeviceNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "no matching BLE device") || contains(msg, "not found")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// tracer wraps a Transport and hex-dumps every Read/Write to w.
type tracer struct {
	inner io.ReadWriteCloser
	w     io.Writer
	mu    sync.Mutex
}

func newTracer(t io.ReadWriteCloser, w io.Writer) *tracer { return &tracer{inner: t, w: w} }

func (t *tracer) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.dump("<<", p[:n])
	}
	return n, err
}

func (t *tracer) Write(p []byte) (int, error) {
	t.dump(">>", p)
	return t.inner.Write(p)
}

func (t *tracer) Close() error { return t.inner.Close() }

func (t *tracer) dump(dir string, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.w, "%s %s\n", dir, hex.EncodeToString(b))
}
