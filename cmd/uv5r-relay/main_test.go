package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
)

func TestMain_FakeMode(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "ttyBLE-test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exitCh := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		exitCh <- run(ctx, []string{"--fake", "--link", link}, &stderr, io.Discard)
	}()

	// Wait for the symlink to appear.
	slavePath := waitForSymlink(t, link, 3*time.Second)

	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscallNoctty(), 0)
	if err != nil {
		t.Fatalf("open slave %s: %v", slavePath, err)
	}
	defer slave.Close()

	// Perform the ident handshake.
	if _, err := slave.Write([]byte(protocol.IdentRequest)); err != nil {
		t.Fatalf("write PROGRAM: %v", err)
	}
	got := readN(t, slave, 1, 2*time.Second)
	if got[0] != protocol.IdentAck {
		t.Fatalf("PROGRAM ack=0x%X", got[0])
	}

	if _, err := slave.Write([]byte{protocol.CmdF}); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, slave, protocol.FDescriptorLen, 2*time.Second); !bytes.Equal(got, protocol.FDescriptor) {
		t.Fatalf("F mismatch: %x", got)
	}

	if _, err := slave.Write([]byte{protocol.CmdM}); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, slave, protocol.MModelLen, 2*time.Second); !bytes.Equal(got, []byte(protocol.MModel)) {
		t.Fatalf("M mismatch: %q", got)
	}

	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	if _, err := slave.Write(send); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, slave, 1, 2*time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("SEND ack=0x%X", got[0])
	}

	// Mainline-CHIRP-style 64-byte uploads driven through the default
	// translator. Two consecutive blocks at 0x0000/0x0040 form one paired
	// 128-byte inner write; both should ACK to CHIRP with 0x06.
	mkUpload := func(addr uint16, fill byte) []byte {
		out := []byte{protocol.OpWrite, byte(addr >> 8), byte(addr), byte(protocol.ReadBlockSize)}
		out = append(out, bytes.Repeat([]byte{fill}, int(protocol.ReadBlockSize))...)
		return out
	}
	if _, err := slave.Write(mkUpload(0x0000, 0xAA)); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, slave, 1, 2*time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("upload[0] ack=0x%X", got[0])
	}
	if _, err := slave.Write(mkUpload(0x0040, 0xBB)); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, slave, 1, 2*time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("upload[1] ack=0x%X", got[0])
	}

	// Trigger clean shutdown.
	cancel()
	select {
	case code := <-exitCh:
		if code != 0 {
			t.Fatalf("exit code %d, stderr=%s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("relay did not exit after cancel; stderr=%s", stderr.String())
	}
}

func waitForSymlink(t *testing.T, link string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		target, err := os.Readlink(link)
		if err == nil && target != "" {
			return target
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("symlink %s never appeared", link)
	return ""
}

func readN(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	buf := make([]byte, n)
	go func() {
		got, err := io.ReadFull(r, buf)
		ch <- res{got, err}
	}()
	select {
	case x := <-ch:
		if x.err != nil {
			t.Fatalf("read %d: got %d err=%v", n, x.n, x.err)
		}
		return buf
	case <-time.After(timeout):
		t.Fatalf("timeout reading %d", n)
		return nil
	}
}
