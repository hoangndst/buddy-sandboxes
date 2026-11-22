package workspace

import "testing"

func TestIsExcluded(t *testing.T) {
	for _, name := range []string{
		".git/config", "sub/.buddy/state", "nested/node_modules/pkg/file.js", "dist/app.js",
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
