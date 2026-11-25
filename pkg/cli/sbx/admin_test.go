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
	"github.com/spf13/cobra"
)

const adminUsersEndpoint = "/api/v1/admin/users"

func setupAdminTestServer(t *testing.T) func() {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == adminUsersEndpoint && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.User{
					{ID: "usr_alice", DisplayName: "Alice", Namespace: "ns-alice", Role: "user", Disabled: false, Created: time.Now()},
				},
			})
		case path == adminUsersEndpoint && r.Method == http.MethodPost:
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.User{
				ID: "usr_bob", DisplayName: req["displayName"], Namespace: req["namespace"], Role: "user", Created: time.Now(),
			})
		case path == "/api/v1/admin/users/usr_alice" && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/v1/admin/users/usr_alice/role" && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/v1/admin/users/usr_alice/tokens" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "tok_abc", "token": "bb_sec_secret_token", "expiresAt": time.Now().Add(24 * time.Hour),
			})
		case path == "/api/v1/admin/users/usr_alice/tokens/tok_abc" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/v1/admin/users/usr_alice/sandboxes" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.SandboxResponse{
					{Name: "sbx-test", Phase: "Running", Image: "alpine", Namespace: "ns-alice", CreatedAt: time.Now()},
				},
				"total": 1,
			})
		case path == "/api/v1/admin/users/usr_alice/sandboxes/sbx-test" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))

	prevEndpoint := flagEndpoint
	prevToken := flagToken
	flagEndpoint = server.URL
	flagToken = "admin-token"

	return func() {
		server.Close()
		flagEndpoint = prevEndpoint
		flagToken = prevToken
	}
}

func execAdminTestCmd(t *testing.T, cmd *cobra.Command, args []string, wantSubstrings ...string) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if args == nil {
		args = []string{}
	}
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s failed: %v", cmd.Name(), err)
	}
	out := buf.String()
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Fatalf("%s output missing expected string %q, got: %s", cmd.Name(), want, out)
		}
	}
}

func TestAdminUsersCommands(t *testing.T) {
	teardown := setupAdminTestServer(t)
	defer teardown()

	execAdminTestCmd(t, newGetCmd(), []string{"users"}, "usr_alice", "Alice")
	execAdminTestCmd(t, newCreateUserCmd(), []string{"Bob", "--namespace", "ns-bob"}, "Created user Bob", "usr_bob")
	execAdminTestCmd(t, newSetUserCmd(), []string{"usr_alice", "--disable"}, "Disabled user usr_alice")
	execAdminTestCmd(t, newSetUserCmd(), []string{"usr_alice", "--enable"}, "Enabled user usr_alice")
	execAdminTestCmd(t, newSetRoleCmd(),
		[]string{"usr_alice", "admin"}, "Updated role for user usr_alice to admin")
}

func TestAdminTokensCommands(t *testing.T) {
	teardown := setupAdminTestServer(t)
	defer teardown()

	execAdminTestCmd(t, newCreateTokenCmd(), []string{"usr_alice"}, "bb_sec_secret_token", "tok_abc")
	execAdminTestCmd(t, newDeleteCmd(),
		[]string{"token", "tok_abc", "--user", "usr_alice"}, "token \"tok_abc\" revoked for user \"usr_alice\"")
}

func TestAdminSandboxesCommands(t *testing.T) {
	teardown := setupAdminTestServer(t)
	defer teardown()

	execAdminTestCmd(t, newGetCmd(), []string{"sbx", "--user", "usr_alice"}, "sbx-test", "Running")
	execAdminTestCmd(t, newDeleteCmd(),
		[]string{"sbx", "sbx-test", "--user", "usr_alice"}, "sandbox \"sbx-test\" deleted")
}
