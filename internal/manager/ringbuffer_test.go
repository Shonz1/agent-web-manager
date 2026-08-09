package manager

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingBufferUnderCapacity(t *testing.T) {
	r := newRingBuffer(10)
	r.Write([]byte("abc"))
	r.Write([]byte("de"))
	if got := string(r.Bytes()); got != "abcde" {
		t.Fatalf("got %q, want %q", got, "abcde")
	}
}

func TestRingBufferWrapsKeepingTail(t *testing.T) {
	r := newRingBuffer(5)
	r.Write([]byte("abcdefgh"))
	if got := string(r.Bytes()); got != "defgh" {
		t.Fatalf("got %q, want %q", got, "defgh")
	}

	r.Write([]byte("ij"))
	if got := string(r.Bytes()); got != "fghij" {
		t.Fatalf("after second write got %q, want %q", got, "fghij")
	}
}

func TestRingBufferSingleOversizedWrite(t *testing.T) {
	r := newRingBuffer(4)
	r.Write([]byte("0123456789"))
	if got := string(r.Bytes()); got != "6789" {
		t.Fatalf("got %q, want %q", got, "6789")
	}
}

func TestRingBufferManySmallWritesStayBounded(t *testing.T) {
	const size = 64
	r := newRingBuffer(size)
	for i := 0; i < 1000; i++ {
		r.Write([]byte("x"))
	}
	got := r.Bytes()
	if len(got) != size {
		t.Fatalf("len = %d, want %d", len(got), size)
	}
	if !bytes.Equal(got, []byte(strings.Repeat("x", size))) {
		t.Fatalf("unexpected contents %q", got)
	}
}
