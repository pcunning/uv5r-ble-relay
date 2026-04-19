package bleconn

import (
	"bytes"
	"errors"
	"sync"

	"github.com/pcunning/uv5r-ble-relay/internal/protocol"
)

// FakeTransport is an in-memory emulation of the UV-5R Mini BLE radio. It
// implements Transport for tests and for `--fake` mode.
//
// On each Write, accumulated bytes are parsed for known commands and the
// matching reply is enqueued in the embedded NotifyQueue (which Read drains).
// When Fragmented is true, replies are split into 20-byte chunks before being
// pushed, mimicking BLE notification fragmentation.
type FakeTransport struct {
	mu         sync.Mutex
	buf        []byte
	notify     *NotifyQueue
	closed     bool
	Fragmented bool
}

// NewFakeTransport returns a fresh fake radio.
func NewFakeTransport() *FakeTransport {
	return &FakeTransport{notify: NewNotifyQueue()}
}

// SetFragmented controls whether replies are split into 20-byte notifications.
func (f *FakeTransport) SetFragmented(v bool) {
	f.mu.Lock()
	f.Fragmented = v
	f.mu.Unlock()
}

// Read drains buffered notifications. Returns io.EOF after Close.
func (f *FakeTransport) Read(p []byte) (int, error) { return f.notify.Read(p) }

// Write feeds bytes from the host into the fake radio's parser.
func (f *FakeTransport) Write(p []byte) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, errors.New("write on closed FakeTransport")
	}
	f.buf = append(f.buf, p...)
	replies := f.parseLocked()
	f.mu.Unlock()
	for _, r := range replies {
		f.deliver(r)
	}
	return len(p), nil
}

// Close releases the queue so blocked readers see EOF.
func (f *FakeTransport) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return f.notify.Close()
}

// parseLocked extracts as many complete commands as possible from f.buf and
// returns the corresponding replies. f.mu must be held.
func (f *FakeTransport) parseLocked() [][]byte {
	var out [][]byte
	for {
		reply, consumed, ok := matchOne(f.buf)
		if !ok {
			return out
		}
		f.buf = f.buf[consumed:]
		if reply != nil {
			out = append(out, reply)
		}
	}
}

// matchOne attempts to consume a single command from the head of buf.
// Returns (reply, consumedBytes, true) if a complete command was recognized,
// or (nil, 0, false) when more data is required.
func matchOne(buf []byte) ([]byte, int, bool) {
	if len(buf) == 0 {
		return nil, 0, false
	}
	switch buf[0] {
	case 'P':
		if len(buf) < len(protocol.IdentRequest) {
			return nil, 0, false
		}
		if !bytes.Equal(buf[:len(protocol.IdentRequest)], []byte(protocol.IdentRequest)) {
			// Unknown payload starting with 'P'; drop one byte to advance.
			return nil, 1, true
		}
		return []byte{protocol.IdentAck}, len(protocol.IdentRequest), true

	case protocol.CmdF:
		out := make([]byte, len(protocol.FDescriptor))
		copy(out, protocol.FDescriptor)
		return out, 1, true

	case protocol.CmdM:
		return []byte(protocol.MModel), 1, true

	case 'S':
		const total = 5 + 20 // "SEND!" + 20 bytes
		if len(buf) < total {
			return nil, 0, false
		}
		if !bytes.Equal(buf[:5], []byte(protocol.SendPrefix)) {
			return nil, 1, true
		}
		return []byte{protocol.IdentAck}, total, true

	case protocol.OpRead:
		// 0x52 hi lo 0x40
		if len(buf) < 4 {
			return nil, 0, false
		}
		hi, lo := buf[1], buf[2]
		ln := int(buf[3])
		out := make([]byte, 4+ln)
		out[0] = protocol.OpRead
		out[1] = hi
		out[2] = lo
		out[3] = byte(ln)
		// Remaining bytes left as zero.
		return out, 4, true

	case protocol.OpWrite:
		// 0x57 hi lo 0x80 + 0x80 bytes
		if len(buf) < 4 {
			return nil, 0, false
		}
		ln := int(buf[3])
		total := 4 + ln
		if len(buf) < total {
			return nil, 0, false
		}
		return []byte{protocol.IdentAck}, total, true
	}
	// Unknown opcode — drop a byte rather than stalling forever.
	return nil, 1, true
}

// deliver pushes data to the notify queue, optionally splitting into 20-byte
// chunks so tests can exercise fragmented notifications.
func (f *FakeTransport) deliver(data []byte) {
	f.mu.Lock()
	frag := f.Fragmented
	f.mu.Unlock()
	if !frag {
		f.notify.Push(data)
		return
	}
	const chunk = 20
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		f.notify.Push(data[i:end])
	}
}
