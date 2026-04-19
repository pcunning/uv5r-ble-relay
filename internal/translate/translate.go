// Package translate adapts mainline CHIRP's UV-5R Mini protocol (which writes
// 64-byte upload blocks) to the BLE-attached radio (which only accepts
// 128-byte upload blocks).
//
// The translator is inserted between the relay's PTY pump and the BLE
// transport. It implements the Transport interface (io.ReadWriteCloser).
//
//	CHIRP --64B writes--> Translator --128B writes--> Radio (BLE)
//	                    <--ACK/data--             <--ACK/data--
//
// Identification handshake and downloads (0x52 reads) are passed through
// unchanged — only uploads (0x57) are re-paired.
package translate

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/pcunning/uv5r-ble-relay/internal/bleconn"
	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
)

// Translator wraps an inner Transport, presenting CHIRP-facing 64-byte upload
// semantics while emitting 128-byte uploads to the radio.
type Translator struct {
	inner  io.ReadWriteCloser
	logger *slog.Logger

	// chirpRx is the CHIRP-facing read queue. Anything pushed here is
	// returned by Read.
	chirpRx *bleconn.NotifyQueue

	// All state below is owned by the Write goroutine; CHIRP must serialize
	// its calls to Write (the relay's pump does), so a single mutex protects
	// concurrent access from Close.
	mu sync.Mutex

	// closed is set by Close and short-circuits further writes.
	closed bool

	// inBuf accumulates partial CHIRP commands across Write calls.
	inBuf []byte

	// phase tracks where we are in the protocol.
	phase phase

	// cachedHalf, if non-nil, holds the first 64 bytes of an in-progress
	// 128-byte upload pair, written to the radio at cachedAddr.
	cachedHalf []byte
	cachedAddr uint16
}

type phase int

const (
	phaseIdentProgram phase = iota // expecting "PROGRAMCOLORPROU"
	phaseIdentF                    // expecting 'F'
	phaseIdentM                    // expecting 'M'
	phaseIdentSend                 // expecting "SEND!" + 20 bytes
	phaseCommand                   // expecting 0x52 / 0x57 commands
)

// New constructs a Translator. The returned value owns inner: closing the
// translator closes inner.
func New(inner io.ReadWriteCloser, logger *slog.Logger) *Translator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Translator{
		inner:   inner,
		logger:  logger,
		chirpRx: bleconn.NewNotifyQueue(),
		phase:   phaseIdentProgram,
	}
}

// Read returns bytes destined for CHIRP (handshake replies, download payloads,
// synthesized or forwarded ACKs).
func (t *Translator) Read(p []byte) (int, error) {
	return t.chirpRx.Read(p)
}

// Write consumes bytes from CHIRP and drives the underlying transport. It
// blocks until every parseable command in p has been processed.
//
// On any I/O error against the inner transport, Write closes the CHIRP-facing
// queue (so Read returns EOF) and returns the error. Subsequent writes return
// the same error.
func (t *Translator) Write(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, errors.New("write on closed Translator")
	}
	t.logger.Debug("translate: CHIRP→relay write",
		"len", len(p), "phase", phaseName(t.phase),
		"hex", hexSnippet(p))
	t.inBuf = append(t.inBuf, p...)
	err := t.processLocked()
	t.mu.Unlock()
	if err != nil {
		_ = t.chirpRx.Close()
		return 0, err
	}
	return len(p), nil
}

// Close shuts the translator down: closes the inner transport and unblocks
// any blocked CHIRP-side Read with EOF.
func (t *Translator) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	_ = t.chirpRx.Close()
	return t.inner.Close()
}

// processLocked drains as many complete commands as possible from t.inBuf.
// Must be called with t.mu held.
func (t *Translator) processLocked() error {
	for {
		consumed, done, err := t.stepLocked()
		if err != nil {
			return err
		}
		if !done {
			return nil // need more bytes
		}
		t.inBuf = t.inBuf[consumed:]
		if len(t.inBuf) == 0 {
			return nil
		}
	}
}

