package bleconn

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifyQueue_ReadAfterWrite(t *testing.T) {
	q := NewNotifyQueue()
	q.Push([]byte{'A', 'B', 'C'})
	buf := make([]byte, 3)
	n, err := q.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, []byte{'A', 'B', 'C'}) {
		t.Fatalf("buf=%v", buf)
	}
}

func TestNotifyQueue_ReadShorter(t *testing.T) {
	q := NewNotifyQueue()
	q.Push([]byte{'A', 'B', 'C', 'D'})

	buf := make([]byte, 2)
	n, err := q.Read(buf)
	if err != nil || n != 2 || !bytes.Equal(buf, []byte{'A', 'B'}) {
		t.Fatalf("first read: n=%d err=%v buf=%v", n, err, buf)
	}
	n, err = q.Read(buf)
	if err != nil || n != 2 || !bytes.Equal(buf, []byte{'C', 'D'}) {
		t.Fatalf("second read: n=%d err=%v buf=%v", n, err, buf)
	}
}

func TestNotifyQueue_ReadBlocksUntilData(t *testing.T) {
	q := NewNotifyQueue()
	var done atomic.Bool
	go func() {
		buf := make([]byte, 4)
		_, _ = q.Read(buf)
		done.Store(true)
	}()
	time.Sleep(50 * time.Millisecond)
	if done.Load() {
		t.Fatalf("Read returned before data was pushed")
	}
	q.Push([]byte("data"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Read did not return after data pushed")
}

func TestNotifyQueue_CloseUnblocksRead(t *testing.T) {
	q := NewNotifyQueue()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4)
		n, err := q.Read(buf)
		ch <- result{n, err}
	}()
	time.Sleep(20 * time.Millisecond)
	_ = q.Close()
	select {
	case r := <-ch:
		if r.n != 0 || r.err != io.EOF {
			t.Fatalf("got n=%d err=%v want 0,EOF", r.n, r.err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Read did not return after Close")
	}
}

func TestNotifyQueue_FragmentedAccrual(t *testing.T) {
	q := NewNotifyQueue()
	chunk := bytes.Repeat([]byte{0x55}, 20)
	q.Push(chunk)
	q.Push(chunk)
	q.Push(chunk)

	buf := make([]byte, 60)
	n, err := readFull(q, buf)
	if err != nil || n != 60 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{0x55}, 60)) {
		t.Fatalf("contents mismatch")
	}
}

// readFull repeatedly calls Read until len(buf) bytes are read or an error.
func readFull(q *NotifyQueue, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := q.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
