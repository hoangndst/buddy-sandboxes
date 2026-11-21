//go:build windows

package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func writeArchiveEntry(root, rel string, header *tar.Header, data io.Reader) error {
	target := filepath.Join(root, rel)
	parent, err := filepath.Rel(root, filepath.Dir(target))
	if err != nil || parent == ".." || strings.HasPrefix(parent, ".."+string(filepath.Separator)) {
		return fmt.Errorf("insecure archive target path: %s", target)
	}
	current := root
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive path traverses symlink: %s", current)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(current, 0755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if header.Typeflag == tar.TypeDir {
		if err := os.Mkdir(target, 0755); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(target)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe archive directory: %s", target)
		}
		return nil
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive path targets symlink: %s", target)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, header.FileInfo().Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