// stepLocked attempts to handle one CHIRP-side command. Returns (consumed,
// true, nil) when one was processed, (0, false, nil) when more bytes are
// needed, or (_, _, err) on I/O failure.
func (t *Translator) stepLocked() (int, bool, error) {
	buf := t.inBuf
	if len(buf) == 0 {
		return 0, false, nil
	}

	switch t.phase {
	case phaseIdentProgram:
		need := len(protocol.IdentRequest)
		if len(buf) < need {
			t.logger.Debug("translate: waiting for more bytes",
				"phase", "PROGRAM", "have", len(buf), "need", need)
			return 0, false, nil
		}
		t.logger.Debug("translate: → PROGRAM ident", "hex", hexSnippet(buf[:need]))
		if err := t.passThroughCommand(buf[:need], 1); err != nil {
			return 0, false, err
		}
		t.logger.Debug("translate: ← PROGRAM ack")
		t.phase = phaseIdentF
		return need, true, nil

	case phaseIdentF:
		t.logger.Debug("translate: → F")
		if err := t.passThroughCommand(buf[:1], protocol.FDescriptorLen); err != nil {
			return 0, false, err
		}
		t.logger.Debug("translate: ← F descriptor")
		t.phase = phaseIdentM
		return 1, true, nil

	case phaseIdentM:
		t.logger.Debug("translate: → M")
		if err := t.passThroughCommand(buf[:1], protocol.MModelLen); err != nil {
			return 0, false, err
		}
		t.logger.Debug("translate: ← M model")
		t.phase = phaseIdentSend
		return 1, true, nil

	case phaseIdentSend:
		const need = len(protocol.SendPrefix) + 20
		if len(buf) < need {
			t.logger.Debug("translate: waiting for more bytes",
				"phase", "SEND", "have", len(buf), "need", need)
			return 0, false, nil
		}
		t.logger.Debug("translate: → SEND!", "hex", hexSnippet(buf[:need]))
		if err := t.passThroughCommand(buf[:need], 1); err != nil {
			return 0, false, err
		}
		t.logger.Debug("translate: ← SEND ack — entering command phase")
		t.phase = phaseCommand
		return need, true, nil

	case phaseCommand:
		return t.stepCommandLocked()
	}
	return 0, false, fmt.Errorf("translate: unknown phase %d", t.phase)
}

// passThroughCommand writes cmd to the inner transport, then reads exactly
// replyLen bytes back and forwards them to the CHIRP queue.
func (t *Translator) passThroughCommand(cmd []byte, replyLen int) error {
	t.logger.Debug("translate: inner write", "len", len(cmd), "hex", hexSnippet(cmd))
	if _, err := t.inner.Write(cmd); err != nil {
		t.logger.Error("translate: inner write failed", "err", err)
		return fmt.Errorf("inner write: %w", err)
	}
	t.logger.Debug("translate: waiting for inner reply", "wantLen", replyLen)
	reply, err := readFull(t.inner, replyLen)
	if err != nil {
		t.logger.Error("translate: inner read failed", "wantLen", replyLen, "err", err)
		return fmt.Errorf("inner read: %w", err)
	}
	t.logger.Debug("translate: inner reply received", "len", len(reply), "hex", hexSnippet(reply))
	t.chirpRx.Push(reply)
	return nil
}

// stepCommandLocked handles a single command in command phase.
func (t *Translator) stepCommandLocked() (int, bool, error) {
	buf := t.inBuf
	switch buf[0] {
	case protocol.IdentRequest[0]: // 0x50 'P' — CHIRP re-sends the full ident for each operation
		// Reset to ident phase so processLocked picks it up on the next
		// iteration. Return done=false so we don't advance inBuf yet.
		t.logger.Debug("translate: re-ident detected, resetting phase")
		t.cachedHalf = nil
		t.phase = phaseIdentProgram
		return 0, true, nil // return done=true so loop continues, consumed=0
	case protocol.OpRead:
		// 0x52 hi lo 0x40 — pass through unchanged.
		if len(buf) < 4 {
			return 0, false, nil
		}
		ln := int(buf[3])
		addr := uint16(buf[1])<<8 | uint16(buf[2])
		t.logger.Debug("translate: READ",
			"addr", fmt.Sprintf("0x%04X", addr), "len", ln)
		// Reply is 4-byte echo + ln bytes.
		if err := t.passThroughCommand(buf[:4], 4+ln); err != nil {
			return 0, false, err
		}
		return 4, true, nil

	case protocol.OpWrite:
		// 0x57 hi lo LEN [LEN bytes]
		if len(buf) < 4 {
			return 0, false, nil
		}
		ln := int(buf[3])
		total := 4 + ln
		if len(buf) < total {
			return 0, false, nil
		}
		consumed, err := t.handleUploadLocked(buf[:total])
		if err != nil {
			return 0, false, err
		}
		return consumed, true, nil

	default:
		// Unknown opcode in command phase: log and pass the lone byte through.
		t.logger.Warn("translate: unknown command byte, passing through",
			"byte", fmt.Sprintf("0x%02X", buf[0]))
		if _, err := t.inner.Write(buf[:1]); err != nil {
			return 0, false, fmt.Errorf("inner write: %w", err)
		}
		return 1, true, nil
	}
}

