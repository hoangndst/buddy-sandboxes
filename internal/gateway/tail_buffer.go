package gateway

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

// tailBuffer is a thread-safe circular buffer that retains the last maxBytes written to it.
// When total bytes written exceeds maxBytes, earlier bytes are discarded and
// String() / Bytes() prepends a truncation indicator with the count of discarded bytes.
// The total byte length of the returned output never exceeds maxBytes, and the tail
// content is snapped forward to the next valid UTF-8 rune boundary.
type tailBuffer struct {
	mu       sync.Mutex
	maxBytes int
	buf      []byte
	dropped  int64
}

func newTailBuffer(maxBytes int) *tailBuffer {
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	return &tailBuffer{
		maxBytes: maxBytes,
		buf:      make([]byte, 0, min(maxBytes, 4096)),
	}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := len(p)
	if n == 0 {
		return 0, nil
	}

	if n >= t.maxBytes {
		t.dropped += int64(len(t.buf)) + int64(n-t.maxBytes)
		t.buf = append(t.buf[:0], p[n-t.maxBytes:]...)
		return n, nil
	}

	available := t.maxBytes - len(t.buf)
	if n <= available {
		t.buf = append(t.buf, p...)
		return n, nil
	}

	overflow := n - available
	t.dropped += int64(overflow)
	copy(t.buf, t.buf[overflow:])
	t.buf = append(t.buf[:len(t.buf)-overflow], p...)
	return n, nil
}

func (t *tailBuffer) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}

func (t *tailBuffer) Dropped() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.dropped == 0 {
		out := make([]byte, len(t.buf))
		copy(out, t.buf)
		return out
	}

	prefix := fmt.Sprintf("[TRUNCATED: %d bytes]\n", t.dropped)
	avail := t.maxBytes - len(prefix)
	if avail <= 0 {
		return []byte(prefix[:min(len(prefix), t.maxBytes)])
	}

	tail := t.buf
	if len(tail) > avail {
		tail = tail[len(tail)-avail:]
	}
	// Snap forward to the next valid rune start so we never produce partial multibyte sequences.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}

	out := make([]byte, 0, len(prefix)+len(tail))
	out = append(out, prefix...)
	out = append(out, tail...)
	return out
}

func (t *tailBuffer) String() string {
	return string(t.Bytes())
}
