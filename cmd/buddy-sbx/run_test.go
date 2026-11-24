package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

const testSandboxesAPIPath = "/api/v1/sandboxes"

func TestBuildRunConfigRuntimeClass(t *testing.T) {
	preset := ResolvePresetOrImage("golang:1.25")

	// 1. Without runtime class
	req := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, false, "", "", "", 0, "", "")
	if req.RuntimeClassName != nil {
		t.Errorf("expected RuntimeClassName to be nil, got %v", *req.RuntimeClassName)
	}

	// 2. With runtime class gvisor
	reqGvisor := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, false, "gvisor", "", "", 0, "", "")
	if reqGvisor.RuntimeClassName == nil || *reqGvisor.RuntimeClassName != "gvisor" {
		t.Errorf("expected RuntimeClassName to be 'gvisor', got %v", reqGvisor.RuntimeClassName)
	}

	// 3. With runtime class kata
	reqKata := buildRunConfig(preset, []string{"golang:1.25"}, false, "", "test-sbx",
		nil, "", "", 0, false, "kata-containers", "", "", 0, "", "")
	if reqKata.RuntimeClassName == nil || *reqKata.RuntimeClassName != "kata-containers" {
		t.Errorf("expected RuntimeClassName to be 'kata-containers', got %v", reqKata.RuntimeClassName)
	}

	// 4. With Git repository and PVC
	reqGit := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "", "", 0, false, "", "https://github.com/example/repo.git", "main", 1, "sub", "test-pvc")
	if reqGit.Workspace == nil {
		t.Fatalf("expected Workspace to be populated")
	}
	if reqGit.Workspace.Ephemeral {
		t.Errorf("expected Ephemeral to be false when PVC is set")
	}
	if reqGit.Workspace.Git == nil || reqGit.Workspace.Git.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected Git URL to match")
	}
	if reqGit.Workspace.PersistentVolumeClaim == nil || reqGit.Workspace.PersistentVolumeClaim.ClaimName != "test-pvc" {
		t.Errorf("expected PVC to match")
	}
}

func TestParseMCPServerFlag(t *testing.T) {
	flagVal := "name=filesystem,image=mcp/filesystem:latest,port=8080," +
		"mountWorkspace=true,mountWorkspaceReadOnly=true"
	spec, err := parseMCPServerFlag(flagVal)
	if err != nil {
		t.Fatalf("unexpected error parsing valid MCP server flag: %v", err)
	}
	if spec.Name != "filesystem" || spec.Image != "mcp/filesystem:latest" ||
		spec.Port != 8080 || !spec.MountWorkspace || !spec.MountWorkspaceReadOnly {
		t.Errorf("unexpected parsed spec: %+v", spec)
	}

	minimal, err := parseMCPServerFlag("name=db,image=redis:7-alpine,port=6379")
	if err != nil {
		t.Fatalf("unexpected error parsing minimal MCP server flag: %v", err)
	}
	if minimal.Name != "db" || minimal.Image != "redis:7-alpine" || minimal.Port != 6379 || minimal.MountWorkspace {
		t.Errorf("unexpected parsed minimal spec: %+v", minimal)
	}

	if _, err := parseMCPServerFlag("image=redis:7-alpine,port=6379"); err == nil {
		t.Errorf("expected error for missing name")
	}

	if _, err := parseMCPServerFlag("name=db,port=6379"); err == nil {
		t.Errorf("expected error for missing image")
	}

	if _, err := parseMCPServerFlag("name=db,image=redis,port=70000"); err == nil {
		t.Errorf("expected error for port > 65535")
	}
	if _, err := parseMCPServerFlag("name=db,image=redis,port=0"); err == nil {
		t.Errorf("expected error for port 0")
	}
	if _, err := parseMCPServerFlag("name=db,image=redis,port=invalid"); err == nil {
		t.Errorf("expected error for non-integer port")
	}

	if _, err := parseMCPServerFlag("name=fs,image=mcp/fs,port=8080,mountWorkspace=notabool"); err == nil {
		t.Errorf("expected error for non-bool mountWorkspace")
	}

	if _, err := parseMCPServerFlag("name=fs,image=mcp/fs,port=8080,unknownKey=val"); err == nil {
		t.Errorf("expected error for unknown key")
	}
}

