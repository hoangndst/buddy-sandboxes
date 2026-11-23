package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsExcluded(t *testing.T) {
	for _, name := range []string{
		".git/config", "sub/.buddy/state", "nested/node_modules/pkg/file.js", "dist/app.js",
		"bin/app", "build/out.bin", "out/binary",
		".env", "nested/.env.local", ".npmrc", "certs/key.pem", "keys/id_rsa_work",
		".config/gcloud/credentials.db", ".ssh/known_hosts", ".buddy-sbx-recovery/sbx/file",
	} {
		if !IsExcluded(name) {
			t.Errorf("IsExcluded(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"src/main.go", "environment", "my-key.txt", "identity_notes.go", ".config/editor/settings"} {
		if IsExcluded(name) {
			t.Errorf("IsExcluded(%q) = true, want false", name)
		}
	}
}

func TestMergeExclusionAliasesRespectFilesystemCaseBehavior(t *testing.T) {
	for _, name := range []string{".GIT/hooks/post-checkout", "node_Modules/package.js", "certs/secret.PEM"} {
		if !isExcludedForFilesystem(name, true) {
			t.Errorf("case-insensitive exclusion did not match %q", name)
		}
		if isExcludedForFilesystem(name, false) {
			t.Errorf("case-sensitive exclusion unexpectedly matched %q", name)
		}
	}
}

func TestValidatePath(t *testing.T) {
	limits := DefaultLimits()
	for _, name := range []string{"../outside", "a/../b", "/absolute", `C:\work`, "C:relative", "a\\b", "a//b", "CON.txt", "dir/NUL", "trailing.", "name:stream", "bad\x00name"} {
		if _, err := ValidatePath(name, limits, false); err == nil {
			t.Errorf("ValidatePath(%q) unexpectedly succeeded", name)
		}
	}
	for _, name := range []string{"main.go", "src/main.go", "path with spaces/file.go"} {
		if _, err := ValidatePath(name, limits, false); err != nil {
			t.Errorf("ValidatePath(%q) failed: %v", name, err)
		}
	}
	if got, err := ValidatePath("", limits, true); err != nil || got != "." {
		t.Fatalf("ValidatePath root = %q, %v; want ., nil", got, err)
	}
}

func TestIgnoreEngine(t *testing.T) {
	gitRules := []string{"custom_bin/*", "*.log", "data/"}
	agentRules := []string{"agent_private.md", "evals/**"}

	engine := NewIgnoreEngineFromLines(gitRules, agentRules)

	// Test gitignore matches
	if !engine.IsExcluded("custom_bin/binary", false) {
		t.Errorf("expected custom_bin/binary to be excluded by gitignore")
	}
	if !engine.IsExcluded("server.log", false) {
		t.Errorf("expected server.log to be excluded by gitignore")
	}
	if !engine.IsExcluded("data", true) {
		t.Errorf("expected data directory to be excluded by gitignore")
	}

	// Test agentsignore matches
	if !engine.IsExcluded("agent_private.md", false) {
		t.Errorf("expected agent_private.md to be excluded by agentsignore")
	}
	if !engine.IsExcluded("evals/benchmark/task1.json", false) {
		t.Errorf("expected evals/benchmark/task1.json to be excluded by agentsignore")
	}

	// Test security floor invariants apply unconditionally
	if !engine.IsExcluded(".env", false) {
		t.Errorf("expected .env to be excluded by security floor")
	}
	if !engine.IsExcluded(".ssh/id_rsa", false) {
		t.Errorf("expected .ssh/id_rsa to be excluded by security floor")
	}

	// Test non-matching file
	if engine.IsExcluded("src/main.go", false) {
		t.Errorf("unexpected exclusion of src/main.go")
	}
}

func TestNewIgnoreEngine_ErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	oversizedLine := strings.Repeat("a", 1024*1024+10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".agentsignore"), []byte(oversizedLine), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	_, err = NewIgnoreEngine(root)
	if err == nil {
		t.Fatalf("expected error for oversized line in .agentsignore, got nil")
	}
}

func TestNewIgnoreEngine_MissingOptional(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	engine, err := NewIgnoreEngine(root)
	if err != nil {
		t.Fatalf("unexpected error when ignore files are missing: %v", err)
	}
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}
	if !engine.IsExcluded("bin/app", false) {
		t.Errorf("expected bin/app to be excluded by default rules")
	}
}

func TestNestedIgnoreEngine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(dir, "packages", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".gitignore"), []byte("node_modules/\ndist/\n*.tmp\n!keep.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	engine, err := NewIgnoreEngine(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !engine.IsExcluded("root.log", false) {
		t.Errorf("expected root.log to be excluded by root .gitignore")
	}
	if !engine.IsExcluded("packages/app/test.log", false) {
		t.Errorf("expected packages/app/test.log to be excluded by root .gitignore")
	}
	if !engine.IsExcluded("packages/app/dist/bundle.js", false) {
		t.Errorf("expected packages/app/dist/bundle.js to be excluded by nested .gitignore")
	}
	if !engine.IsExcluded("packages/app/node_modules/foo.js", false) {
		t.Errorf("expected packages/app/node_modules/foo.js to be excluded by nested .gitignore")
	}
	if !engine.IsExcluded("packages/app/temp.tmp", false) {
		t.Errorf("expected packages/app/temp.tmp to be excluded by nested .gitignore")
	}
	if engine.IsExcluded("packages/app/keep.tmp", false) {
		t.Errorf("expected packages/app/keep.tmp to be included via negation rule")
	}
	if engine.IsExcluded("packages/app/src/index.ts", false) {
		t.Errorf("unexpected exclusion of packages/app/src/index.ts")
	}
}
