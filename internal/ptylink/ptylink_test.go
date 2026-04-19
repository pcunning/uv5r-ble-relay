package ptylink

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpen_ReturnsSlavePath(t *testing.T) {
	p, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	st, err := os.Stat(p.SlavePath())
	if err != nil {
		t.Fatalf("stat slave: %v", err)
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("slave is not a char device: mode=%v", st.Mode())
	}
}

func TestOpen_Roundtrip(t *testing.T) {
	p, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	slave, err := os.OpenFile(p.SlavePath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open slave: %v", err)
	}
	defer slave.Close()

	// master -> slave
	want := []byte("hello\x01\x02")
	if _, err := p.Master().Write(want); err != nil {
		t.Fatalf("master write: %v", err)
	}
	got := readAtLeast(t, slave, len(want))
	if !bytes.Equal(got, want) {
		t.Fatalf("master->slave: got %q want %q", got, want)
	}

	// slave -> master
	want2 := []byte("ping")
	if _, err := slave.Write(want2); err != nil {
		t.Fatalf("slave write: %v", err)
	}
	got2 := readAtLeast(t, p.Master(), len(want2))
	if !bytes.Equal(got2, want2) {
		t.Fatalf("slave->master: got %q want %q", got2, want2)
	}
}

func TestSymlink_CreatedAndOverwritten(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "ttyBLE-test")

	p1, err := Open(link)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	target1, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target1 != p1.SlavePath() {
		t.Fatalf("symlink target=%q want %q", target1, p1.SlavePath())
	}

	// Open a second pty and overwrite the same symlink.
	p2, err := Open(link)
	if err != nil {
		_ = p1.Close()
		t.Fatalf("Open #2: %v", err)
	}
	target2, err := os.Readlink(link)
	if err != nil {
		_ = p1.Close()
		_ = p2.Close()
		t.Fatalf("readlink #2: %v", err)
	}
	if target2 != p2.SlavePath() {
		t.Fatalf("second symlink target=%q want %q", target2, p2.SlavePath())
	}

	if err := p2.Close(); err != nil {
		t.Fatalf("p2.Close: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected symlink removed, got err=%v", err)
	}
	_ = p1.Close()
}

// readAtLeast reads bytes with a deadline so the test never hangs.
func readAtLeast(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		// PTYs on macOS sometimes don't support deadlines; fall back to a
		// goroutine-based read with timeout.
		return readAtLeastGoroutine(t, f, n)
	}
	defer func() { _ = f.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:got]
}

func readAtLeastGoroutine(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	type result struct {
		buf []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(f, buf)
		ch <- result{buf, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		return r.buf
	case <-time.After(2 * time.Second):
		t.Fatalf("read timed out")
		return nil
	}
}
