package workspace

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	projectLockTimeout = 30 * time.Second
	projectLockRetry   = 100 * time.Millisecond
	windowsOS          = "windows"
)

// LockProject serializes Buddy snapshot and merge operations for a project on
// this host. The lock file remains after release; the operating system releases
// ownership when the process exits.
func LockProject(ctx context.Context, projectRoot string) (func() error, error) {
	canonical, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project path: %w", err)
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, fmt.Errorf("resolve project symlinks: %w", err)
	}
	canonical = filepath.Clean(canonical)
	lockKey := canonical
	if runtime.GOOS == windowsOS {
		lockKey = strings.ToLower(lockKey)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locate user cache directory: %w", err)
	}
	lockDir := filepath.Join(cacheDir, "buddy-sbx", "workspace-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace lock directory: %w", err)
	}
	if err := os.Chmod(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure workspace lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(lockKey))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("%x.lock", digest))
	lock := flock.New(lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, projectLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(lockCtx, projectLockRetry)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("another Buddy operation is syncing this project")
		}
		return nil, fmt.Errorf("acquire project sync lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("another Buddy operation is syncing this project")
	}
	return lock.Unlock, nil
}

// MergeArchive stages a root-relative result archive and merges it against the
// baseline captured by Pack. Conflicting remote files are written beneath the
// project's recovery directory.
func MergeArchive(ctx context.Context, projectRoot, sandboxName string, baseline Baseline, src io.Reader, limits Limits) (MergeResult, error) {
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	if limits.TransferTimeout <= 0 {
		limits.TransferTimeout = DefaultLimits().TransferTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limits.TransferTimeout)
	defer cancel()

	if _, err := ValidatePath(sandboxName, limits, false); err != nil || strings.Contains(sandboxName, "/") {
		return MergeResult{}, fmt.Errorf("invalid sandbox name for recovery path")
	}

	stagePath, err := os.MkdirTemp("", "buddy-sbx-merge-")
	if err != nil {
		return MergeResult{}, fmt.Errorf("create merge staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagePath) }()
	stage, err := os.OpenRoot(stagePath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open merge staging directory: %w", err)
	}
	defer func() { _ = stage.Close() }()
	remote, err := unpackResult(ctx, stage, src, limits)
	if err != nil {
		return MergeResult{}, err
	}

	unlock, err := LockProject(ctx, projectRoot)
	if err != nil {
		return MergeResult{}, err
	}
	defer func() { _ = unlock() }()
	root, err := os.OpenRoot(projectRoot)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open project root: %w", err)
	}
	defer func() { _ = root.Close() }()
	return applyRemote(ctx, root, stage, sandboxName, baseline, remote)
}

func unpackResult(ctx context.Context, root *os.Root, src io.Reader, limits Limits) (Baseline, error) {
	tr := tar.NewReader(src)
	remote := make(Baseline)
	entries := 0
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// tar.Reader may stop after the end markers without reading the
			// transport to EOF. Drain it so stream trailers and read errors are
			// observed before the archive is treated as an authoritative snapshot.
			if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, r: src}); err != nil {
				return nil, fmt.Errorf("finish result archive download: %w", err)
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read result archive: %w", err)
		}
		entries, total, err = stageResultEntry(ctx, root, tr, h, remote, entries, total, limits)
		if err != nil {
			return nil, err
		}
	}
	return remote, nil
}

