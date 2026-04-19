package bleconn

import (
	"io"
	"sync"
)

// NotifyQueue is a thread-safe byte buffer that backs Transport.Read for the
// real BLE implementation. The notification handler calls Push concurrently
// with Read; Read blocks until data is available or the queue is closed.
type NotifyQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

// NewNotifyQueue returns a fresh, empty queue.
func NewNotifyQueue() *NotifyQueue {
	q := &NotifyQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push appends b to the queue and wakes any waiting reader. The bytes are
// copied so the caller may reuse its slice immediately.
func (q *NotifyQueue) Push(b []byte) {
	if len(b) == 0 {
		return
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.buf = append(q.buf, b...)
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Read drains up to len(p) bytes, blocking until at least one byte is
// available or the queue is closed (in which case it returns 0, io.EOF).
func (q *NotifyQueue) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	for len(q.buf) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.buf) == 0 && q.closed {
		q.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, q.buf)
	q.buf = q.buf[n:]
	q.mu.Unlock()
	return n, nil
}

// Close marks the queue closed; future and currently blocked Reads observe
// EOF once any buffered data has been drained.
func (q *NotifyQueue) Close() error {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
	return nil
}
