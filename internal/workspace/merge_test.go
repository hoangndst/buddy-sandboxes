package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name     string
	typeFlag byte
	mode     int64
	body     string
}

func resultTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, entry := range entries {
		h := &tar.Header{Name: entry.name, Typeflag: entry.typeFlag, Mode: entry.mode, ModTime: time.Unix(0, 0)}
		if entry.typeFlag == tar.TypeReg {
			h.Size = int64(len(entry.body))
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(w, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func makeBaseline(t *testing.T, project string) Baseline {
	t.Helper()
	var archive bytes.Buffer
	baseline, _, err := Pack(context.Background(), project, &archive, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return baseline
}

func TestMergeArchiveAppliesChangesAndDeletesBottomUp(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, "remove-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"change.txt": "before", "gone.txt": "gone", "remove-dir/stale.txt": "stale"} {
		if err := os.WriteFile(filepath.Join(project, filepath.FromSlash(name)), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	baseline := makeBaseline(t, project)
	archive := resultTar(t,
		tarEntry{name: "new-dir/", typeFlag: tar.TypeDir, mode: 0o750},
		tarEntry{name: "empty-dir/", typeFlag: tar.TypeDir, mode: 0o711},
		tarEntry{name: "change.txt", typeFlag: tar.TypeReg, mode: 0o600, body: "after"},
		tarEntry{name: "new-dir/new.txt", typeFlag: tar.TypeReg, mode: 0o640, body: "new"},
	)
	result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", result.Conflicts)
	}
	if !contains(result.Modified, "change.txt") || !contains(result.Added, "new-dir") ||
		!contains(result.Added, "new-dir/new.txt") || !contains(result.Added, "empty-dir") {
		t.Fatalf("unexpected additions/modifications: %+v", result)
	}
	for _, deleted := range []string{"gone.txt", "remove-dir/stale.txt", "remove-dir"} {
		if !contains(result.Deleted, deleted) {
			t.Errorf("deleted paths %v do not contain %q", result.Deleted, deleted)
		}
	}
	if got, err := os.ReadFile(filepath.Join(project, "change.txt")); err != nil || string(got) != "after" {
		t.Fatalf("modified file = %q, %v", got, err)
	}
	expectedNewDirectoryMode := os.FileMode(0o750)
	expectedEmptyDirectoryMode := os.FileMode(0o711)
	if runtime.GOOS == windowsOS {
		expectedNewDirectoryMode = 0o755
		expectedEmptyDirectoryMode = 0o755
	}
	if got, err := os.Stat(filepath.Join(project, "new-dir")); err != nil || normalizeMode(got.Mode(), true) != expectedNewDirectoryMode {
		t.Fatalf("new directory mode = %v, %v", got, err)
	}
	if got, err := os.Stat(filepath.Join(project, "empty-dir")); err != nil || normalizeMode(got.Mode(), true) != expectedEmptyDirectoryMode {
		t.Fatalf("empty directory mode = %v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(project, "remove-dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted directory still exists: %v", err)
	}
}

func TestMergeArchiveNormalizesFileModesByPlatform(t *testing.T) {
	project := t.TempDir()
	local := filepath.Join(project, "file.txt")
	if err := os.WriteFile(local, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	remoteMode := int64(0o755)
	if runtime.GOOS != windowsOS {
		archive := resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: remoteMode, body: "same"})
		result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if !contains(result.Modified, "file.txt") {
			t.Fatalf("POSIX executable mode change was not applied: %+v", result)
		}
	} else {
		archive := resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: remoteMode, body: "same"})
		result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if contains(result.Modified, "file.txt") {
			t.Fatalf("POSIX-only mode change reported on Windows: %+v", result)
		}
	}
	baseline = makeBaseline(t, project)
	archive := resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: 0o444, body: "readonly"})
	if _, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == windowsOS {
		if normalizeMode(info.Mode(), false) != 0o444 {
			t.Fatalf("Windows file was not made read-only: %v", info.Mode())
		}
	} else if info.Mode().Perm() != 0o444 {
		t.Fatalf("POSIX file mode = %o, want 444", info.Mode().Perm())
	}
	baseline = makeBaseline(t, project)
	archive = resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: 0o644, body: "updated"})
	if _, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits()); err != nil {
		t.Fatalf("replace read-only destination: %v", err)
	}
	content, err := os.ReadFile(local)
	if err != nil || string(content) != "updated" {
		t.Fatalf("updated read-only destination = %q, %v", content, err)
	}
	info, err = os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == windowsOS && normalizeMode(info.Mode(), false) != 0o644 {
		t.Fatalf("Windows file was not made writable: %v", info.Mode())
	}
}

