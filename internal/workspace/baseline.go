package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

func readEntry(root *os.Root, name string) (Entry, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return Entry{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Entry{}, fmt.Errorf("workspace path %q is a symlink", name)
	}
	if info.IsDir() {
		return Entry{Type: EntryDirectory, Mode: normalizeMode(info.Mode(), true)}, nil
	}
	if !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("workspace path %q is not a regular file or directory", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Entry{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return Entry{}, fmt.Errorf("workspace path %q changed while it was checked", name)
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return Entry{Type: EntryRegular, Mode: normalizeMode(before.Mode(), false), Size: n, Digest: digest}, nil
}

func matchesBaseline(root *os.Root, name string, baseline Baseline) (bool, error) {
	expected, exists := baseline[name]
	actual, err := readEntry(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return !exists, nil
	}
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	return sameEntry(expected, actual), nil
}

func sameEntry(a, b Entry) bool {
	if a.Type != b.Type || a.Mode != b.Mode {
		return false
	}
	if a.Type == EntryDirectory {
		return true
	}
	return a.Size == b.Size && a.Digest == b.Digest
}
