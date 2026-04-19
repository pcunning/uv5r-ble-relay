package translate

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pcunning/uv5r-ble-relay/internal/bleconn"
	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
)

// countingFake wraps a FakeTransport, recording every byte written by the
// translator so tests can assert on the inner protocol.
type countingFake struct {
	inner *bleconn.FakeTransport

	mu          sync.Mutex
	writes      [][]byte // each Write call captured
	uploads128  int      // count of fully-formed 0x57 frames with ln=0x80
	uploadsSeen []writeRecord
}

type writeRecord struct {
	addr    uint16
	ln      int
	payload []byte
}

func newCountingFake() *countingFake {
	return &countingFake{inner: bleconn.NewFakeTransport()}
}

func (c *countingFake) Read(p []byte) (int, error) { return c.inner.Read(p) }

func (c *countingFake) Write(p []byte) (int, error) {
	c.mu.Lock()
	cp := append([]byte(nil), p...)
	c.writes = append(c.writes, cp)
	// Parse 0x57 frames from the byte stream.
	for i := 0; i+4 <= len(cp); {
		if cp[i] == protocol.OpWrite {
			ln := int(cp[i+3])
			if i+4+ln > len(cp) {
				break
			}
			if ln == int(protocol.WriteBlockSize) {
				c.uploads128++
				c.uploadsSeen = append(c.uploadsSeen, writeRecord{
					addr:    binary.BigEndian.Uint16(cp[i+1 : i+3]),
					ln:      ln,
					payload: append([]byte(nil), cp[i+4:i+4+ln]...),
				})
			}
			i += 4 + ln
		} else if cp[i] == protocol.OpRead {
			i += 4
		} else {
			i++
		}
	}
	c.mu.Unlock()
	return c.inner.Write(p)
}

func (c *countingFake) Close() error { return c.inner.Close() }

func (c *countingFake) inner128Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uploads128
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// readWithTimeout returns up to n bytes from r within timeout.
func readWithTimeout(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()
	out := make([]byte, n)
	done := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		k, err := io.ReadFull(r, out)
		done <- struct {
			n   int
			err error
		}{k, err}
	}()
	select {
	case x := <-done:
		if x.err != nil {
			t.Fatalf("read %d: got %d err=%v", n, x.n, x.err)
		}
		return out
	case <-time.After(timeout):
		t.Fatalf("timeout reading %d bytes", n)
		return nil
	}
}

// drainCount runs in the background, counting bytes returned by Read until
// EOF. It returns the running counter.
func drainCount(r io.Reader) (*atomic.Int64, chan struct{}) {
	var n atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			k, err := r.Read(buf)
			if k > 0 {
				n.Add(int64(k))
			}
			if err != nil {
				return
			}
		}
	}()
	return &n, done
}

func waitFor(t *testing.T, want int64, got func() int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: want %d, got %d", want, got())
}

func TestTranslator_IdentificationPassthrough(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()

	// PROGRAM
	if _, err := tr.Write([]byte(protocol.IdentRequest)); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("PROGRAM ack=0x%X", got[0])
	}
	// F
	if _, err := tr.Write([]byte{protocol.CmdF}); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, protocol.FDescriptorLen, time.Second); !bytes.Equal(got, protocol.FDescriptor) {
		t.Fatalf("F mismatch: %x", got)
	}
	// M
	if _, err := tr.Write([]byte{protocol.CmdM}); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, protocol.MModelLen, time.Second); !bytes.Equal(got, []byte(protocol.MModel)) {
		t.Fatalf("M mismatch: %q", got)
	}
	// SEND!
	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	if _, err := tr.Write(send); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("SEND ack=0x%X", got[0])
	}

	if cf.inner128Count() != 0 {
		t.Fatalf("ident phase produced %d 128-byte uploads, want 0", cf.inner128Count())
	}
}

func enterCommandPhase(t *testing.T, tr *Translator) {
	t.Helper()
	if _, err := tr.Write([]byte(protocol.IdentRequest)); err != nil {
		t.Fatal(err)
	}
	readWithTimeout(t, tr, 1, time.Second)
	tr.Write([]byte{protocol.CmdF})
	readWithTimeout(t, tr, protocol.FDescriptorLen, time.Second)
	tr.Write([]byte{protocol.CmdM})
	readWithTimeout(t, tr, protocol.MModelLen, time.Second)
	send := append([]byte(protocol.SendPrefix), bytes.Repeat([]byte{0x01}, 20)...)
	tr.Write(send)
	readWithTimeout(t, tr, 1, time.Second)
}

