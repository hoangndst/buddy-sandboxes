package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCopyEndpoint(t *testing.T) {
	tests := []struct {
		input    string
		expected copyEndpoint
	}{
		{
			input: "./local/file.txt",
			expected: copyEndpoint{
				isRemote: false,
				sandbox:  "",
				path:     "./local/file.txt",
			},
		},
		{
			input: "sbx-123:/workspace/file.txt",
			expected: copyEndpoint{
				isRemote: true,
				sandbox:  "sbx-123",
				path:     "/workspace/file.txt",
			},
		},
		{
			input: "sbx-abc:relative/path.txt",
			expected: copyEndpoint{
				isRemote: true,
				sandbox:  "sbx-abc",
				path:     "/workspace/relative/path.txt",
			},
		},
		{
			input: "sbx-xyz:",
			expected: copyEndpoint{
				isRemote: true,
				sandbox:  "sbx-xyz",
				path:     "/workspace",
			},
		},
	}

	for _, tt := range tests {
		got := parseCopyEndpoint(tt.input)
		if got != tt.expected {
			t.Errorf("parseCopyEndpoint(%q) = %+v, want %+v", tt.input, got, tt.expected)
		}
	}
}

func TestTarPackAndUnpackRoundTrip(t *testing.T) {
	tempSrc := t.TempDir()
	tempDst := t.TempDir()

	// Create test file structure in tempSrc
	subDir := filepath.Join(tempSrc, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	file1 := filepath.Join(tempSrc, "hello.txt")
	if err := os.WriteFile(file1, []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	file2 := filepath.Join(subDir, "nested.txt")
	if err := os.WriteFile(file2, []byte("nested content\n"), 0644); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	// Pack tempSrc into tar buffer
	var buf bytes.Buffer
	if err := tarPack(tempSrc, &buf); err != nil {
		t.Fatalf("tarPack failed: %v", err)
	}

	// Unpack into tempDst
	if err := tarUnpack(&buf, tempDst); err != nil {
		t.Fatalf("tarUnpack failed: %v", err)
	}

	// Verify unpacked files
	dstFile1 := filepath.Join(tempDst, filepath.Base(tempSrc), "hello.txt")
	content1, err := os.ReadFile(dstFile1)
	if err != nil {
		t.Fatalf("failed to read unpacked file1: %v", err)
	}
	if string(content1) != "hello world\n" {
		t.Errorf("content mismatch in file1: got %q, want %q", string(content1), "hello world\n")
	}

	dstFile2 := filepath.Join(tempDst, filepath.Base(tempSrc), "subdir", "nested.txt")
	content2, err := os.ReadFile(dstFile2)
	if err != nil {
		t.Fatalf("failed to read unpacked file2: %v", err)
	}
	if string(content2) != "nested content\n" {
		t.Errorf("content mismatch in file2: got %q, want %q", string(content2), "nested content\n")
	}
}

func TestTarUnpackPathTraversalDefense(t *testing.T) {
	tempDst := t.TempDir()

	// Malicious tar with ../../../traversal.txt
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	maliciousName := "../../traversal.txt"
	content := []byte("evil")
	_ = tw.WriteHeader(&tar.Header{
		Name: maliciousName,
		Mode: 0644,
		Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()

	err := tarUnpack(&buf, tempDst)
	if err == nil {
		t.Fatalf("expected error on path traversal in tarUnpack, got nil")
	}
}
