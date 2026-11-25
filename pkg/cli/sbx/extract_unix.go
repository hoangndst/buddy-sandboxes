//go:build !windows

package sbx

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func writeArchiveEntry(root, rel string, header *tar.Header, data io.Reader) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootFD) }()

	parts := splitPath(rel)
	if len(parts) == 0 {
		return fmt.Errorf("empty archive path")
	}
	parentFD := rootFD
	opened := []int{}
	defer func() {
		for _, fd := range opened {
			_ = unix.Close(fd)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if err := unix.Mkdirat(parentFD, part, 0755); err != nil && err != unix.EEXIST {
			return err
		}
		fd, err := unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("unsafe archive directory %q: %w", part, err)
		}
		opened = append(opened, fd)
		parentFD = fd
	}

	name := parts[len(parts)-1]
	if header.Typeflag == tar.TypeDir {
		if err := unix.Mkdirat(parentFD, name, 0755); err != nil && err != unix.EEXIST {
			return err
		}
		fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("unsafe archive directory %q: %w", name, err)
		}
		return unix.Close(fd)
	}

	flags := unix.O_CREAT | unix.O_WRONLY | unix.O_TRUNC | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(parentFD, name, flags, uint32(header.FileInfo().Mode().Perm()))
	if err != nil {
		return fmt.Errorf("unsafe archive file %q: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := io.Copy(f, data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func splitPath(rel string) []string {
	clean := filepath.Clean(rel)
	if clean == "." || filepath.IsAbs(clean) {
		return nil
	}
	return strings.Split(clean, string(filepath.Separator))
}