// uploadFrame builds a CHIRP-side 0x57 frame at addr with the given payload.
func uploadFrame(addr uint16, payload []byte) []byte {
	out := make([]byte, 0, 4+len(payload))
	out = append(out, protocol.OpWrite, byte(addr>>8), byte(addr), byte(len(payload)))
	out = append(out, payload...)
	return out
}

func TestTranslator_FullPairUpload(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	first := bytes.Repeat([]byte{0xAA}, int(protocol.ReadBlockSize))
	second := bytes.Repeat([]byte{0xBB}, int(protocol.ReadBlockSize))

	if _, err := tr.Write(uploadFrame(0x0000, first)); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("first half ack=0x%X", got[0])
	}
	if cf.inner128Count() != 0 {
		t.Fatalf("inner writes after first half: %d, want 0", cf.inner128Count())
	}

	if _, err := tr.Write(uploadFrame(0x0040, second)); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("second half ack=0x%X", got[0])
	}

	if cf.inner128Count() != 1 {
		t.Fatalf("inner 128-byte writes=%d, want 1", cf.inner128Count())
	}
	cf.mu.Lock()
	rec := cf.uploadsSeen[0]
	cf.mu.Unlock()
	if rec.addr != 0x0000 {
		t.Fatalf("paired addr=0x%04X, want 0x0000", rec.addr)
	}
	wantPayload := append(append([]byte{}, first...), second...)
	if !bytes.Equal(rec.payload, wantPayload) {
		t.Fatalf("paired payload mismatch")
	}
}

func TestTranslator_FinalShortSegment(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	payload := bytes.Repeat([]byte{0x77}, int(protocol.ReadBlockSize))
	if _, err := tr.Write(uploadFrame(0x9000, payload)); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("ack=0x%X", got[0])
	}

	if cf.inner128Count() != 1 {
		t.Fatalf("inner 128 writes=%d, want 1", cf.inner128Count())
	}
	rec := cf.uploadsSeen[0]
	if rec.addr != 0x9000 {
		t.Fatalf("addr=0x%04X want 0x9000", rec.addr)
	}
	if !bytes.Equal(rec.payload[:0x40], payload) {
		t.Fatalf("first half payload mismatch")
	}
	for i := 0x40; i < 0x80; i++ {
		if rec.payload[i] != 0xFF {
			t.Fatalf("pad byte %d = 0x%02X, want 0xFF", i, rec.payload[i])
		}
	}
}

func TestTranslator_LastBlockOfSegment1(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	payload := bytes.Repeat([]byte{0x33}, int(protocol.ReadBlockSize))
	if _, err := tr.Write(uploadFrame(0x8000, payload)); err != nil {
		t.Fatal(err)
	}
	if got := readWithTimeout(t, tr, 1, time.Second); got[0] != protocol.IdentAck {
		t.Fatalf("ack=0x%X", got[0])
	}
	if cf.inner128Count() != 1 {
		t.Fatalf("inner=%d, want 1", cf.inner128Count())
	}
	rec := cf.uploadsSeen[0]
	if rec.addr != 0x8000 {
		t.Fatalf("addr=0x%04X want 0x8000", rec.addr)
	}
	for i := 0x40; i < 0x80; i++ {
		if rec.payload[i] != 0xFF {
			t.Fatalf("pad %d=0x%02X", i, rec.payload[i])
		}
	}
}

func TestTranslator_DownloadPassthrough(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	if _, err := tr.Write([]byte{protocol.OpRead, 0x00, 0x00, byte(protocol.ReadBlockSize)}); err != nil {
		t.Fatal(err)
	}
	got := readWithTimeout(t, tr, 4+int(protocol.ReadBlockSize), time.Second)
	if got[0] != protocol.OpRead || got[3] != byte(protocol.ReadBlockSize) {
		t.Fatalf("header mismatch: %x", got[:4])
	}
}