func stageResultEntry(
	ctx context.Context,
	root *os.Root,
	tr *tar.Reader,
	h *tar.Header,
	remote Baseline,
	entries int,
	total int64,
	limits Limits,
) (int, int64, error) {
	name := h.Name
	if h.Typeflag == tar.TypeDir {
		name = strings.TrimSuffix(name, "/")
	}
	name = strings.TrimPrefix(name, "./")
	if name == "." && h.Typeflag == tar.TypeDir {
		return entries, total, nil
	}
	clean, err := ValidatePath(name, limits, false)
	if err != nil {
		return entries, total, fmt.Errorf("unsafe result path: %w", err)
	}
	excluded, err := discardExcludedEntry(ctx, tr, clean)
	if err != nil {
		return entries, total, err
	}
	if excluded {
		return entries, total, nil
	}
	entries++
	if entries > limits.MaxEntries {
		return entries, total, fmt.Errorf("result archive exceeds %d entries", limits.MaxEntries)
	}
	if h.Typeflag == tar.TypeReg {
		if h.Size < 0 || h.Size > limits.MaxFileBytes || h.Size > limits.MaxTotalBytes-total {
			return entries, total, fmt.Errorf("result file %q exceeds transfer limits", clean)
		}
		total += h.Size
	}
	if h.Typeflag != tar.TypeDir && h.Mode&0o7000 != 0 {
		return entries, total, fmt.Errorf("result entry %q has setuid, setgid, or sticky permissions", clean)
	}
	if _, exists := remote[clean]; exists {
		return entries, total, fmt.Errorf("duplicate result path %q", clean)
	}
	mode := normalizeMode(fs.FileMode(h.Mode), h.Typeflag == tar.TypeDir)
	switch h.Typeflag {
	case tar.TypeDir:
		if h.Size != 0 {
			return entries, total, fmt.Errorf("directory entry %q has data", clean)
		}
		if err := root.MkdirAll(clean, 0o700); err != nil {
			return entries, total, fmt.Errorf("stage result directory %q: %w", clean, err)
		}
		remote[clean] = Entry{Type: EntryDirectory, Mode: mode}
	case tar.TypeReg:
		if err := ensureDirectories(root, filepath.ToSlash(filepath.Dir(clean))); err != nil {
			return entries, total, err
		}
		f, err := root.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return entries, total, fmt.Errorf("stage result file %q: %w", clean, err)
		}
		hash := sha256.New()
		n, copyErr := io.CopyN(io.MultiWriter(f, hash), contextReader{ctx: ctx, r: tr}, h.Size)
		if copyErr == nil && n != h.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr == nil {
			copyErr = f.Sync()
		}
		closeErr := f.Close()
		if copyErr != nil {
			return entries, total, fmt.Errorf("stage result file %q: %w", clean, copyErr)
		}
		if closeErr != nil {
			return entries, total, closeErr
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		remote[clean] = Entry{Type: EntryRegular, Mode: mode, Size: n, Digest: digest}
	default:
		return entries, total, fmt.Errorf("unsupported result archive entry type %d for %q", h.Typeflag, clean)
	}
	return entries, total, nil
}

func discardExcludedEntry(ctx context.Context, tr *tar.Reader, name string) (bool, error) {
	if !isExcludedForMerge(name) {
		return false, nil
	}
	if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, r: tr}); err != nil {
		return true, err
	}
	return true, nil
}

func ensureDirectories(root *os.Root, directory string) error {
	if directory == "." || directory == "" {
		return nil
	}
	parts := strings.Split(directory, "/")
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("workspace parent %q is not a directory", current)
		}
	}
	return nil
}

func applyRemote(ctx context.Context, root, stage *os.Root, sandboxName string, baseline, remote Baseline) (MergeResult, error) {
	result := MergeResult{}
	mergeBaseline := cloneBaseline(baseline)
	paths := make([]string, 0, len(remote))
	for name := range remote {
		paths = append(paths, name)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di == dj {
			return paths[i] < paths[j]
		}
		return di < dj
	})
	createdDirectories := make(map[string]bool)
	directoryModes := make(map[string]Entry)
	blockedDirectories := make(map[string]bool)
	if err := prepareDirectoryPermissionIncreases(root, paths, mergeBaseline, remote, directoryModes, blockedDirectories, &result); err != nil {
		return result, err
	}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entry := remote[name]
		skipped, err := skipBlockedRemoteEntry(root, stage, sandboxName, name, entry, mergeBaseline, blockedDirectories, &result)
		if err != nil {
			return result, err
		}
		if skipped {
			continue
		}
		skipped, err = skipChangedAncestorEntry(root, stage, sandboxName, name, entry, mergeBaseline, blockedDirectories, &result)
		if err != nil {
			return result, err
		}
		if skipped {
			continue
		}
		if entry.Type == EntryDirectory {
			if err := prepareRemoteDirectory(root, name, mergeBaseline, entry, createdDirectories, directoryModes, blockedDirectories, &result); err != nil {
				return result, err
			}
			continue
		}
		if err := applyRegularRemoteEntry(ctx, root, stage, sandboxName, name, entry, mergeBaseline, &result); err != nil {
			return result, err
		}
	}
	deleted := make([]string, 0)
	for name := range mergeBaseline {
		if _, stillExists := remote[name]; !stillExists {
			deleted = append(deleted, name)
		}
	}
	sort.Slice(deleted, func(i, j int) bool {
		di, dj := strings.Count(deleted[i], "/"), strings.Count(deleted[j], "/")
		if di == dj {
			return deleted[i] > deleted[j]
		}
		return di > dj
	})
	for _, name := range deleted {
		changedAncestor, err := changedBaselineAncestor(root, name, mergeBaseline)
		if err != nil || changedAncestor != "" {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local ancestor changed since snapshot; remote deletion was not applied"})
			continue
		}
		matches, err := matchesBaseline(root, name, mergeBaseline)
		if err != nil || !matches {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path changed since snapshot; remote deletion was not applied"})
			continue
		}
		if err := root.Remove(name); err != nil {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "directory contains local or excluded files; remote deletion was not applied"})
			continue
		}
		result.Deleted = append(result.Deleted, name)
	}
	if err := applyDirectoryModes(root, mergeBaseline, directoryModes, createdDirectories, &result); err != nil {
		return result, err
	}
	return result, nil
}

