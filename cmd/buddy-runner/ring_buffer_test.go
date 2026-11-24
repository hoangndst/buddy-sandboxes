package main

import (
	"bytes"
	"testing"
)

func TestRingBufferBasic(t *testing.T) {
	rb := NewRingBuffer(10)
	if rb.Len() != 0 {
		t.Fatalf("expected empty buffer")
	}

	n, err := rb.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write failed: n=%d, err=%v", n, err)
	}
	if rb.Len() != 5 {
		t.Fatalf("expected len 5, got %d", rb.Len())
	}
	if !bytes.Equal(rb.Bytes(), []byte("hello")) {
		t.Fatalf("expected 'hello', got %q", string(rb.Bytes()))
	}
	if rb.IsWrapped() {
		t.Fatalf("buffer should not be wrapped yet")
	}

	// Write more to fill exactly
	_, _ = rb.Write([]byte("12345"))
	if rb.Len() != 10 {
		t.Fatalf("expected len 10, got %d", rb.Len())
	}
	if !bytes.Equal(rb.Bytes(), []byte("hello12345")) {
		t.Fatalf("expected 'hello12345', got %q", string(rb.Bytes()))
	}

	// Write overflow
	_, _ = rb.Write([]byte("XYZ"))
	if rb.Len() != 10 {
		t.Fatalf("expected len 10 after wrap, got %d", rb.Len())
	}
	if !rb.IsWrapped() {
		t.Fatalf("expected wrapped flag")
	}
	if !bytes.Equal(rb.Bytes(), []byte("lo12345XYZ")) {
		t.Fatalf("expected 'lo12345XYZ', got %q", string(rb.Bytes()))
	}
}

func TestRingBufferOversizedWrite(t *testing.T) {
	rb := NewRingBuffer(5)
	_, _ = rb.Write([]byte("0123456789"))
	if rb.Len() != 5 {
		t.Fatalf("expected len 5, got %d", rb.Len())
	}
	if !bytes.Equal(rb.Bytes(), []byte("56789")) {
		t.Fatalf("expected trailing '56789', got %q", string(rb.Bytes()))
	}
	if !rb.IsWrapped() {
		t.Fatalf("expected wrapped flag")
	}
}