func TestTranslator_FullUploadSequence(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	chirpAcks, _ := drainCount(tr)

	chirpBlocks := 0
	for i, start := range protocol.MemStarts {
		size := int(protocol.MemSizes[i])
		blocks := size / int(protocol.ReadBlockSize)
		payload := bytes.Repeat([]byte{0x5A}, int(protocol.ReadBlockSize))
		for j := 0; j < blocks; j++ {
			addr := uint16(uint32(start) + uint32(j)*uint32(protocol.ReadBlockSize))
			if _, err := tr.Write(uploadFrame(addr, payload)); err != nil {
				t.Fatal(err)
			}
			chirpBlocks++
		}
	}
	if chirpBlocks != 521 {
		t.Fatalf("chirpBlocks=%d want 521", chirpBlocks)
	}
	waitFor(t, 521, func() int64 { return chirpAcks.Load() }, 5*time.Second)
	if cf.inner128Count() != 262 {
		t.Fatalf("inner 128 writes=%d, want 262", cf.inner128Count())
	}
}

func TestTranslator_FullDownloadSequence(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()
	enterCommandPhase(t, tr)

	rxBytes, _ := drainCount(tr)

	totalReads := 0
	for i, start := range protocol.MemStarts {
		size := int(protocol.MemSizes[i])
		blocks := size / int(protocol.ReadBlockSize)
		for j := 0; j < blocks; j++ {
			addr := uint16(uint32(start) + uint32(j)*uint32(protocol.ReadBlockSize))
			cmd := []byte{protocol.OpRead, byte(addr >> 8), byte(addr), byte(protocol.ReadBlockSize)}
			if _, err := tr.Write(cmd); err != nil {
				t.Fatal(err)
			}
			totalReads++
		}
	}
	if totalReads != 521 {
		t.Fatalf("totalReads=%d want 521", totalReads)
	}
	expected := int64(protocol.TotalMemBytes + 4*521)
	waitFor(t, expected, func() int64 { return rxBytes.Load() }, 5*time.Second)
}

// TestTranslator_ReIdent verifies that a second PROGRAMCOLORPROU handshake
// (which CHIRP sends before every upload/download operation) is handled
// correctly after the translator is already in command phase.
func TestTranslator_ReIdent(t *testing.T) {
	cf := newCountingFake()
	tr := New(cf, quietLogger())
	defer tr.Close()

	// First ident + one 64-byte write pair (read ACKs synchronously).
	enterCommandPhase(t, tr)
	doWritePairSync(t, tr, 0x0000)

	// Second full ident — simulates CHIRP starting a second operation.
	enterCommandPhase(t, tr)

	// Should still work: issue a download read in the new command phase.
	addr := uint16(0x0000)
	cmd := []byte{protocol.OpRead, byte(addr >> 8), byte(addr), byte(protocol.ReadBlockSize)}
	if _, err := tr.Write(cmd); err != nil {
		t.Fatalf("read after re-ident: %v", err)
	}
	got := readWithTimeout(t, tr, 4+int(protocol.ReadBlockSize), time.Second)
	if len(got) != 4+int(protocol.ReadBlockSize) {
		t.Fatalf("expected %d bytes, got %d", 4+int(protocol.ReadBlockSize), len(got))
	}
}

// doWritePairSync sends two consecutive 64-byte uploads forming one 128-byte
// pair and reads each ACK synchronously (no background goroutine).
func doWritePairSync(t *testing.T, tr *Translator, base uint16) {
	t.Helper()
	for i := 0; i < 2; i++ {
		addr := base + uint16(i)*uint16(protocol.ReadBlockSize)
		frame := make([]byte, 4+int(protocol.ReadBlockSize))
		frame[0] = protocol.OpWrite
		frame[1] = byte(addr >> 8)
		frame[2] = byte(addr)
		frame[3] = byte(protocol.ReadBlockSize)
		if _, err := tr.Write(frame); err != nil {
			t.Fatalf("write pair[%d]: %v", i, err)
		}
		got := readWithTimeout(t, tr, 1, time.Second)
		if got[0] != protocol.IdentAck {
			t.Fatalf("write pair[%d]: ack=0x%02X, want 0x06", i, got[0])
		}
	}
}
