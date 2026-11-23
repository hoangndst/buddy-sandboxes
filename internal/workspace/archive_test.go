package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPackUsesRelativePathsAndSharedExclusionPolicy(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, "empty"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".config", "gcloud"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"src/main.go":                "package main\n",
		".env.local":                 "do not copy",
		".config/gcloud/credentials": "do not copy",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(project, filepath.FromSlash(name)), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	var archive bytes.Buffer
	baseline, stats, err := Pack(context.Background(), project, &archive, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.TotalBytes != int64(len(files["src/main.go"])) || stats.Entries != 4 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if _, ok := baseline[".env.local"]; ok {
		t.Fatal("excluded environment file was added to baseline")
	}
	if _, ok := baseline["empty"]; !ok {
		t.Fatal("empty directory missing from baseline")
	}
	if got := baseline["src/main.go"].Digest; got != sha256.Sum256([]byte(files["src/main.go"])) {
		t.Fatalf("unexpected file digest: %x", got)
	}
	expectedFileMode := os.FileMode(0o640)
	expectedDirectoryMode := os.FileMode(0o751)
	if runtime.GOOS == "windows" {
		expectedFileMode = 0o644
		expectedDirectoryMode = 0o755
	}
	if baseline["src/main.go"].Mode != expectedFileMode || baseline["empty"].Mode != expectedDirectoryMode {
		t.Fatalf("unexpected normalized modes: file=%o directory=%o", baseline["src/main.go"].Mode, baseline["empty"].Mode)
	}
	if len(stats.ExcludedPath) != 2 {
		t.Fatalf("excluded paths = %v, want two entries", stats.ExcludedPath)
	}

	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if len(names) != 4 || names[0] != ".config/" || names[1] != "empty/" || names[2] != "src/" || names[3] != "src/main.go" {
		t.Fatalf("archive paths = %v", names)
	}
}

func TestPackRejectsSymlinksAndLimits(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		project := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(project, "link")); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		if _, _, err := Pack(context.Background(), project, io.Discard, DefaultLimits()); err == nil {
			t.Fatal("Pack succeeded with symlink")
		}
	})

	t.Run("entry limit", func(t *testing.T) {
		project := t.TempDir()
		if err := os.Mkdir(filepath.Join(project, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "sub", "a"), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		limits := DefaultLimits()
		limits.MaxEntries = 1
		if _, _, err := Pack(context.Background(), project, io.Discard, limits); err == nil {
			t.Fatal("Pack succeeded above entry limit")
		}
	})
}

func TestPackWithGitIgnoreAndAgentsIgnore(t *testing.T) {
	project := t.TempDir()

	if err := os.MkdirAll(filepath.Join(project, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "build"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		".gitignore":       "build/*\n*.log\n",
		".agentsignore":    "agent_secret.txt\n",
		"src/app.go":       "package main\n",
		"build/binary":     "ELF-BINARY-DATA",
		"app.log":          "some logs",
		"agent_secret.txt": "sensitive agent notes",
	}

	for rel, content := range files {
		path := filepath.Join(project, filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	baseline, stats, err := Pack(context.Background(), project, &buf, DefaultLimits())
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	if _, ok := baseline["src/app.go"]; !ok {
		t.Errorf("src/app.go should be included in baseline")
	}
	if _, ok := baseline["build/binary"]; ok {
		t.Errorf("build/binary should be excluded by .gitignore")
	}
	if _, ok := baseline["app.log"]; ok {
		t.Errorf("app.log should be excluded by .gitignore")
	}
	if _, ok := baseline["agent_secret.txt"]; ok {
		t.Errorf("agent_secret.txt should be excluded by .agentsignore")
	}

	excludedSet := make(map[string]bool)
	for _, p := range stats.ExcludedPath {
		excludedSet[p] = true
	}

	if !excludedSet["build/binary"] && !excludedSet["build"] {
		t.Errorf("expected build or build/binary in ExcludedPath, got: %v", stats.ExcludedPath)
	}
	if !excludedSet["app.log"] {
		t.Errorf("expected app.log in ExcludedPath, got: %v", stats.ExcludedPath)
	}
	if !excludedSet["agent_secret.txt"] {
		t.Errorf("expected agent_secret.txt in ExcludedPath, got: %v", stats.ExcludedPath)
	}
}