// handleUploadLocked processes one CHIRP 0x57 frame.
func (t *Translator) handleUploadLocked(frame []byte) (int, error) {
	addr := binary.BigEndian.Uint16(frame[1:3])
	ln := int(frame[3])
	payload := frame[4:]

	// CHIRP mainline only ever sends 0x40-byte writes. Anything else: pass
	// through unchanged (and flush any cached half first).
	if ln != int(protocol.ReadBlockSize) {
		t.logger.Warn("translate: non-64-byte upload, passing through",
			"addr", fmt.Sprintf("0x%04X", addr), "len", ln)
		if err := t.flushCachedHalfLocked(); err != nil {
			return 0, err
		}
		if err := t.writeAndAckLocked(frame); err != nil {
			return 0, err
		}
		return len(frame), nil
	}

	seg, ok := protocol.SegmentOf(addr)
	if !ok {
		t.logger.Warn("translate: upload address outside known segments",
			"addr", fmt.Sprintf("0x%04X", addr))
		if err := t.flushCachedHalfLocked(); err != nil {
			return 0, err
		}
		if err := t.writeAndAckLocked(frame); err != nil {
			return 0, err
		}
		return len(frame), nil
	}

	segEnd := uint32(seg.Start) + uint32(seg.Size)
	aligned128 := uint32(addr)%uint32(protocol.WriteBlockSize) == 0
	pairFits := uint32(addr)+uint32(protocol.WriteBlockSize) <= segEnd
	isFirstHalf := aligned128 && pairFits
	isSecondHalf := (uint32(addr) % uint32(protocol.WriteBlockSize)) == uint32(protocol.ReadBlockSize)

	if isFirstHalf {
		// If a stale half is already cached, flush it (defensive — mainline
		// CHIRP shouldn't ever interleave).
		if t.cachedHalf != nil {
			t.logger.Warn("translate: stale cached half flushed",
				"oldAddr", fmt.Sprintf("0x%04X", t.cachedAddr),
				"newAddr", fmt.Sprintf("0x%04X", addr))
			if err := t.flushCachedHalfLocked(); err != nil {
				return 0, err
			}
		}
		t.logger.Debug("translate: WRITE first-half cached, synthesizing ACK",
			"addr", fmt.Sprintf("0x%04X", addr))
		// Cache; synthesize ACK.
		t.cachedHalf = append([]byte(nil), payload...)
		t.cachedAddr = addr
		t.chirpRx.Push([]byte{protocol.IdentAck})
		return len(frame), nil
	}

	if isSecondHalf && t.cachedHalf != nil && uint32(t.cachedAddr)+uint32(protocol.ReadBlockSize) == uint32(addr) {
		t.logger.Debug("translate: WRITE second-half — combining and forwarding 128B",
			"pairAddr", fmt.Sprintf("0x%04X", t.cachedAddr))
		// Combine and emit one 128-byte upload at cachedAddr.
		full := make([]byte, 0, 4+int(protocol.WriteBlockSize))
		full = append(full,
			protocol.OpWrite,
			byte(t.cachedAddr>>8), byte(t.cachedAddr),
			byte(protocol.WriteBlockSize),
		)
		full = append(full, t.cachedHalf...)
		full = append(full, payload...)
		t.cachedHalf = nil
		t.cachedAddr = 0
		if err := t.writeAndAckLocked(full); err != nil {
			return 0, err
		}
		return len(frame), nil
	}

	t.logger.Debug("translate: WRITE final/odd block — padding to 128B",
		"addr", fmt.Sprintf("0x%04X", addr),
		"aligned128", aligned128, "pairFits", pairFits,
		"isSecondHalf", isSecondHalf, "hasCached", t.cachedHalf != nil)
	// Final / odd block — pad to 128 bytes with 0xFF and emit.
	if t.cachedHalf != nil {
		t.logger.Warn("translate: cached half flushed before final block",
			"oldAddr", fmt.Sprintf("0x%04X", t.cachedAddr),
			"newAddr", fmt.Sprintf("0x%04X", addr))
		if err := t.flushCachedHalfLocked(); err != nil {
			return 0, err
		}
	}
	padded := make([]byte, 0, 4+int(protocol.WriteBlockSize))
	padded = append(padded,
		protocol.OpWrite,
		byte(addr>>8), byte(addr),
		byte(protocol.WriteBlockSize),
	)
	padded = append(padded, payload...)
	pad := int(protocol.WriteBlockSize) - int(protocol.ReadBlockSize)
	for i := 0; i < pad; i++ {
		padded = append(padded, 0xFF)
	}
	if err := t.writeAndAckLocked(padded); err != nil {
		return 0, err
	}
	return len(frame), nil
}