//nolint:gocyclo // The test checks related read-only failure cases against platform-specific behavior.
func TestWindowsReadOnlyMergeFailures(t *testing.T) {
	if runtime.GOOS != windowsOS {
		t.Skip("Windows read-only attributes are specific to Windows")
	}
	project := t.TempDir()
	local := filepath.Join(project, "file.txt")
	if err := os.WriteFile(local, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local, 0o444); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	remote := func() []byte {
		return resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: 0o644, body: "remote"})
	}

	t.Run("changed local read-only file conflicts", func(t *testing.T) {
		if err := os.Chmod(local, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("local edit"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(local, 0o444); err != nil {
			t.Fatal(err)
		}
		result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(remote()), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Conflicts) != 1 {
			t.Fatalf("conflicts = %+v", result.Conflicts)
		}
		info, err := os.Stat(local)
		if err != nil || normalizeMode(info.Mode(), false) != 0o444 {
			t.Fatalf("conflicting file lost read-only attribute: %v, %v", info, err)
		}
		if content, err := os.ReadFile(local); err != nil || string(content) != "local edit" {
			t.Fatalf("local edit changed: %q, %v", content, err)
		}
	})

	t.Run("rename failure rolls back read-only attribute", func(t *testing.T) {
		if err := os.Chmod(local, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("baseline"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(local, 0o444); err != nil {
			t.Fatal(err)
		}
		rootRenameHook = func(*os.Root, string, string) error { return errors.New("forced rename failure") }
		chmodCalls := 0
		rootChmodHook = func(root *os.Root, name string, mode os.FileMode) error {
			chmodCalls++
			return root.Chmod(name, mode)
		}
		defer func() {
			rootRenameHook = nil
			rootChmodHook = nil
			rootRemoveHook = nil
		}()
		_, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(remote()), DefaultLimits())
		if err == nil || !strings.Contains(err.Error(), "forced rename failure") {
			t.Fatalf("merge error = %v", err)
		}
		if chmodCalls != 2 {
			t.Fatalf("Chmod calls = %d, want clear and restore", chmodCalls)
		}
		info, statErr := os.Stat(local)
		if statErr != nil || normalizeMode(info.Mode(), false) != 0o444 {
			t.Fatalf("read-only attribute not restored: %v, %v", info, statErr)
		}
		if content, readErr := os.ReadFile(local); readErr != nil || string(content) != "baseline" {
			t.Fatalf("original bytes changed: %q, %v", content, readErr)
		}
		entries, readErr := os.ReadDir(project)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".buddy-sbx-tmp-") {
				t.Fatalf("temporary file remains after rollback: %s", entry.Name())
			}
		}
	})

	t.Run("rollback failure is reported", func(t *testing.T) {
		if err := os.Chmod(local, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("baseline"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(local, 0o444); err != nil {
			t.Fatal(err)
		}
		rootRenameHook = func(*os.Root, string, string) error { return errors.New("forced rename failure") }
		chmodCalls := 0
		rootChmodHook = func(root *os.Root, name string, mode os.FileMode) error {
			chmodCalls++
			if chmodCalls == 2 {
				return errors.New("forced readonly rollback failure")
			}
			return root.Chmod(name, mode)
		}
		rootRemoveHook = func(*os.Root, string) error { return errors.New("forced temp cleanup failure") }
		defer func() {
			rootRenameHook = nil
			rootChmodHook = nil
			rootRemoveHook = nil
		}()
		_, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(remote()), DefaultLimits())
		if err == nil || !strings.Contains(err.Error(), "forced readonly rollback failure") || !strings.Contains(err.Error(), "forced temp cleanup failure") {
			t.Fatalf("rollback error = %v", err)
		}
	})
}

type mutateOnRead struct {
	reader io.Reader
	mutate func()
	once   bool
}

func (r *mutateOnRead) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		r.mutate()
	}
	return r.reader.Read(p)
}