func TestRunRejectsInvalidDuration(t *testing.T) {
	for _, test := range []struct{ input, message string }{
		{input: "-1s", message: "--duration cannot be negative"},
		{input: fmt.Sprintf("%ds", sandboxesv1alpha1.MaxActiveDeadlineSeconds+1), message: "--duration cannot exceed"},
	} {
		cmd := newRunCmd()
		cmd.SetArgs([]string{"golang:1.25", "--duration=" + test.input})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("expected duration error containing %q, got %v", test.message, err)
		}
	}
}

func TestRunRejectsInvalidCloneOptionsBeforeClientSetup(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "local path", args: []string{"codex", "--clone", "https://github.com/example/repo.git", "."}},
		{name: "legacy URL conflict", args: []string{
			"codex", "--clone", "https://github.com/example/repo.git", "--git-url", "https://github.com/example/other.git",
		}},
		{name: "branch without repository", args: []string{"codex", "--branch", "main"}},
		{name: "traversal subpath", args: []string{
			"codex", "--clone", "https://github.com/example/repo.git", "--git-subpath", "../../outside",
		}},
		{name: "detached clone", args: []string{"codex", "--clone", "https://github.com/example/repo.git", "--detach"}},
		{name: "secret without repository", args: []string{"codex", "--git-secret-ref", "repo-creds"}},
		{name: "PVC conflict", args: []string{
			"codex", "--clone", "https://github.com/example/repo.git", "--pvc", "workspace",
		}},
		{name: "userinfo URL", args: []string{"codex", "--clone", "https://user:token@github.com/example/repo.git"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRunCmd()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected clone option validation error")
			}
		})
	}
}

func TestRunRejectsCustomProjectImageWithoutCommandBeforeClientSetup(t *testing.T) {
	cmd := newRunCmd()
	cmd.SetArgs([]string{"example.invalid/custom:latest", "."})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires a command after -- or --entrypoint") {
		t.Fatalf("expected early custom image command error, got %v", err)
	}
}

func TestCustomProjectCommandRequirementAllowsDetachedKeeper(t *testing.T) {
	preset := ResolvePresetOrImage("example.invalid/custom:latest")
	if !requiresCustomProjectCommand(".", preset, "", nil, false, false) {
		t.Fatal("attached custom-image project without a command should be rejected")
	}
	if requiresCustomProjectCommand(".", preset, "", nil, false, true) {
		t.Fatal("detached custom-image project with an idle keeper should not require an agent command")
	}
}

func TestRunRejectsProviderKeyEnvironmentForwarding(t *testing.T) {
	t.Setenv(openAIAPIKeyEnv, "sk-test")
	cmd := newRunCmd()
	cmd.SetArgs([]string{"codex", "--env", openAIAPIKeyEnv + "=sk-test"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "managed by Buddy") {
		t.Fatalf("expected provider key forwarding to be rejected, got %v", err)
	}
}

func TestBuildRunConfigProcessNamespaceAndDuration(t *testing.T) {
	preset := ResolvePresetOrImage("golang:1.25")
	build := func(duration time.Duration, share bool) *apiv1.CreateSandboxRequest {
		return buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
			nil, "", "", duration, share, "", "", "", 0, "", "")
	}

	if req := build(0, false); req.MaxDurationSeconds != nil || req.ShareProcessNamespace != nil {
		t.Fatalf("zero duration and default sharing should be omitted: %#v", req)
	}
	if got := *build(500*time.Millisecond, false).MaxDurationSeconds; got != 1 {
		t.Fatalf("500ms should round up to 1 second, got %d", got)
	}
	if got := *build(1100*time.Millisecond, false).MaxDurationSeconds; got != 2 {
		t.Fatalf("1.1s should round up to 2 seconds, got %d", got)
	}
	if got := *build(2*time.Second, false).MaxDurationSeconds; got != 2 {
		t.Fatalf("2s should remain 2 seconds, got %d", got)
	}
	if req := build(0, true); req.ShareProcessNamespace == nil || !*req.ShareProcessNamespace {
		t.Fatalf("explicit process sharing should be sent as true: %#v", req.ShareProcessNamespace)
	}
}