// flushCachedHalfLocked emits any cached first-half as a padded 128-byte
// upload. Used when the protocol is in an unexpected state.
func (t *Translator) flushCachedHalfLocked() error {
	if t.cachedHalf == nil {
		return nil
	}
	addr := t.cachedAddr
	half := t.cachedHalf
	t.cachedHalf = nil
	t.cachedAddr = 0
	padded := make([]byte, 0, 4+int(protocol.WriteBlockSize))
	padded = append(padded,
		protocol.OpWrite,
		byte(addr>>8), byte(addr),
		byte(protocol.WriteBlockSize),
	)
	padded = append(padded, half...)
	pad := int(protocol.WriteBlockSize) - int(protocol.ReadBlockSize)
	for i := 0; i < pad; i++ {
		padded = append(padded, 0xFF)
	}
	return t.writeAndAckLocked(padded)
}

// writeAndAckLocked emits one 128-byte upload and forwards the radio's 1-byte
// ACK back to CHIRP.
func (t *Translator) writeAndAckLocked(frame []byte) error {
	addr := uint16(frame[1])<<8 | uint16(frame[2])
	t.logger.Debug("translate: → radio 128B WRITE",
		"addr", fmt.Sprintf("0x%04X", addr), "frameLen", len(frame))
	if _, err := t.inner.Write(frame); err != nil {
		t.logger.Error("translate: 128B write failed", "addr", fmt.Sprintf("0x%04X", addr), "err", err)
		return fmt.Errorf("inner write: %w", err)
	}
	t.logger.Debug("translate: waiting for 128B write ACK", "addr", fmt.Sprintf("0x%04X", addr))
	ack, err := readFull(t.inner, 1)
	if err != nil {
		t.logger.Error("translate: 128B ack read failed", "addr", fmt.Sprintf("0x%04X", addr), "err", err)
		return fmt.Errorf("inner read ack: %w", err)
	}
	t.logger.Debug("translate: ← 128B ACK",
		"addr", fmt.Sprintf("0x%04X", addr), "ack", fmt.Sprintf("0x%02X", ack[0]))
	if ack[0] != protocol.IdentAck {
		t.logger.Warn("translate: unexpected ACK byte from radio",
			"addr", fmt.Sprintf("0x%04X", addr),
			"got", fmt.Sprintf("0x%02X", ack[0]), "want", fmt.Sprintf("0x%02X", protocol.IdentAck))
	}
	t.chirpRx.Push(ack)
	return nil
}

// phaseName returns a human-readable name for a phase value.
func phaseName(p phase) string {
	switch p {
	case phaseIdentProgram:
		return "PROGRAM"
	case phaseIdentF:
		return "F"
	case phaseIdentM:
		return "M"
	case phaseIdentSend:
		return "SEND"
	case phaseCommand:
		return "COMMAND"
	default:
		return fmt.Sprintf("unknown(%d)", p)
	}
}

// hexSnippet returns up to 32 bytes as hex, with a truncation indicator.
func hexSnippet(b []byte) string {
	const max = 32
	if len(b) <= max {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:max]) + fmt.Sprintf("…(+%d)", len(b)-max)
}

// readFull reads exactly n bytes from r, accumulating across short reads.
func readFull(r io.Reader, n int) ([]byte, error) {
	out := make([]byte, n)
	got := 0
	for got < n {
		k, err := r.Read(out[got:])
		if k > 0 {
			got += k
		}
		if err != nil {
			if got == n {
				return out, nil
			}
			return nil, err
		}
	}
	return out, nil
}
