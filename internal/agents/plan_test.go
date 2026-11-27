package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

func TestPlanPureDeterministic(t *testing.T) {
	task := &store.Task{
		ID:            "task-plan-1",
		Namespace:     "tenant-dev",
		WorkspaceName: "repo-auth",
		Prompt:        "Fix authentication race condition",
	}

	agent := &Agent{
		ID:                         "prof-codex",
		Name:                       "Codex Default",
		HarnessType:                "codex",
		Model:                      "gpt-4o",
		SystemPrompt:               "Follow zero-downtime rules",
		VerificationCommand:        "go test -race ./...",
		MaxSteps:                   25,
		TimeoutSeconds:             1200,
		VerificationTimeoutSeconds: 180,
		Env: map[string]string{
			"GO111MODULE": "on",
		},
	}

	plan, err := Plan(task, agent, nil)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if plan.Image != DefaultAgentImageCodex {
		t.Fatalf("expected codex image %q, got %q", DefaultAgentImageCodex, plan.Image)
	}
	if !plan.RequiresWorkspace {
		t.Fatal("expected codex to require workspace")
	}
	if plan.TimeoutSeconds != 1200 {
		t.Fatalf("expected 1200s timeout, got %d", plan.TimeoutSeconds)
	}
	if plan.VerificationTimeoutSeconds != 180 {
		t.Fatalf("expected 180s verification timeout, got %d", plan.VerificationTimeoutSeconds)
	}

	// Verify command
	expectedCmdPrefix := "/opt/buddy/bin/buddy-runner agent-run"
	if strings.Join(plan.Command[:2], " ") != expectedCmdPrefix {
		t.Fatalf("expected command prefix %q, got %q", expectedCmdPrefix, strings.Join(plan.Command, " "))
	}

	// Parse generated task.json
	var spec TaskSpec
	if err := json.Unmarshal(plan.TaskJSON, &spec); err != nil {
		t.Fatalf("unmarshal taskJSON: %v", err)
	}
	if spec.SpecVersion != SpecVersion {
		t.Fatalf("expected spec version %q, got %q", SpecVersion, spec.SpecVersion)
	}
	if spec.TaskID != "task-plan-1" {
		t.Fatalf("expected task ID %q, got %q", "task-plan-1", spec.TaskID)
	}
	if spec.Prompt != "Fix authentication race condition" {
		t.Fatalf("unexpected prompt: %q", spec.Prompt)
	}
	if spec.PromptFile != DefaultPromptFile {
		t.Fatalf("expected prompt file %q, got %q", DefaultPromptFile, spec.PromptFile)
	}
	if spec.SystemPrompt != "Follow zero-downtime rules" {
		t.Fatalf("unexpected system prompt: %q", spec.SystemPrompt)
	}
	if spec.SystemPromptFile != DefaultSystemPromptFile {
		t.Fatalf("expected system prompt file %q, got %q", DefaultSystemPromptFile, spec.SystemPromptFile)
	}
	if spec.MaxSteps != 25 {
		t.Fatalf("expected 25 max steps, got %d", spec.MaxSteps)
	}
	if spec.Model != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %q", spec.Model)
	}
	if spec.Verify == nil || spec.Verify.Command != "go test -race ./..." || spec.Verify.Timeout != 180 {
		t.Fatalf("unexpected verify spec: %+v", spec.Verify)
	}
	if spec.Env["GO111MODULE"] != "on" {
		t.Fatalf("expected env GO111MODULE=on, got: %v", spec.Env)
	}
}

func TestPlanModelAllowlist(t *testing.T) {
	task := &store.Task{
		ID:            "task-allowlist",
		WorkspaceName: "ws-test",
		Prompt:        "Test model allowlist",
	}

	// Model allowed
	agentAllowed := &Agent{
		HarnessType:    "claude",
		Model:          "claude-3-5-sonnet-20241022",
		ModelAllowlist: []string{"claude-3-5-sonnet-20241022", "claude-3-7-sonnet"},
	}
	if _, err := Plan(task, agentAllowed, nil); err != nil {
		t.Fatalf("expected allowed model to succeed, got: %v", err)
	}

	// Model forbidden
	agentForbidden := &Agent{
		HarnessType:    "claude",
		Model:          "claude-opus-4",
		ModelAllowlist: []string{"claude-3-5-sonnet-20241022", "claude-3-7-sonnet"},
	}
	if _, err := Plan(task, agentForbidden, nil); err == nil {
		t.Fatal("expected error for model not in allowlist")
	}
}

func TestPlanRejectsEmptyOrDefaultHarness(t *testing.T) {
	task := &store.Task{
		ID:            "task-empty-harness",
		WorkspaceName: "ws-test",
		Prompt:        "Test empty harness",
	}

	// Empty harness
	if _, err := Plan(task, &Agent{HarnessType: ""}, nil); err == nil {
		t.Fatal("expected error for empty harness type")
	}

	// Default harness (must not run prompt as shell command)
	if _, err := Plan(task, &Agent{HarnessType: "default"}, nil); err == nil {
		t.Fatal("expected error for default harness type")
	}
}