func TestMergeArchiveDetectsEditDuringDownload(t *testing.T) {
	project := t.TempDir()
	local := filepath.Join(project, "file.txt")
	if err := os.WriteFile(local, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	archive := resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: 0o600, body: "remote"})
	input := &mutateOnRead{
		reader: bytes.NewReader(archive),
		mutate: func() {
			if err := os.WriteFile(local, []byte("edited during download"), 0o600); err != nil {
				t.Error(err)
			}
		},
	}
	result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, input, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Path != "file.txt" {
		t.Fatalf("conflicts = %+v", result.Conflicts)
	}
	if got, err := os.ReadFile(local); err != nil || string(got) != "edited during download" {
		t.Fatalf("local edit lost: %q, %v", got, err)
	}
}

func TestMergeArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, item := range []tarEntry{
		{name: "../outside", typeFlag: tar.TypeReg, mode: 0o600, body: "bad"},
		{name: "link", typeFlag: tar.TypeSymlink, mode: 0o777},
		{name: "device", typeFlag: tar.TypeChar, mode: 0o600},
		{name: "setuid", typeFlag: tar.TypeReg, mode: 0o4755, body: "bad"},
	} {
		t.Run(item.name, func(t *testing.T) {
			project := t.TempDir()
			archive := resultTar(t, item)
			if _, err := MergeArchive(context.Background(), project, "sandbox-test", Baseline{}, bytes.NewReader(archive), DefaultLimits()); err == nil {
				t.Fatalf("MergeArchive accepted unsafe entry %q", item.name)
			}
		})
	}
}

