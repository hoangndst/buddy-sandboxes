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

func TestTasksCLIKubectlCommands(t *testing.T) {
	activeExec := "exec_01"
	mockTask := apiv1.TaskResponse{
		ID:                "task_01",
		UserID:            "usr_alice",
		Namespace:         "default",
		WorkspaceName:     "repo-auth",
		Prompt:            "Fix race condition in auth.go",
		Status:            apiv1.TaskStatePending,
		MaxAttempts:       1,
		ActiveExecutionID: &activeExec,
		CreatedAt:         time.Now().UTC(),
	}

	mockDiff := "--- a/auth.go\n+++ b/auth.go\n@@ -1 +1 @@\n-old\n+new\n"
	mockExec := apiv1.ExecutionResponse{
		ID:            "exec_01",
		TaskID:        "task_01",
		Namespace:     "default",
		WorkspaceName: "repo-auth",
		AttemptNumber: 1,
		Status:        apiv1.ExecutionStateRunning,
		Diff:          &mockDiff,
		CreatedAt:     time.Now().UTC(),
	}

	mockProfile := apiv1.AgentProfileResponse{
		ID:                  "prof_01",
		Name:                "Golang Harness",
		HarnessType:         "codex",
		ModelName:           "gpt-4o",
		VerificationCommand: "go test ./...",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/v1/tasks" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apiv1.ListTasksResponse{
				Tasks: []apiv1.TaskResponse{mockTask},
			})
		case path == "/api/v1/tasks/task_01" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mockTask)
		case path == "/api/v1/tasks" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(mockTask)
		case path == "/api/v1/tasks/task_01/cancel" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
		case path == "/api/v1/tasks/task_01/executions" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apiv1.ListExecutionsResponse{
				Executions: []apiv1.ExecutionResponse{mockExec},
			})
		case path == "/api/v1/executions/exec_01" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mockExec)
		case path == "/api/v1/executions/exec_01/logs" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("=== Step 1: Running unit tests ===\nPASS\n"))
		case path == "/api/v1/agent-profiles" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apiv1.ListAgentProfilesResponse{
				Profiles: []apiv1.AgentProfileResponse{mockProfile},
			})
		case path == "/api/v1/agent-profiles/prof_01" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mockProfile)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	opts := &RootOptions{
		Endpoint: server.URL,
		Token:    "test-token",
	}

	tests := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{
			name:       "get tasks tabular",
			args:       []string{"get", "tasks"},
			wantSubstr: "task_01",
		},
		{
			name:       "get task single",
			args:       []string{"get", "task", "task_01"},
			wantSubstr: "repo-auth",
		},
		{
			name:       "get executions for task",
			args:       []string{"get", "executions", "--task", "task_01"},
			wantSubstr: "exec_01",
		},
		{
			name:       "get execution single",
			args:       []string{"get", "execution", "exec_01"},
			wantSubstr: "exec_01",
		},
		{
			name:       "get profiles",
			args:       []string{"get", "profiles"},
			wantSubstr: "prof_01",
		},
		{
			name:       "run task",
			args:       []string{"run", "task", "--workspace", "repo-auth", "--prompt", "Fix race condition in auth.go"},
			wantSubstr: "Task task_01 submitted",
		},
		{
			name:       "create task",
			args:       []string{"create", "task", "--workspace", "repo-auth", "--prompt", "Fix bug in auth"},
			wantSubstr: "Created task task_01",
		},
		{
			name:       "get task diff",
			args:       []string{"get", "task", "task_01", "--diff"},
			wantSubstr: "+new",
		},
		{
			name:       "logs task args",
			args:       []string{"logs", "task", "task_01"},
			wantSubstr: "Step 1: Running unit tests",
		},
		{
			name:       "logs execution args",
			args:       []string{"logs", "execution", "exec_01"},
			wantSubstr: "Step 1: Running unit tests",
		},
		{
			name:       "logs execution",
			args:       []string{"logs", "execution/exec_01"},
			wantSubstr: "Step 1: Running unit tests",
		},
		{
			name:       "delete task",
			args:       []string{"delete", "task", "task_01"},
			wantSubstr: "task \"task_01\" cancelled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rootCmd := NewRootCmdWithOptions(opts)
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(tc.args)

			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("unexpected error executing %v: %v", tc.args, err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.wantSubstr) {
				t.Fatalf("expected output to contain %q, got: %s", tc.wantSubstr, out)
			}
		})
	}
}
