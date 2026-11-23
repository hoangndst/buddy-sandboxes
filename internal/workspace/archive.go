package workspace

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"time"
)

// Pack writes a bounded, root-relative tar snapshot of sourceDir and returns
// the baseline captured from the same file reads.
func Pack(ctx context.Context, sourceDir string, dst io.Writer, limits Limits) (Baseline, Stats, error) {
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	if limits.TransferTimeout <= 0 {
		limits.TransferTimeout = DefaultLimits().TransferTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limits.TransferTimeout)
	defer cancel()

	unlock, err := LockProject(ctx, sourceDir)
	if err != nil {
		return nil, Stats{}, err
	}
	defer func() { _ = unlock() }()

	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, Stats{}, fmt.Errorf("open project root: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return nil, Stats{}, fmt.Errorf("stat project root: %w", err)
	}
	if !info.IsDir() {
		return nil, Stats{}, fmt.Errorf("project path must be a directory")
	}

	tw := tar.NewWriter(dst)
	baseline := make(Baseline)
	stats := Stats{}
	engine := NewIgnoreEngine(root)
	err = packDirectory(ctx, root, tw, ".", baseline, &stats, limits, engine)
	closeErr := tw.Close()
	if err != nil {
		return nil, stats, err
	}
	if closeErr != nil {
		return nil, stats, closeErr
	}
	sort.Strings(stats.ExcludedPath)
	return baseline, stats, nil
}

func packDirectory(ctx context.Context, root *os.Root, tw *tar.Writer, parent string, baseline Baseline, stats *Stats, limits Limits, engine *IgnoreEngine) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parentInfo, err := root.Lstat(parent)
	if err != nil {
		return err
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("project path %q changed to a non-directory during snapshot", parent)
	}
	dir, err := root.Open(parent)
	if err != nil {
		return err
	}
	openedInfo, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return err
	}
	if !openedInfo.IsDir() || !os.SameFile(parentInfo, openedInfo) {
		_ = dir.Close()
		return fmt.Errorf("project directory %q changed during snapshot", parent)
	}
	entries, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := item.Name()
		rel := name
		if parent != "." {
			rel = parent + "/" + name
		}
		if engine.IsExcluded(rel, item.IsDir()) {
			stats.ExcludedPath = append(stats.ExcludedPath, rel)
			continue
		}
		rel, err = ValidatePath(rel, limits, false)
		if err != nil {
			return fmt.Errorf("invalid project path %q: %w", rel, err)
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("stat project path %q: %w", rel, err)
		}
		if hasSpecialMode(info.Mode()) {
			return fmt.Errorf("project path %q has setuid, setgid, or sticky permissions", rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("project contains symlink %q", rel)
		}
		if info.IsDir() {
			if err := countEntry(stats, limits); err != nil {
				return err
			}
			mode := normalizeMode(info.Mode(), true)
			baseline[rel] = Entry{Type: EntryDirectory, Mode: mode}
			header := &tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: int64(mode), ModTime: time.Unix(0, 0).UTC()}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if err := packDirectory(ctx, root, tw, rel, baseline, stats, limits, engine); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported project file type for %q", rel)
		}
		if info.Size() < 0 || info.Size() > limits.MaxFileBytes {
			return fmt.Errorf("project file %q exceeds the per-file limit", rel)
		}
		if info.Size() > limits.MaxTotalBytes-stats.TotalBytes {
			return fmt.Errorf("project snapshot exceeds the total data limit")
		}
		if err := countEntry(stats, limits); err != nil {
			return err
		}
		if err := packRegularFile(ctx, root, tw, rel, info, baseline, stats); err != nil {
			return err
		}
	}
	return nil
}

func packRegularFile(ctx context.Context, root *os.Root, tw *tar.Writer, name string, expected fs.FileInfo, baseline Baseline, stats *Stats) error {
	f, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open project file %q: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != expected.Size() || !os.SameFile(expected, before) {
		return fmt.Errorf("project file %q changed during snapshot", name)
	}
	mode := normalizeMode(before.Mode(), false)
	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: int64(mode), Size: before.Size(), ModTime: time.Unix(0, 0).UTC()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, hash), contextReader{ctx: ctx, r: f}, before.Size()); err != nil {
		return fmt.Errorf("read project file %q: %w", name, err)
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("project file %q changed during snapshot", name)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	baseline[name] = Entry{Type: EntryRegular, Mode: mode, Size: before.Size(), Digest: digest}
	stats.Files++
	stats.TotalBytes += before.Size()
	return nil
}

func countEntry(stats *Stats, limits Limits) error {
	stats.Entries++
	if stats.Entries > limits.MaxEntries {
		return fmt.Errorf("project snapshot exceeds %d entries", limits.MaxEntries)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