func TestRunCommandRuntimeFlags(t *testing.T) {
	cmd := newRunCmd()
	err := cmd.ParseFlags([]string{"--runtime-class", "gvisor", "golang:1.25"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	runtimeClassVal, err := cmd.Flags().GetString("runtime-class")
	if err != nil || runtimeClassVal != "gvisor" {
		t.Errorf("expected runtime-class flag to be 'gvisor', got %s, err: %v", runtimeClassVal, err)
	}

	cmdShare := newRunCmd()
	if err := cmdShare.ParseFlags([]string{"--share-process-namespace", "golang:1.25"}); err != nil {
		t.Fatalf("unexpected error parsing process namespace flag: %v", err)
	}
	shareValue, err := cmdShare.Flags().GetBool("share-process-namespace")
	if err != nil || !shareValue {
		t.Errorf("expected share-process-namespace flag true, got %v, err: %v", shareValue, err)
	}

	cmdRuntime := newRunCmd()
	err = cmdRuntime.ParseFlags([]string{"--runtime", "kata-containers", "golang:1.25"})
	if err != nil {
		t.Fatalf("unexpected error parsing --runtime alias: %v", err)
	}

	aliasVal, err := cmdRuntime.Flags().GetString("runtime")
	if err != nil || aliasVal != "kata-containers" {
		t.Errorf("expected runtime alias flag to be 'kata-containers', got %s, err: %v", aliasVal, err)
	}
}

func TestRunCommandNoSyncAndReadOnlyFlags(t *testing.T) {
	cmd := newRunCmd()
	err := cmd.ParseFlags([]string{"--no-sync", "codex"})
	if err != nil {
		t.Fatalf("unexpected error parsing --no-sync: %v", err)
	}
	noSync, err := cmd.Flags().GetBool("no-sync")
	if err != nil || !noSync {
		t.Errorf("expected --no-sync to be true, got %v, err: %v", noSync, err)
	}

	cmdRO := newRunCmd()
	err = cmdRO.ParseFlags([]string{"--read-only", "codex"})
	if err != nil {
		t.Fatalf("unexpected error parsing --read-only: %v", err)
	}
	execErr := cmdRO.RunE(cmdRO, []string{"codex"})
	if execErr == nil || !strings.Contains(execErr.Error(), "--read-only is invalid") {
		t.Errorf("expected error about --read-only being invalid, got: %v", execErr)
	}
}

func TestRunCommandModelValidation(t *testing.T) {
	cmd := newRunCmd()
	if err := cmd.ParseFlags([]string{"--model", "gpt-4o", "shell"}); err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}
	err := cmd.RunE(cmd, []string{"shell"})
	if err == nil || !strings.Contains(err.Error(), "--model is only supported for AI agent presets") {
		t.Errorf("expected error about --model on shell, got: %v", err)
	}
}

func TestRootCommandSilenceConfiguration(t *testing.T) {
	if !rootCmd.SilenceUsage {
		t.Error("rootCmd.SilenceUsage must be true to suppress Cobra usage help on non-zero exit codes")
	}
	if !rootCmd.SilenceErrors {
		t.Error("rootCmd.SilenceErrors must be true to prevent duplicate error banners on handled exits")
	}
}

func newDetachedTestServer(
	t *testing.T,
	sbxID, sessID string,
	createdSandbox, createdSession *bool,
	sessionCmd *[]string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testSandboxesAPIPath:
			*createdSandbox = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: sbxID, Phase: "Running"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/"+sbxID:
			_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: sbxID, Phase: "Running"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/"+sbxID+"/sessions":
			*createdSession = true
			var req apiv1.CreateSessionRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			*sessionCmd = req.Command
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.SessionResponse{ID: sessID, SandboxName: sbxID})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestDetachedRunWithCommandCreatesSession(t *testing.T) {
	var createdSandbox bool
	var createdSession bool
	var sessionCmd []string

	server := newDetachedTestServer(t, "test-sbx-123", "sess-abc", &createdSandbox, &createdSession, &sessionCmd)
	defer server.Close()

	oldEndpoint := flagEndpoint
	flagEndpoint = server.URL
	defer func() { flagEndpoint = oldEndpoint }()

	cmd := newRunCmd()
	cmd.SetArgs([]string{"shell", "-d", "--", "echo", "hello-detached"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected detached run with command to succeed, got error: %v", err)
	}

	if !createdSandbox {
		t.Error("expected sandbox to be created")
	}
	if !createdSession {
		t.Error("expected session to be created")
	}
	want := []string{"/bin/bash", "echo", "hello-detached"}
	if !reflect.DeepEqual(sessionCmd, want) {
		t.Errorf("unexpected session command: got %v, want %v", sessionCmd, want)
	}
}

func TestDetachedRunWithEntrypointOverridesSessionCommand(t *testing.T) {
	var createdSandbox bool
	var createdSession bool
	var sessionCmd []string

	server := newDetachedTestServer(t, "test-sbx-ep", "sess-ep", &createdSandbox, &createdSession, &sessionCmd)
	defer server.Close()

	oldEndpoint := flagEndpoint
	flagEndpoint = server.URL
	defer func() { flagEndpoint = oldEndpoint }()

	cmd := newRunCmd()
	cmd.SetArgs([]string{"ubuntu:24.04", "-d", "--entrypoint", "/bin/sh", "--", "-c", "echo hello"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected detached run with entrypoint to succeed, got error: %v", err)
	}

	if !createdSandbox {
		t.Error("expected sandbox to be created")
	}
	if !createdSession {
		t.Error("expected session to be created")
	}
	expectedCmd := []string{"/bin/sh", "-c", "echo hello"}
	if !reflect.DeepEqual(sessionCmd, expectedCmd) {
		t.Errorf("unexpected session command: got %v, want %v", sessionCmd, expectedCmd)
	}
}

func TestDetachedRunWithAgentPresetCodex(t *testing.T) {
	var createdSandbox bool
	var createdSession bool
	var sessionCmd []string

	server := newDetachedTestServer(t, "test-sbx-codex", "sess-codex", &createdSandbox, &createdSession, &sessionCmd)
	defer server.Close()

	oldEndpoint := flagEndpoint
	flagEndpoint = server.URL
	defer func() { flagEndpoint = oldEndpoint }()

	cmd := newRunCmd()
	cmd.SetArgs([]string{"codex", "-d", "--", "fix compiler errors"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected detached run with codex preset to succeed, got error: %v", err)
	}

	if !createdSandbox {
		t.Error("expected sandbox to be created")
	}
	if !createdSession {
		t.Error("expected session to be created")
	}
	expectedCmd := []string{
		"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox",
		"exec", "--skip-git-repo-check", "fix compiler errors",
	}
	if !reflect.DeepEqual(sessionCmd, expectedCmd) {
		t.Errorf("unexpected session command: got %v, want %v", sessionCmd, expectedCmd)
	}
}

func TestDetachedRunWithoutCommandPreservesIdleKeeper(t *testing.T) {
	var createdSandbox bool
	var sandboxCmd []string
	var createdSession bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testSandboxesAPIPath:
			createdSandbox = true
			var req apiv1.CreateSandboxRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			sandboxCmd = req.Command
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "test-sbx-idle", Phase: "Running"})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/sessions"):
			createdSession = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldEndpoint := flagEndpoint
	flagEndpoint = server.URL
	defer func() { flagEndpoint = oldEndpoint }()

	cmd := newRunCmd()
	cmd.SetArgs([]string{"shell", "-d"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected detached run without command to succeed, got error: %v", err)
	}

	if !createdSandbox {
		t.Error("expected sandbox to be created")
	}
	if createdSession {
		t.Error("did not expect session to be created for headless detached run")
	}
	expectedIdleCmd := []string{"/bin/sh", "-c", "trap : TERM INT; sleep infinity & wait"}
	if len(sandboxCmd) != len(expectedIdleCmd) ||
		sandboxCmd[0] != expectedIdleCmd[0] ||
		sandboxCmd[2] != expectedIdleCmd[2] {
		t.Errorf("expected sandbox command to be idle keeper %v, got %v", expectedIdleCmd, sandboxCmd)
	}
}

func TestBuildRunConfigDetachedCommandSelection(t *testing.T) {
	preset := ResolvePresetOrImage("shell")

	// Detached without command: idle keeper
	reqIdle := buildRunConfig(preset, []string{"shell"}, true, "", "test-sbx",
		nil, "", "", 0, false, "", "", "", 0, "", "")
	expectedIdleCmd := []string{"/bin/sh", "-c", "trap : TERM INT; sleep infinity & wait"}
	if len(reqIdle.Command) != len(expectedIdleCmd) || reqIdle.Command[2] != expectedIdleCmd[2] {
		t.Errorf("expected idle keeper for detached run without task args, got: %v", reqIdle.Command)
	}

	// Detached with task command: preset command
	reqTask := buildRunConfig(preset, []string{"shell", "echo", "run-task"}, true, "", "test-sbx",
		nil, "", "", 0, false, "", "", "", 0, "", "")
	if len(reqTask.Command) != 1 || reqTask.Command[0] != "/bin/bash" {
		t.Errorf("expected preset command /bin/bash for detached run with task args, got: %v", reqTask.Command)
	}
	if len(reqTask.Args) != 2 || reqTask.Args[0] != "echo" || reqTask.Args[1] != "run-task" {
		t.Errorf("expected task args [echo run-task], got: %v", reqTask.Args)
	}
}
