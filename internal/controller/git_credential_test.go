package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitCredentialHelperScopesCredentials(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"GIT_CREDENTIAL_HOST": "git.example.net",
		"GIT_CREDENTIAL_PATH": "team/service.git",
		"GIT_USERNAME":        "git-user",
		"GIT_TOKEN":           "test-secret-value",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte(gitCredentialHelperScript), 0o700); err != nil {
		t.Fatal(err)
	}

	run := func(input string) string {
		t.Helper()
		cmd := exec.Command(helper, "get")
		cmd.Env = append(os.Environ(), "BUDDY_GIT_CREDENTIAL_DIR="+dir)
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("credential helper failed: %v: %s", err, output)
		}
		return string(output)
	}

	matching := run("protocol=https\nhost=git.example.net\npath=team/service.git\n\n")
	if !strings.Contains(matching, "username=git-user\npassword=test-secret-value") {
		t.Fatalf("matching repository did not receive credentials: %q", matching)
	}
	for name, input := range map[string]string{
		"different host":    "protocol=https\nhost=other.example.net\npath=team/service.git\n\n",
		"different path":    "protocol=https\nhost=git.example.net\npath=team/other.git\n\n",
		"insecure protocol": "protocol=http\nhost=git.example.net\npath=team/service.git\n\n",
	} {
		if output := run(input); strings.Contains(output, "test-secret-value") {
			t.Errorf("%s received credentials: %q", name, output)
		}
	}
}

func TestGitCloneScriptKeepsPodTokenOutOfCurlArguments(t *testing.T) {
	if output, err := exec.Command("/bin/sh", "-n", "-c", gitCloneScript).CombinedOutput(); err != nil {
		t.Fatalf("Git init script has invalid shell syntax: %v: %s", err, output)
	}
	for _, required := range []string{"--config \"$BROKER_CONFIG\"", "chmod 0600 \"$BROKER_CONFIG\"", "--max-time 10", "--max-redirs 0", "trap git_auth_finish EXIT"} {
		if !strings.Contains(gitCloneScript, required) {
			t.Errorf("Git init script is missing %q", required)
		}
	}
	if !strings.Contains(gitCloneScript, "*[!A-Za-z0-9._-]*") {
		t.Fatal("Git init token validation must accept the dot and hyphen characters in projected JWTs")
	}
	if strings.Contains(gitCloneScript, "buddy-git-auth") || strings.Contains(gitCloneScript, "--header \"Authorization: Bearer $token\"") {
		t.Fatal("Git init script passes the projected token to another process as an argument")
	}
}
