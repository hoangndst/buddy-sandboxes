package sbx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func setupKubectlTestServer(t *testing.T) func() {
	t.Helper()

	mockSbx := apiv1.SandboxResponse{
		ID:        "sbx-test-1",
		Name:      "sbx-test-1",
		Namespace: "default",
		Image:     "alpine:latest",
		Phase:     "Running",
		CreatedAt: time.Now().UTC(),
	}

	mockSess := apiv1.SessionResponse{
		ID:          "sess-test-1",
		Namespace:   "default",
		SandboxName: "sbx-test-1",
		Command:     []string{"sh", "-c", "echo hello"},
		State:       apiv1.SessionStateRunning,
		CreatedAt:   time.Now().UTC(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Sandboxes
		case path == "/api/v1/sandboxes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(apiv1.ListSandboxesResponse{
				Items: []apiv1.SandboxResponse{mockSbx},
				Total: 1,
			})
		case path == "/api/v1/sandboxes/sbx-test-1" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(mockSbx)
		case path == "/api/v1/sandboxes/sbx-test-1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		// Sessions
		case path == "/api/v1/sandboxes/sbx-test-1/sessions" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(apiv1.ListSessionsResponse{
				Sessions: []apiv1.SessionResponse{mockSess},
			})
		case path == "/api/v1/sandboxes/sbx-test-1/sessions/sess-test-1" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(mockSess)
		case path == "/api/v1/sandboxes/sbx-test-1/sessions/sess-test-1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		// Credentials
		case path == "/api/v1/credentials" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.CredentialMetadata{
					{Service: "openai", Scope: "user", UpdatedAt: time.Now().UTC()},
				},
			})
		case path == "/api/v1/credentials/openai" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		// Users
		case path == "/api/v1/admin/users" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.User{
					{
						ID:          "usr_alice",
						DisplayName: "Alice",
						Namespace:   "default",
						Role:        "admin",
						Disabled:    false,
						Created:     time.Now().UTC(),
					},
				},
			})
		case path == "/api/v1/admin/users/usr_alice/sandboxes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(apiv1.ListSandboxesResponse{
				Items: []apiv1.SandboxResponse{mockSbx},
				Total: 1,
			})

		default:
			http.NotFound(w, r)
		}
	}))

	prevEndpoint := flagEndpoint
	prevToken := flagToken
	flagEndpoint = server.URL
	flagToken = "test-token"

	return func() {
		server.Close()
		flagEndpoint = prevEndpoint
		flagToken = prevToken
	}
}

func execKubectlCmd(t *testing.T, args []string, wantOut ...string) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewRootCmdWithOptions(&RootOptions{
		Endpoint: flagEndpoint,
		Token:    flagToken,
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	out := buf.String()
	if err != nil {
		t.Fatalf("command failed with %v, output: %s", err, out)
	}

	for _, want := range wantOut {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got: %s", want, out)
		}
	}
}

func TestKubectlGetCommands(t *testing.T) {
	teardown := setupKubectlTestServer(t)
	defer teardown()

	// get sandboxes / sbx
	execKubectlCmd(t, []string{"get", "sbx"}, "sbx-test-1", "alpine:latest")
	execKubectlCmd(t, []string{"get", "sandboxes", "sbx-test-1"}, "sbx-test-1")
	execKubectlCmd(t, []string{"get", "sbx", "-o", "wide"}, "POD IP", "NAMESPACE")

	// get sessions / sess with slash notation and name filter
	execKubectlCmd(t, []string{"get", "sess", "-s", "sbx-test-1"}, "sess-test-1", "running")
	execKubectlCmd(t, []string{"get", "sessions", "sbx-test-1"}, "sess-test-1")
	execKubectlCmd(t, []string{"get", "sess", "sbx-test-1/sess-test-1"}, "sess-test-1")
	execKubectlCmd(t, []string{"get", "sess", "sess-test-1", "-s", "sbx-test-1"}, "sess-test-1")

	// get secrets with filter
	execKubectlCmd(t, []string{"get", "secrets"}, "openai", "user")
	execKubectlCmd(t, []string{"get", "secret", "openai"}, "openai")

	// get users with filter
	execKubectlCmd(t, []string{"get", "users"}, "usr_alice", "Alice")
	execKubectlCmd(t, []string{"get", "user", "usr_alice"}, "usr_alice")

	// get sandboxes filtered by user
	execKubectlCmd(t, []string{"get", "sbx", "--user", "usr_alice"}, "sbx-test-1")
	execKubectlCmd(t, []string{"get", "sbx", "sbx-test-1", "--user", "usr_alice"}, "sbx-test-1")

	var buf bytes.Buffer
	cmd := NewRootCmdWithOptions(&RootOptions{
		Endpoint: flagEndpoint,
		Token:    flagToken,
	})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"get", "sbx", "non-existent-box", "--user", "usr_alice"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected get sbx with unknown name to fail with not found, got %v", err)
	}
}