func cloneBaseline(baseline Baseline) Baseline {
	clone := make(Baseline, len(baseline))
	for name, entry := range baseline {
		clone[name] = entry
	}
	return clone
}

func prepareDirectoryPermissionIncreases(
	root *os.Root,
	paths []string,
	baseline, remote Baseline,
	directoryModes map[string]Entry,
	blocked map[string]bool,
	result *MergeResult,
) error {
	for _, name := range paths {
		entry := remote[name]
		if entry.Type != EntryDirectory {
			continue
		}
		prior, tracked := baseline[name]
		if !tracked || prior.Type != EntryDirectory || entry.Mode&0o222&^prior.Mode == 0 {
			continue
		}
		changedAncestor, err := changedBaselineAncestor(root, name, baseline)
		if err != nil {
			return err
		}
		matches, err := matchesBaseline(root, name, baseline)
		if err != nil || !matches || changedAncestor != "" {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local directory changed since snapshot"})
			blocked[name] = true
			continue
		}
		mode := prior.Mode | 0o200 | (entry.Mode & 0o222)
		if err := chmodRoot(root, name, mode); err != nil {
			return fmt.Errorf("make workspace directory %q writable for synchronization: %w", name, err)
		}
		prior.Mode = normalizeMode(mode, true)
		baseline[name] = prior
		directoryModes[name] = entry
	}
	return nil
}

func applyRegularRemoteEntry(
	ctx context.Context,
	root, stage *os.Root,
	sandboxName, name string,
	entry Entry,
	baseline Baseline,
	result *MergeResult,
) error {
	if prior, exists := baseline[name]; exists && prior.Type != EntryRegular {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path type changed since snapshot"})
		return copyRecovery(root, stage, sandboxName, name)
	}
	if prior, exists := baseline[name]; exists && sameEntry(prior, entry) {
		return nil
	}
	matches, err := matchesBaseline(root, name, baseline)
	if err != nil {
		matches = false
	}
	if !matches {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path changed since snapshot"})
		return copyRecovery(root, stage, sandboxName, name)
	}
	if err := ensureDirectories(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "parent directory changed since snapshot"})
		return copyRecovery(root, stage, sandboxName, name)
	}
	if err := replaceFromStage(ctx, root, stage, name, entry, baseline); err != nil {
		if errors.Is(err, errLocalChanged) {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path changed during synchronization"})
			return copyRecovery(root, stage, sandboxName, name)
		}
		return err
	}
	if _, exists := baseline[name]; exists {
		result.Modified = append(result.Modified, name)
	} else {
		result.Added = append(result.Added, name)
	}
	return nil
}

