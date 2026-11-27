package gateway

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestTailBufferUnderCapacity(t *testing.T) {
	tb := newTailBuffer(16)
	n, err := tb.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 bytes written, got %d", n)
	}
	if tb.Dropped() != 0 {
		t.Fatalf("expected 0 dropped, got %d", tb.Dropped())
	}
	if tb.String() != "hello world" {
		t.Fatalf("expected 'hello world', got %q", tb.String())
	}
}

func TestTailBufferOverflow(t *testing.T) {
	tb := newTailBuffer(10)
	_, _ = tb.Write([]byte("hello "))     // 6 bytes
	_, _ = tb.Write([]byte("big world!")) // 10 bytes -> total 16 bytes

	// Capacity is 10. Dropped should be 6.
	// Buffer should hold the last 10 bytes: "big world!"
	if tb.Dropped() != 6 {
		t.Fatalf("expected 6 dropped, got %d", tb.Dropped())
	}
	expected := "[TRUNCATED: 6 bytes]\nbig world!"
	if tb.String() != expected {
		t.Fatalf("expected %q, got %q", expected, tb.String())
	}
}

func TestTailBufferSingleWriteExceedsCapacity(t *testing.T) {
	tb := newTailBuffer(5)
	_, _ = tb.Write([]byte("123"))
	_, _ = tb.Write([]byte("ABCDEFGHIJ")) // 10 bytes (>= 5)

	// Previous 3 bytes dropped, plus first 5 bytes of second write dropped = 8 bytes dropped.
	// Remaining 5 bytes: "FGHIJ"
	if tb.Dropped() != 8 {
		t.Fatalf("expected 8 dropped, got %d", tb.Dropped())
	}
	expected := "[TRUNCATED: 8 bytes]\nFGHIJ"
	if tb.String() != expected {
		t.Fatalf("expected %q, got %q", expected, tb.String())
	}
}

func TestTailBufferConcurrentWrites(t *testing.T) {
	tb := newTailBuffer(64)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = tb.Write([]byte(fmt.Sprintf("worker-%d-%d\n", id, j)))
			}
		}(i)
	}
	wg.Wait()

	if tb.Len() > 64 {
		t.Fatalf("buffer exceeded max capacity: %d", tb.Len())
	}
	out := tb.String()
	if !strings.HasPrefix(out, "[TRUNCATED:") && tb.Dropped() > 0 {
		t.Fatalf("expected truncation prefix when dropped > 0, got %q", out)
	}
}
