package bleconn

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func recorder() (func([]byte) (int, error), *[][]byte) {
	var calls [][]byte
	w := func(b []byte) (int, error) {
		c := make([]byte, len(b))
		copy(c, b)
		calls = append(calls, c)
		return len(b), nil
	}
	return w, &calls
}

func TestChunker_SplitsIntoMTU(t *testing.T) {
	w, calls := recorder()
	data := bytes.Repeat([]byte{0xAB}, 132)
	if err := WriteChunked(w, data, 20, 0); err != nil {
		t.Fatal(err)
	}
	want := []int{20, 20, 20, 20, 20, 20, 12}
	if len(*calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(*calls), len(want))
	}
	for i, c := range *calls {
		if len(c) != want[i] {
			t.Fatalf("call %d len=%d want=%d", i, len(c), want[i])
		}
	}
}

func TestChunker_ExactBoundary(t *testing.T) {
	w, calls := recorder()
	if err := WriteChunked(w, bytes.Repeat([]byte{1}, 60), 20, 0); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(*calls))
	}
	for i, c := range *calls {
		if len(c) != 20 {
			t.Fatalf("call %d len=%d want 20", i, len(c))
		}
	}
}

func TestChunker_Single(t *testing.T) {
	w, calls := recorder()
	if err := WriteChunked(w, []byte{0x06}, 20, 0); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 1 {
		t.Fatalf("calls=%v", *calls)
	}
}

func TestChunker_Zero(t *testing.T) {
	w, calls := recorder()
	if err := WriteChunked(w, nil, 20, 0); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(*calls))
	}
}

func TestChunker_PropagatesError(t *testing.T) {
	myErr := errors.New("boom")
	w := func(b []byte) (int, error) { return 0, myErr }
	if err := WriteChunked(w, []byte{1, 2, 3}, 2, 0); !errors.Is(err, myErr) {
		t.Fatalf("got %v, want %v", err, myErr)
	}
}

func TestChunker_PaceSleeps(t *testing.T) {
	w, _ := recorder()
	start := time.Now()
	if err := WriteChunked(w, bytes.Repeat([]byte{1}, 4), 1, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("expected >=15ms elapsed, got %v", elapsed)
	}
}
