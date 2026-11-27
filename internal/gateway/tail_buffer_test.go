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
	// Capacity is 35. Total written is 16 bytes. Dropped is 4 bytes.
	// Prefix is "[TRUNCATED: 4 bytes]\n" (21 bytes). Avail is 14 bytes.
	// Buffer holds the last 12 bytes ("o big world!"), tail retains "big world!" (10 bytes).
	tb := newTailBuffer(35)
	_, _ = tb.Write([]byte("hello "))     // 6 bytes
	_, _ = tb.Write([]byte("big world!")) // 10 bytes -> total 16 bytes, capacity 35 (no buffer overflow yet)

	// Now force buffer overflow by creating a smaller capacity buffer
	tb = newTailBuffer(32)
	// Write 25 bytes
	_, _ = tb.Write([]byte("first line of text here\n")) // 24 bytes
	// Write 18 bytes -> total 42 bytes. Dropped is 10 bytes.
	_, _ = tb.Write([]byte("second line here\n")) // 17 bytes

	if tb.Dropped() != 9 {
		t.Fatalf("expected 9 dropped, got %d", tb.Dropped())
	}
	out := tb.String()
	if len([]byte(out)) > 32 {
		t.Fatalf("output length %d exceeded maxBytes 32", len([]byte(out)))
	}
	if !strings.HasPrefix(out, "[TRUNCATED: 9 bytes]\n") {
		t.Fatalf("expected truncation prefix, got %q", out)
	}
}

func TestTailBufferSingleWriteExceedsCapacity(t *testing.T) {
	tb := newTailBuffer(32)
	_, _ = tb.Write([]byte("123"))
	_, _ = tb.Write([]byte("ABCDEFGHIJ0123456789abcdefghij")) // 30 bytes

	// Previous 3 bytes + 1 byte overflow = 4 bytes dropped.
	// Capacity 32. Prefix "[TRUNCATED: 4 bytes]\n" = 21 bytes. Avail = 11 bytes.
	if tb.Dropped() != 1 {
		t.Fatalf("expected 1 dropped, got %d", tb.Dropped())
	}
	out := tb.String()
	if len([]byte(out)) > 32 {
		t.Fatalf("output length %d exceeded maxBytes 32", len([]byte(out)))
	}
	if !strings.HasPrefix(out, "[TRUNCATED: 1 bytes]\n") {
		t.Fatalf("expected prefix, got %q", out)
	}
}

func TestTailBufferUTF8Boundary(t *testing.T) {
	// 3-byte Chinese characters: "你好世界" (12 bytes)
	// Truncation should snap forward to the nearest rune start without producing invalid UTF-8.
	tb := newTailBuffer(40)
	_, _ = tb.Write([]byte("prefix_data_"))
	// Write multiple runes
	for i := 0; i < 20; i++ {
		_, _ = tb.Write([]byte("你好世界\n"))
	}

	out := tb.Bytes()
	if len(out) > 40 {
		t.Fatalf("output byte length %d exceeds maxBytes 40", len(out))
	}
	str := string(out)
	if !strings.HasPrefix(str, "[TRUNCATED:") {
		t.Fatalf("expected truncation marker, got %q", str)
	}
	// The tail after marker must be valid UTF-8
	body := str[strings.Index(str, "\n")+1:]
	for _, r := range body {
		if r == '\uFFFD' {
			t.Fatalf("found invalid UTF-8 replacement character in tail: %q", body)
		}
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
