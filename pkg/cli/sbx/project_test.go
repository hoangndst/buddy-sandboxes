package sbx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hoangndst/buddy-sandboxes/internal/workspace"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func TestDeleteSandboxAfterSyncIgnoresCanceledTerminalContext(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/sandboxes/project-work" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := client.NewClient(client.Config{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := deleteSandboxAfterSync(ctx, c, "project-work"); err != nil {
		t.Fatalf("delete after successful sync: %v", err)
	}
	if !deleted {
		t.Fatal("Sandbox deletion request was not sent")
	}
}

type projectGatewayOptions struct {
	conflict      bool
	nameCollision bool
	deleteFailure bool
	exitCode      int
	resultName    string
	failureStage  string
}

type projectGatewayState struct {
	created         bool
	createAttempted bool
	deleted         bool
	uploaded        bool
	downloaded      bool
	command         *apiv1.CreateSandboxRequest
	execCount       int
	lastExecTTY     string
}

//nolint:gocyclo // This fake gateway coordinates the HTTP and WebSocket lifecycle used by the integration tests.
func newProjectGateway(
	t *testing.T,
	project string,
	opts projectGatewayOptions,
) (*client.Client, *projectGatewayState, func()) {
	t.Helper()
	state := &projectGatewayState{}
	resultName := opts.resultName
	if resultName == "" {
		resultName = "result.txt"
	}
	resultArchive := projectResultArchive(t, resultName, "from-sandbox")
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/project-name":
			if opts.nameCollision || strings.HasPrefix(opts.failureStage, "ambiguous-create") && state.created {
				creationID := "foreign-creation-id"
				if opts.failureStage == "ambiguous-create-owned" {
					creationID = state.command.CreationID
				}
				_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "project-name", CreationID: creationID})
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == testSandboxesAPIPath:
			state.createAttempted = true
			if opts.failureStage == "create" {
				http.Error(w, "creation failed", http.StatusInternalServerError)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&state.command); err != nil {
				t.Errorf("decode create request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			state.created = true
			if strings.HasPrefix(opts.failureStage, "ambiguous-create") {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack create response: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "project-id", Phase: "Pending"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/project-id":
			phase := "Running"
			if opts.failureStage == "readiness" {
				phase = "Failed"
			}
			_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "project-id", Phase: phase})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/project-id/upload":
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read upload: %v", err)
			}
			state.uploaded = true
			if opts.failureStage == "upload" {
				http.Error(w, "upload failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/project-id/download":
			state.downloaded = true
			_, _ = w.Write(resultArchive)
		case r.Method == http.MethodDelete &&
			(r.URL.Path == "/api/v1/sandboxes/project-id" || r.URL.Path == "/api/v1/sandboxes/project-name"):
			state.deleted = true
			if opts.deleteFailure {
				http.Error(w, "delete unavailable", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/project-id/exec":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade exec websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if opts.conflict {
				if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("local-edit"), 0o600); err != nil {
					t.Errorf("write local conflict: %v", err)
				}
			}
			state.execCount++
			state.lastExecTTY = r.URL.Query().Get("tty")
			exitCode := opts.exitCode
			if opts.failureStage == "initialization" {
				exitCode = 1
			}
			exit, _ := json.Marshal(apiv1.ExitMessage{ExitCode: exitCode})
			_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{apiv1.ChannelExit}, exit...))
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			time.Sleep(20 * time.Millisecond)
		default:
			http.Error(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL), http.StatusNotFound)
		}
	}))
	c, err := client.NewClient(client.Config{Endpoint: server.URL})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return c, state, server.Close
}

func projectResultArchive(t *testing.T, name, body string) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}
	if err := w.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func projectWorkflowOptions(project string, remove bool) projectRunOptions {
	preset := ResolvePresetOrImage("shell")
	return projectRunOptions{
		Path: project, Name: "project-name", Remove: remove, Duration: 5 * time.Minute,
		Preset: preset, CreateRequest: &apiv1.CreateSandboxRequest{Image: preset.Image},
		AgentCommand: append([]string(nil), preset.Command...),
	}
}

func codexProjectWorkflowOptions(project string) projectRunOptions {
	preset := ResolvePresetOrImage("codex")
	return projectRunOptions{
		Path: project, Name: "project-name", Duration: 5 * time.Minute,
		Preset: preset, CreateRequest: &apiv1.CreateSandboxRequest{Image: preset.Image},
		AgentCommand: append([]string(nil), preset.Command...),
	}
}

