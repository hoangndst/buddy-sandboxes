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
		{
			input:    `C:\Users\dev\artifact.zip`,
			expected: copyEndpoint{path: `C:\Users\dev\artifact.zip`},
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

func TestTarUnpackRejectsSymlinkTraversal(t *testing.T) {
	dst := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("should not escape")
	if err := tw.WriteHeader(&tar.Header{Name: "escape/overwritten", Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarUnpack(&buf, dst); err == nil {
		t.Fatal("expected extraction through an existing symlink to fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "overwritten")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside destination, stat error: %v", err)
	}
}

func TestTarUnpackRejectsArchiveSymlink(t *testing.T) {
	dst := t.TempDir()
	outside := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside}); err != nil {
		t.Fatal(err)
	}
	content := []byte("should not escape")
	if err := tw.WriteHeader(&tar.Header{Name: "escape/overwritten", Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarUnpack(&buf, dst); err == nil {
		t.Fatal("expected archive symlink entry to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "overwritten")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside destination, stat error: %v", err)
	}
}

func TestInstallDownloadedItemUsesFileDestination(t *testing.T) {
	stage := t.TempDir()
	dstDir := t.TempDir()
	entry := filepath.Join(stage, "dist.tar.gz")
	if err := os.WriteFile(entry, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dstDir, "dist.tar.gz")
	if err := installDownloadedItem(stage, dst, "dist.tar.gz", false); err != nil {
		t.Fatalf("installDownloadedItem failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "artifact" {
		t.Fatalf("got %q, want artifact", got)
	}
}

func TestTarPackAsUsesRequestedName(t *testing.T) {
	src := filepath.Join(t.TempDir(), "original.json")
	if err := os.WriteFile(src, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tarPackAs(src, "renamed.json", &buf); err != nil {
		t.Fatalf("tarPackAs failed: %v", err)
	}
	tr := tar.NewReader(&buf)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "renamed.json" {
		t.Fatalf("archive entry name = %q, want renamed.json", header.Name)
	}
}

func TestMoveExtractedContentsMergesExistingDirectories(t *testing.T) {
	stage := t.TempDir()
	dst := t.TempDir()
	if err := os.Mkdir(filepath.Join(stage, "project"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dst, "project"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "project", "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "project", "existing.txt"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := moveExtractedContents(stage, dst); err != nil {
		t.Fatalf("moveExtractedContents failed: %v", err)
	}
	for name, want := range map[string]string{"existing.txt": "existing", "new.txt": "new"} {
		got, err := os.ReadFile(filepath.Join(dst, "project", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
