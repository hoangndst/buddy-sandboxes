package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadEntryAndMatchesBaseline(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "dir", "file"), []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	directory, err := readEntry(root, "dir")
	if err != nil {
		t.Fatal(err)
	}
	file, err := readEntry(root, "dir/file")
	if err != nil {
		t.Fatal(err)
	}
	baseline := Baseline{"dir": directory, "dir/file": file}
	if ok, err := matchesBaseline(root, "dir/file", baseline); err != nil || !ok {
		t.Fatalf("file baseline match = %v, %v", ok, err)
	}
	if ok, err := matchesBaseline(root, "missing", baseline); err != nil || !ok {
		t.Fatalf("absent path baseline match = %v, %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(project, "dir", "file"), []byte("modified"), 0o640); err != nil {
		t.Fatal(err)
	}
	if ok, err := matchesBaseline(root, "dir/file", baseline); err != nil || ok {
		t.Fatalf("modified file baseline match = %v, %v", ok, err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Chmod(filepath.Join(project, "dir", "file"), 0o444); err != nil {
			t.Fatal(err)
		}
		readOnly, err := readEntry(root, "dir/file")
		if err != nil || readOnly.Mode != 0o444 {
			t.Fatalf("read-only baseline mode = %o, %v", readOnly.Mode, err)
		}
		if err := os.Chmod(filepath.Join(project, "dir", "file"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, err := matchesBaseline(root, "dir/file", Baseline{"dir/file": readOnly}); err != nil || ok {
			t.Fatalf("read-only attribute toggle match = %v, %v", ok, err)
		}
	}
}