func skipBlockedRemoteEntry(
	root, stage *os.Root,
	sandboxName, name string,
	entry Entry,
	baseline Baseline,
	blocked map[string]bool,
	result *MergeResult,
) (bool, error) {
	if blockedAncestor(name, blocked) == "" {
		return false, nil
	}
	if entry.Type == EntryRegular && remoteEntryChanged(name, entry, baseline) {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "parent directory changed since snapshot"})
		if err := copyRecovery(root, stage, sandboxName, name); err != nil {
			return true, err
		}
	}
	return true, nil
}

func skipChangedAncestorEntry(
	root, stage *os.Root,
	sandboxName, name string,
	entry Entry,
	baseline Baseline,
	blocked map[string]bool,
	result *MergeResult,
) (bool, error) {
	changed, err := changedBaselineAncestor(root, name, baseline)
	if err != nil || changed == "" {
		return false, err
	}
	if !blocked[changed] {
		result.Conflicts = append(result.Conflicts, Conflict{Path: changed, Reason: "local directory changed since snapshot"})
	}
	blocked[changed] = true
	if entry.Type == EntryRegular && remoteEntryChanged(name, entry, baseline) {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "parent directory changed since snapshot"})
		if err := copyRecovery(root, stage, sandboxName, name); err != nil {
			return true, err
		}
	}
	return true, nil
}

func remoteEntryChanged(name string, entry Entry, baseline Baseline) bool {
	prior, exists := baseline[name]
	return !exists || !sameEntry(prior, entry)
}

func blockedAncestor(name string, blocked map[string]bool) string {
	parent := filepath.ToSlash(filepath.Dir(name))
	for parent != "." && parent != "/" {
		if blocked[parent] {
			return parent
		}
		parent = filepath.ToSlash(filepath.Dir(parent))
	}
	return ""
}

func changedBaselineAncestor(root *os.Root, name string, baseline Baseline) (string, error) {
	parent := filepath.ToSlash(filepath.Dir(name))
	for parent != "." && parent != "/" {
		expected, tracked := baseline[parent]
		if tracked {
			info, err := root.Lstat(parent)
			if errors.Is(err, fs.ErrNotExist) {
				return parent, nil
			}
			if err != nil {
				return "", err
			}
			if expected.Type != EntryDirectory || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || normalizeMode(info.Mode(), true) != expected.Mode {
				return parent, nil
			}
		}
		parent = filepath.ToSlash(filepath.Dir(parent))
	}
	return "", nil
}

var errLocalChanged = errors.New("local path changed during synchronization")

var rootRenameHook func(*os.Root, string, string) error
var rootChmodHook func(*os.Root, string, os.FileMode) error
var rootRemoveHook func(*os.Root, string) error

func renameRoot(root *os.Root, oldName, newName string) error {
	if rootRenameHook != nil {
		return rootRenameHook(root, oldName, newName)
	}
	return root.Rename(oldName, newName)
}

func chmodRoot(root *os.Root, name string, mode os.FileMode) error {
	if rootChmodHook != nil {
		return rootChmodHook(root, name, mode)
	}
	return root.Chmod(name, mode)
}

func removeRoot(root *os.Root, name string) error {
	if rootRemoveHook != nil {
		return rootRemoveHook(root, name)
	}
	return root.Remove(name)
}

func cleanupTemporary(root *os.Root, tempName string, cause error) error {
	if err := removeRoot(root, tempName); err != nil {
		return fmt.Errorf("%w; remove temporary file %q: %v", cause, tempName, err)
	}
	return cause
}

