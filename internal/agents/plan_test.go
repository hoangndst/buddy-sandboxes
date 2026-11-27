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

	profile := &store.AgentProfile{
		ID:                         "prof-codex",
		Name:                       "Codex Default",
		HarnessType:                "codex",
		ModelName:                  "gpt-4o",
		SystemPrompt:               "Follow zero-downtime rules",
		VerificationCommand:        "go test -race ./...",
		MaxSteps:                   25,
		TimeoutSeconds:             1200,
		VerificationTimeoutSeconds: 180,
	}

	plan, err := Plan(task, profile, nil)
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
	if spec.SystemPrompt != "Follow zero-downtime rules" {
		t.Fatalf("unexpected system prompt: %q", spec.SystemPrompt)
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
}

func TestPlanExplicitHarnessImage(t *testing.T) {
	task := &store.Task{
		ID:            "task-plan-2",
		Namespace:     "default",
		WorkspaceName: "ws-custom",
		Prompt:        "Custom agent task",
	}

	profile := &store.AgentProfile{
		ID:          "prof-custom",
		HarnessType: "shell",
	}

	harness := &HarnessSpec{
		ID:          "shell-v1",
		Image:       "custom-registry.example.com/agents/shell:v1.2.3",
		Entrypoint:  []string{"/custom/entrypoint.sh"},
		SpecVersion: SpecVersion,
	}

	plan, err := Plan(task, profile, harness)
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
	profile := &store.AgentProfile{
		ID:          "prof-valid",
		HarnessType: "codex",
	}

	// Missing task
	if _, err := Plan(nil, profile, nil); err == nil {
		t.Fatal("expected error for nil task")
	}

	// Empty task ID
	task := &store.Task{ID: ""}
	if _, err := Plan(task, profile, nil); err == nil {
		t.Fatal("expected error for empty task ID")
	}

	// Missing profile
	task.ID = "task-valid"
	if _, err := Plan(task, nil, nil); err == nil {
		t.Fatal("expected error for nil profile")
	}

	// Invalid model name (e.g. leading flag injection)
	profileInvalidModel := &store.AgentProfile{
		ID:          "prof-bad",
		HarnessType: "codex",
		ModelName:   "-bad-model",
	}
	if _, err := Plan(task, profileInvalidModel, nil); err == nil {
		t.Fatal("expected error for invalid model name starting with '-'")
	}

	// Unknown harness type
	profileUnknown := &store.AgentProfile{
		ID:          "prof-unknown",
		HarnessType: "non-existent-harness",
	}
	if _, err := Plan(task, profileUnknown, nil); err == nil {
		t.Fatal("expected error for unknown harness type")
	}
}