func TestMergeArchiveSkipsExcludedGitEntriesBeforeModeValidation(t *testing.T) {
	project := t.TempDir()
	archive := resultTar(t,
		tarEntry{name: ".git/", typeFlag: tar.TypeDir, mode: 0o2775},
		tarEntry{name: ".git/config", typeFlag: tar.TypeReg, mode: 0o644, body: "config"},
	)
	if _, err := MergeArchive(context.Background(), project, "sandbox-test", Baseline{}, bytes.NewReader(archive), DefaultLimits()); err != nil {
		t.Fatalf("excluded Git metadata was rejected: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("excluded Git metadata was merged: %v", err)
	}
}

func TestMergeArchivePreservesLocalConflictAndRecoversRemoteFile(t *testing.T) {
	project := t.TempDir()
	localPath := filepath.Join(project, "file.txt")
	if err := os.WriteFile(localPath, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	if err := os.WriteFile(localPath, []byte("local edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := resultTar(t, tarEntry{name: "file.txt", typeFlag: tar.TypeReg, mode: 0o600, body: "remote edit"})
	result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Path != "file.txt" {
		t.Fatalf("conflicts = %+v", result.Conflicts)
	}
	if got, _ := os.ReadFile(localPath); string(got) != "local edit" {
		t.Fatalf("local file overwritten: %q", got)
	}
	recovered, err := os.ReadFile(filepath.Join(project, ".buddy-sbx-recovery", "sandbox-test", "file.txt"))
	if err != nil || string(recovered) != "remote edit" {
		t.Fatalf("recovered remote file = %q, %v", recovered, err)
	}
}

func TestMergeArchivePreservesUntrackedChildrenWhenDeletingDirectory(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, "tracked-dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(resultTar(t)), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "untracked.txt")); err != nil {
		t.Fatalf("untracked child was removed: %v", err)
	}
	found := false
	for _, conflict := range result.Conflicts {
		if conflict.Path == "tracked-dir" {
			found = true
		}
	}
	if !found {
		t.Fatalf("directory deletion conflict missing: %+v", result.Conflicts)
	}
}

func TestMergeArchiveDoesNotFollowSwappedAncestorSymlink(t *testing.T) {
	project := t.TempDir()
	parent := filepath.Join(project, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(parent, "file.txt")
	if err := os.WriteFile(localPath, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Skipf("directory symlink creation unavailable: %v", err)
	}
	archive := resultTar(t, tarEntry{name: "parent/file.txt", typeFlag: tar.TypeReg, mode: 0o600, body: "remote"})
	result, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("swapped ancestor did not cause a merge conflict")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside sentinel changed to %q, %v", got, err)
	}
}

func TestMergeArchiveRejectsAncestorSwapBeforePublish(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("directory symlink creation may require elevated privileges on Windows")
	}
	project := t.TempDir()
	parent := filepath.Join(project, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(parent, "file.txt")
	if err := os.WriteFile(localPath, []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := makeBaseline(t, project)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootRenameHook = func(root *os.Root, oldName, newName string) error {
		if err := root.Rename("parent", "parent-original"); err != nil {
			return err
		}
		if err := os.Symlink(outside, parent); err != nil {
			return err
		}
		return root.Rename(oldName, newName)
	}
	defer func() { rootRenameHook = nil }()
	archive := resultTar(t, tarEntry{name: "parent/file.txt", typeFlag: tar.TypeReg, mode: 0o600, body: "remote"})
	if _, err := MergeArchive(context.Background(), project, "sandbox-test", baseline, bytes.NewReader(archive), DefaultLimits()); err == nil {
		t.Fatal("ancestor replacement race was accepted")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside sentinel changed to %q, %v", got, err)
	}
}

func TestLockProjectSerializesAliasesAndIndependentRoots(t *testing.T) {
	first := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	unlock, err := LockProject(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := LockProject(ctx, alias); err == nil || !strings.Contains(err.Error(), "another Buddy operation") {
		t.Fatalf("alias lock error = %v", err)
	}
	other, err := LockProject(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("independent root lock failed: %v", err)
	}
	if err := other(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == windowsOS {
		caseAlias := strings.ToUpper(first)
		caseCtx, caseCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		if _, err := LockProject(caseCtx, caseAlias); err == nil || !strings.Contains(err.Error(), "another Buddy operation") {
			caseCancel()
			t.Fatalf("case-fold alias lock error = %v", err)
		}
		caseCancel()
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestLockProjectCancellationAndCacheFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LockProject(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock error = %v", err)
	}
	if runtime.GOOS != "linux" {
		t.Skip("XDG_CACHE_HOME override is Linux-specific")
	}
	cacheFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cacheFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", cacheFile)
	if _, err := LockProject(context.Background(), t.TempDir()); err == nil {
		t.Fatal("LockProject succeeded with an invalid cache path")
	}
}

func TestLockProjectSubprocessHelper(t *testing.T) {
	if os.Getenv("BUDDY_WORKSPACE_LOCK_HELPER") != "1" {
		return
	}
	root := os.Getenv("BUDDY_WORKSPACE_LOCK_ROOT")
	ready := os.Getenv("BUDDY_WORKSPACE_LOCK_READY")
	stop := os.Getenv("BUDDY_WORKSPACE_LOCK_STOP")
	unlock, err := LockProject(context.Background(), root)
	if err != nil {
		t.Fatalf("helper lock: %v", err)
	}
	runtime.KeepAlive(unlock)
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			runtime.KeepAlive(unlock)
			os.Exit(0)
		}
		time.Sleep(10 * time.Millisecond)
	}
	runtime.KeepAlive(unlock)
	os.Exit(3)
}

func TestLockProjectReleasesWhenProcessExits(t *testing.T) {
	project := t.TempDir()
	control := t.TempDir()
	ready := filepath.Join(control, "ready")
	stop := filepath.Join(control, "stop")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockProjectSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		"BUDDY_WORKSPACE_LOCK_HELPER=1",
		"BUDDY_WORKSPACE_LOCK_ROOT="+project,
		"BUDDY_WORKSPACE_LOCK_READY="+ready,
		"BUDDY_WORKSPACE_LOCK_STOP="+stop,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(stop, []byte("stop"), 0o600)
		if !waited {
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	if _, err := LockProject(ctx, project); err == nil || !strings.Contains(err.Error(), "another Buddy operation") {
		cancel()
		t.Fatalf("contending process lock error = %v", err)
	}
	cancel()
	if err := os.WriteFile(stop, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock helper exit: %v", err)
	}
	waited = true
	unlock, err := LockProject(context.Background(), project)
	if err != nil {
		t.Fatalf("lock not released after helper exit: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		t.Fatal(err)
	}
	key := canonical
	if runtime.GOOS == windowsOS {
		key = strings.ToLower(key)
	}
	digest := sha256.Sum256([]byte(key))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(cache, "buddy-sbx", "workspace-locks", fmt.Sprintf("%x.lock", digest))
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should remain after release: %v", err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("lock file is not empty: size=%d", info.Size())
	}
	if runtime.GOOS != windowsOS && info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", info.Mode().Perm())
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