func replaceFromStage(ctx context.Context, root, stage *os.Root, name string, entry Entry, baseline Baseline) error {
	if matches, err := matchesBaseline(root, name, baseline); err != nil || !matches {
		return errLocalChanged
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	parent := filepath.ToSlash(filepath.Dir(name))
	tmp := ".buddy-sbx-tmp-" + suffix
	if parent != "." {
		tmp = parent + "/" + tmp
	}
	source, err := stage.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	destination, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(destination, contextReader{ctx: ctx, r: source}, entry.Size)
	if copyErr == nil {
		copyErr = destination.Chmod(entry.Mode)
	}
	if copyErr == nil {
		copyErr = destination.Sync()
	}
	closeErr := destination.Close()
	if copyErr != nil {
		return cleanupTemporary(root, tmp, copyErr)
	}
	if closeErr != nil {
		return cleanupTemporary(root, tmp, closeErr)
	}
	if matches, err := matchesBaseline(root, name, baseline); err != nil || !matches {
		return cleanupTemporary(root, tmp, errLocalChanged)
	}
	readOnlyDestination := false
	if runtime.GOOS == windowsOS {
		if info, statErr := root.Lstat(name); statErr == nil && normalizeMode(info.Mode(), false) == 0o444 {
			if matches, checkErr := matchesBaseline(root, name, baseline); checkErr != nil || !matches {
				return cleanupTemporary(root, tmp, errLocalChanged)
			}
			if err := chmodRoot(root, name, 0o644); err != nil {
				return cleanupTemporary(root, tmp, fmt.Errorf("clear read-only attribute on %q: %w", name, err))
			}
			readOnlyDestination = true
		} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return cleanupTemporary(root, tmp, statErr)
		}
	}
	if err := renameRoot(root, tmp, name); err != nil {
		rollbackErr := error(nil)
		if readOnlyDestination {
			rollbackErr = chmodRoot(root, name, 0o444)
		}
		cleanupErr := removeRoot(root, tmp)
		if rollbackErr != nil || cleanupErr != nil {
			return fmt.Errorf("publish workspace file %q: %w; restore read-only attribute: %v; remove temporary file %q: %v", name, err, rollbackErr, tmp, cleanupErr)
		}
		return fmt.Errorf("publish workspace file %q: %w; temporary file %q was removed", name, err, tmp)
	}
	return nil
}

func prepareRemoteDirectory(
	root *os.Root,
	name string,
	baseline Baseline,
	remote Entry,
	created map[string]bool,
	modes map[string]Entry,
	blocked map[string]bool,
	result *MergeResult,
) error {
	if prior, exists := baseline[name]; exists && prior.Type != EntryDirectory {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path type changed since snapshot"})
		blocked[name] = true
		return nil
	}
	if err := ensureDirectories(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "parent directory changed since snapshot"})
		return nil
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if prior, tracked := baseline[name]; tracked && prior.Type == EntryDirectory {
			result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local directory was deleted since snapshot"})
			blocked[name] = true
			return nil
		}
		if err := root.Mkdir(name, 0o700); err != nil {
			return err
		}
		created[name] = true
		info, err = root.Lstat(name)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path is not a directory"})
		blocked[name] = true
		return nil
	}
	expected, tracked := baseline[name]
	if !tracked {
		if created[name] {
			modes[name] = remote
			return nil
		}
		return nil
	}
	if expected.Type != EntryDirectory {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local path type changed since snapshot"})
		return nil
	}
	if normalizeMode(info.Mode(), true) != expected.Mode {
		result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local directory mode changed since snapshot"})
		blocked[name] = true
		return nil
	}
	if remote.Mode != expected.Mode {
		modes[name] = remote
	}
	return nil
}

func applyDirectoryModes(root *os.Root, baseline Baseline, modes map[string]Entry, created map[string]bool, result *MergeResult) error {
	paths := make([]string, 0, len(modes))
	for name := range modes {
		paths = append(paths, name)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di == dj {
			return paths[i] > paths[j]
		}
		return di > dj
	})
	for _, name := range paths {
		if !created[name] {
			matches, err := matchesBaseline(root, name, baseline)
			if err != nil || !matches {
				result.Conflicts = append(result.Conflicts, Conflict{Path: name, Reason: "local directory changed before mode update"})
				continue
			}
		}
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		info, err := dir.Stat()
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("workspace path %q is not a directory", name)
		}
		if err == nil {
			err = dir.Chmod(modes[name].Mode)
		}
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if created[name] {
			result.Added = append(result.Added, name)
		} else {
			result.Modified = append(result.Modified, name)
		}
	}
	return nil
}

func copyRecovery(root, stage *os.Root, sandboxName, name string) error {
	recovery := ".buddy-sbx-recovery/" + sandboxName + "/" + name
	if err := ensureDirectories(root, filepath.ToSlash(filepath.Dir(recovery))); err != nil {
		return err
	}
	info, err := stage.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	source, err := stage.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	destination, err := root.OpenFile(recovery, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func randomSuffix() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