func TestGoldenTaskAndResultJSON(t *testing.T) {
	task := &store.Task{
		ID:            "task-golden",
		WorkspaceName: "ws-golden",
		Prompt:        "Fix issue #42",
	}
	exec := &store.Execution{
		ID:            "exec-golden-1",
		AttemptNumber: 1,
	}
	targetBranch := "feature/fix-42"
	task.TargetBranch = &targetBranch

	agent := &Agent{
		ID:                  "prof-golden",
		HarnessType:         "codex",
		Model:               "o3-mini",
		VerificationCommand: "make test",
		Env: map[string]string{
			"CI": "true",
		},
	}

	plan, err := PlanWithExecution(task, exec, agent, nil)
	if err != nil {
		t.Fatalf("PlanWithExecution failed: %v", err)
	}

	var parsedTask TaskSpec
	if err := json.Unmarshal(plan.TaskJSON, &parsedTask); err != nil {
		t.Fatalf("unmarshal task JSON: %v", err)
	}
	if parsedTask.ExecutionID != "exec-golden-1" {
		t.Fatalf("expected executionId 'exec-golden-1', got %q", parsedTask.ExecutionID)
	}
	if parsedTask.Attempt != 1 {
		t.Fatalf("expected attempt 1, got %d", parsedTask.Attempt)
	}
	if parsedTask.TargetBranch != "feature/fix-42" {
		t.Fatalf("expected targetBranch 'feature/fix-42', got %q", parsedTask.TargetBranch)
	}

	// Verify ResultSpec format with reserved fields
	passed := true
	res := ResultSpec{
		SpecVersion: SpecVersion,
		Status:      "succeeded",
		ExitCode:    0,
		Summary:     "All tests passed",
		ProposedMemories: []ProposedMemory{
			{Key: "test_key", Content: "test content"},
		},
		Verification: &VerificationData{
			Passed:   passed,
			ExitCode: 0,
			Output:   "PASS\n",
		},
	}
	resJSON, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result JSON: %v", err)
	}
	var unmarshaledRes ResultSpec
	if err := json.Unmarshal(resJSON, &unmarshaledRes); err != nil {
		t.Fatalf("unmarshal result JSON: %v", err)
	}
	if unmarshaledRes.Summary != "All tests passed" {
		t.Fatalf("unexpected summary: %q", unmarshaledRes.Summary)
	}
	if len(unmarshaledRes.ProposedMemories) != 1 || unmarshaledRes.ProposedMemories[0].Key != "test_key" {
		t.Fatalf("unexpected proposed memories: %+v", unmarshaledRes.ProposedMemories)
	}
}

func TestPlanExplicitHarnessImage(t *testing.T) {
	task := &store.Task{
		ID:            "task-plan-2",
		Namespace:     "default",
		WorkspaceName: "ws-custom",
		Prompt:        "Custom agent task",
	}

	agent := &Agent{
		ID:          "prof-custom",
		HarnessType: "shell",
	}

	harness := &HarnessSpec{
		ID:          "shell-v1",
		Image:       "custom-registry.example.com/agents/shell:v1.2.3",
		Entrypoint:  []string{"/custom/entrypoint.sh"},
		SpecVersion: SpecVersion,
	}

	plan, err := Plan(task, agent, harness)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if plan.Image != harness.Image {
		t.Fatalf("expected harness image %q, got %q", harness.Image, plan.Image)
	}
	if plan.RequiresWorkspace {
		t.Fatal("expected shell harness to not strictly require workspace")
	}

	cmdStr := strings.Join(plan.Command, " ")
	if !strings.Contains(cmdStr, "--entrypoint /custom/entrypoint.sh") {
		t.Fatalf("expected custom entrypoint flag, got %q", cmdStr)
	}
}

func TestPlanValidation(t *testing.T) {
	agent := &Agent{
		ID:          "prof-valid",
		HarnessType: "codex",
	}

	// Missing task
	if _, err := Plan(nil, agent, nil); err == nil {
		t.Fatal("expected error for nil task")
	}

	// Empty task ID
	task := &store.Task{ID: ""}
	if _, err := Plan(task, agent, nil); err == nil {
		t.Fatal("expected error for empty task ID")
	}

	// Missing agent
	task.ID = "task-valid"
	if _, err := Plan(task, nil, nil); err == nil {
		t.Fatal("expected error for nil agent")
	}

	// Invalid model name (e.g. leading flag injection)
	agentInvalidModel := &Agent{
		ID:          "prof-bad",
		HarnessType: "codex",
		Model:       "-bad-model",
	}
	if _, err := Plan(task, agentInvalidModel, nil); err == nil {
		t.Fatal("expected error for invalid model name starting with '-'")
	}

	// Unknown harness type
	agentUnknown := &Agent{
		ID:          "prof-unknown",
		HarnessType: "non-existent-harness",
	}
	if _, err := Plan(task, agentUnknown, nil); err == nil {
		t.Fatal("expected error for unknown harness type")
	}
}