func TestRunProjectWorkflowDetachedStartsKeeperWithoutAgentExec(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{})
	defer closeServer()
	opts := projectWorkflowOptions(project, false)
	opts.Detach = true
	if err := runProjectWorkflow(context.Background(), c, opts); err != nil {
		t.Fatalf("detached project run: %v", err)
	}
	if !state.created || !state.uploaded || state.downloaded || state.deleted {
		t.Fatalf("unexpected detached lifecycle: %+v", state)
	}
	command := state.command
	if command == nil || len(command.Command) != 3 || command.Command[0] != "/bin/sh" ||
		command.Command[2] != "sleep infinity" {
		t.Fatalf("detached command = %#v", state.command)
	}
}

func TestRunProjectWorkflowSynchronizesBeforeDeleteEvenOnAgentError(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{exitCode: 7})
	defer closeServer()
	err := runProjectWorkflow(context.Background(), c, projectWorkflowOptions(project, true))
	var exitErr ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("workflow error = %v, want agent exit code 7", err)
	}
	if !state.uploaded || !state.downloaded || !state.deleted {
		t.Fatalf("nonzero exit did not synchronize and delete: %+v", state)
	}
	if got, err := os.ReadFile(filepath.Join(project, "result.txt")); err != nil || string(got) != "from-sandbox" {
		t.Fatalf("synchronized result = %q, %v", got, err)
	}
}

func TestRunProjectWorkflowBrokeredCodexDoesNotForwardKey(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(openAIAPIKeyEnv, "sk-test")
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{})
	defer closeServer()
	if err := runProjectWorkflow(context.Background(), c, codexProjectWorkflowOptions(project)); err != nil {
		t.Fatalf("brokered Codex project run: %v", err)
	}
	if !state.uploaded || !state.downloaded || state.execCount != 2 {
		t.Fatalf("unexpected brokered project lifecycle: %+v", state)
	}
	if state.command == nil || state.command.Provider != providerOpenAI || state.command.Network == nil ||
		!state.command.Network.AllowAPIBroker {
		t.Fatalf("Codex Sandbox lacks broker network access: %#v", state.command)
	}
	for _, env := range state.command.Env {
		if env.Name == openAIAPIKeyEnv {
			t.Fatal("host provider key was forwarded into the Sandbox")
		}
	}
}

func TestRunProjectWorkflowReportsSandboxIDWhenDeleteFails(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{deleteFailure: true})
	defer closeServer()
	err := runProjectWorkflow(context.Background(), c, projectWorkflowOptions(project, true))
	if err == nil || !strings.Contains(err.Error(), "project-id") {
		t.Fatalf("workflow error = %v, want recoverable Sandbox ID", err)
	}
	if !state.downloaded || !state.deleted {
		t.Fatalf("expected sync followed by attempted deletion: %+v", state)
	}
}

func TestRunProjectWorkflowRetainsSandboxWhenMergeConflicts(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{conflict: true, resultName: "source.txt"})
	defer closeServer()
	err := runProjectWorkflow(context.Background(), c, projectWorkflowOptions(project, true))
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("workflow error = %v, want merge conflict", err)
	}
	if !state.downloaded || state.deleted {
		t.Fatalf("conflicted Sandbox was not retained: %+v", state)
	}
	if got, err := os.ReadFile(filepath.Join(project, "source.txt")); err != nil || string(got) != "local-edit" {
		t.Fatalf("local conflicting edit = %q, %v", got, err)
	}
}

func TestRunProjectWorkflowRejectsNameCollisionBeforeCreate(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{nameCollision: true})
	defer closeServer()
	err := runProjectWorkflow(context.Background(), c, projectWorkflowOptions(project, false))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("workflow error = %v, want name collision", err)
	}
	if state.created || state.uploaded || state.downloaded {
		t.Fatalf("name collision created or used Sandbox: %+v", state)
	}
}

func TestRunProjectWorkflowCleanupAfterBrokeredSetupFailures(t *testing.T) {
	stages := []string{
		"create", "readiness", "upload", "initialization",
		"ambiguous-create-owned", "ambiguous-create-foreign",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			project := t.TempDir()
			if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(openAIAPIKeyEnv, "sk-test")
			c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{failureStage: stage})
			defer closeServer()
			err := runProjectWorkflow(context.Background(), c, codexProjectWorkflowOptions(project))
			if err == nil {
				t.Fatal("workflow succeeded despite injected setup failure")
			}
			if stage == "create" {
				if !state.createAttempted || state.created || state.deleted {
					t.Fatalf("ambiguous create failure should revoke session without deleting an unowned Sandbox: %+v", state)
				}
				return
			}
			if stage == "ambiguous-create-owned" {
				if !state.deleted {
					t.Fatalf("owned Sandbox from ambiguous create was not cleaned up: %+v", state)
				}
				return
			}
			if stage == "ambiguous-create-foreign" {
				if state.deleted {
					t.Fatalf("foreign Sandbox was deleted after ambiguous create: %+v", state)
				}
				return
			}
			if !state.created || !state.deleted {
				t.Fatalf("owned Sandbox was not cleaned up after %s failure: %+v", stage, state)
			}
			if stage == "upload" && !state.uploaded {
				t.Fatal("upload failure was not reached")
			}
			if stage == "initialization" && state.execCount != 1 {
				t.Fatalf("workspace initialization calls = %d, want one", state.execCount)
			}
		})
	}
}