func TestKubectlDescribeCommands(t *testing.T) {
	teardown := setupKubectlTestServer(t)
	defer teardown()

	// describe sandbox
	execKubectlCmd(t, []string{"describe", "sbx", "sbx-test-1"}, "Name:", "sbx-test-1", "Phase:", "Running")

	// describe session (flag and slash syntax)
	execKubectlCmd(t, []string{"describe", "sess", "sess-test-1", "-s", "sbx-test-1"},
		"Session ID:", "sess-test-1", "Sandbox:", "sbx-test-1")
	execKubectlCmd(t, []string{"describe", "sess", "sbx-test-1/sess-test-1"}, "Session ID:", "sess-test-1")

	// describe user
	execKubectlCmd(t, []string{"describe", "user", "usr_alice"}, "User ID:", "usr_alice", "Role:", "admin")
}

func TestKubectlDeleteCommands(t *testing.T) {
	teardown := setupKubectlTestServer(t)
	defer teardown()

	// delete sandbox
	execKubectlCmd(t, []string{"delete", "sbx", "sbx-test-1"}, "sandbox \"sbx-test-1\" deleted")

	// delete session
	execKubectlCmd(t, []string{"delete", "sess", "sess-test-1", "-s", "sbx-test-1"}, "session \"sess-test-1\" deleted")
	execKubectlCmd(t, []string{"delete", "sess", "sbx-test-1/sess-test-1"}, "session \"sess-test-1\" deleted")

	// delete secret
	execKubectlCmd(t, []string{"delete", "secret", "openai"}, "secret \"openai\" deleted")

	// delete secret with invalid repo flag for non-git service
	var delBuf bytes.Buffer
	delCmd := NewRootCmdWithOptions(&RootOptions{
		Endpoint: flagEndpoint,
		Token:    flagToken,
	})
	delCmd.SetOut(&delBuf)
	delCmd.SetErr(&delBuf)
	delCmd.SetArgs([]string{"delete", "secret", "openai", "--repo", "https://github.com/org/repo.git"})
	if err := delCmd.Execute(); err == nil {
		t.Fatal("expected delete secret openai --repo to fail")
	}
	if !strings.Contains(delBuf.String(), "--repo is not valid for OpenAI credentials") {
		t.Fatalf("expected repo error message, got: %s", delBuf.String())
	}

	// delete failure returns an error instead of terminating via os.Exit
	delBuf.Reset()
	failCmd := NewRootCmdWithOptions(&RootOptions{
		Endpoint: flagEndpoint,
		Token:    flagToken,
	})
	failCmd.SetOut(&delBuf)
	failCmd.SetErr(&delBuf)
	failCmd.SetArgs([]string{"delete", "sbx", "non-existent-box"})
	if err := failCmd.Execute(); err == nil {
		t.Fatal("expected delete of non-existent sandbox to fail with an error")
	}
}

func TestKubectlAttachAutoResolution(t *testing.T) {
	teardown := setupKubectlTestServer(t)
	defer teardown()

	cmd := newAttachCmd()
	cmd.SetArgs([]string{"sbx-test-1"})
	// Reaching the attach websocket dial step confirms it successfully resolved to sess-test-1
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "attach websocket dial") {
		t.Fatalf("expected attach websocket dial error, got: %v", err)
	}
}
