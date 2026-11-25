package main

import (
	"sync"
)

// RingBuffer is a thread-safe circular byte buffer with a fixed capacity.
type RingBuffer struct {
	mu        sync.RWMutex
	buf       []byte
	size      int
	start     int
	length    int
	isWrapped bool
}

// NewRingBuffer allocates a new circular buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1024 * 1024 // Default 1MB
	}
	return &RingBuffer{
		buf:  make([]byte, capacity),
		size: capacity,
	}
}

// Write appends bytes to the ring buffer, overwriting the oldest bytes when full.
func (r *RingBuffer) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pLen := len(p)
	if pLen == 0 {
		return 0, nil
	}

	// If payload is larger than capacity, only take the trailing slice
	if pLen >= r.size {
		copy(r.buf, p[pLen-r.size:])
		r.start = 0
		r.length = r.size
		r.isWrapped = true
		return pLen, nil
	}

	for i := 0; i < pLen; i++ {
		idx := (r.start + r.length) % r.size
		r.buf[idx] = p[i]
		if r.length < r.size {
			r.length++
		} else {
			r.start = (r.start + 1) % r.size
			r.isWrapped = true
		}
	}

	return pLen, nil
}

// Bytes returns a contiguous snapshot of all active bytes from oldest to newest.
func (r *RingBuffer) Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.length == 0 {
		return nil
	}

	out := make([]byte, r.length)
	if r.start+r.length <= r.size {
		copy(out, r.buf[r.start:r.start+r.length])
	} else {
		firstPart := r.size - r.start
		copy(out[:firstPart], r.buf[r.start:])
		copy(out[firstPart:], r.buf[:r.length-firstPart])
	}
	return out
}

// Len returns the current number of bytes stored.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.length
}

// IsWrapped reports whether the ring buffer has wrapped around.
func (r *RingBuffer) IsWrapped() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isWrapped
}