func TestSplitRunArguments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		dash       int
		wantTarget string
		wantPath   string
		wantTask   []string
		wantErr    bool
	}{
		{name: "no project", args: []string{"codex"}, dash: -1, wantTarget: "codex"},
		{name: "project path", args: []string{"codex", "."}, dash: -1, wantTarget: "codex", wantPath: "."},
		{
			name: "task after separator", args: []string{"codex", ".", "Fix", "tests"}, dash: 2,
			wantTarget: "codex", wantPath: ".", wantTask: []string{"Fix", "tests"},
		},
		{
			name: "task without path", args: []string{"codex", "Fix", "tests"}, dash: 1,
			wantTarget: "codex", wantTask: []string{"Fix", "tests"},
		},
		{name: "reject extra positional", args: []string{"codex", ".", "Fix"}, dash: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, project, task, err := splitRunArguments(tc.args, tc.dash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if target != tc.wantTarget || project != tc.wantPath || !reflect.DeepEqual(task, tc.wantTask) {
				t.Fatalf("got target=%q project=%q task=%q", target, project, task)
			}
		})
	}
}

func TestPrintMergeSummary(t *testing.T) {
	// 1. Empty changes
	var buf bytes.Buffer
	printMergeSummary(&buf, workspace.MergeResult{})
	if !strings.Contains(buf.String(), "(no files changed)") {
		t.Errorf("expected no files changed summary, got: %q", buf.String())
	}

	// 2. Formatted changes
	buf.Reset()
	merge := workspace.MergeResult{
		Added:    []string{"pkg/b.go"},
		Modified: []string{"pkg/a.go"},
		Deleted:  []string{"pkg/c.go"},
	}
	printMergeSummary(&buf, merge)
	out := buf.String()
	if !strings.Contains(out, "✓ Workspace synchronized:") {
		t.Errorf("expected header, got: %q", out)
	}
	if !strings.Contains(out, "  M pkg/a.go") ||
		!strings.Contains(out, "  A pkg/b.go") ||
		!strings.Contains(out, "  D pkg/c.go") {
		t.Errorf("expected formatted changes, got: %q", out)
	}

	// 3. Overflow cap (over 20 files)
	buf.Reset()
	var manyModified []string
	for i := 0; i < 25; i++ {
		manyModified = append(manyModified, fmt.Sprintf("file_%02d.txt", i))
	}
	printMergeSummary(&buf, workspace.MergeResult{Modified: manyModified})
	out = buf.String()
	if !strings.Contains(out, "... and 5 more files") {
		t.Errorf("expected overflow summary, got: %q", out)
	}
}

func TestRunProjectWorkflowNoSyncSkipsDownload(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{})
	defer closeServer()

	opts := projectWorkflowOptions(project, true)
	opts.NoSync = true

	if err := runProjectWorkflow(context.Background(), c, opts); err != nil {
		t.Fatalf("no-sync project workflow failed: %v", err)
	}
	if !state.uploaded {
		t.Fatal("expected project to be uploaded for in-container work")
	}
	if state.downloaded {
		t.Fatal("expected download to be skipped when --no-sync is enabled")
	}
	if !state.deleted {
		t.Fatal("expected sandbox to be deleted when remove is true")
	}
}

func TestRunProjectWorkflowHeadlessFailureAbortsSync(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{exitCode: 5})
	defer closeServer()

	opts := projectWorkflowOptions(project, true)
	opts.TaskArgs = []string{"run", "unit-tests"} // Headless task mode

	err := runProjectWorkflow(context.Background(), c, opts)
	var exitErr ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("expected ExitCodeError 5, got: %v", err)
	}
	if state.lastExecTTY != "false" {
		t.Fatalf("expected headless execution to pass tty=false, got: %q", state.lastExecTTY)
	}
	if state.downloaded {
		t.Fatal("expected workspace download/merge to be aborted on headless exit code failure")
	}
	if state.deleted {
		t.Fatal("expected sandbox to be retained for post-mortem recovery on failure")
	}
}

func TestRunProjectWorkflowCancellationAbortsSync(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, state, closeServer := newProjectGateway(t, project, projectGatewayOptions{})
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before or during execution

	opts := projectWorkflowOptions(project, true)
	err := runProjectWorkflow(ctx, c, opts)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
	if state.downloaded {
		t.Fatal("expected workspace download to be skipped on cancellation")
	}
}
