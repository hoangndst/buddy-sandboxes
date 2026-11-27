package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

const (
	// SpecVersion is the current protocol version for agent contracts.
	SpecVersion = "buddy.agent/v1"

	DefaultTaskFile   = "/run/buddy/task.json"
	DefaultResultFile = "/run/buddy/result.json"
	DefaultEntrypoint = "/etc/buddy/agent/run.sh"

	HarnessCodex      = "codex"
	HarnessClaude     = "claude"
	HarnessClaudeCode = "claude-code"
	HarnessShell      = "shell"
	HarnessCustom     = "custom"
	HarnessDefault    = "default"

	DefaultAgentImageBase   = "buddy-sandboxes-base:local"
	DefaultAgentImageCodex  = "buddy-sandboxes-codex:local"
	DefaultAgentImageClaude = "buddy-sandboxes-claude-code:local"
)

// TaskSpec represents the input contract payload delivered at /run/buddy/task.json.
type TaskSpec struct {
	SpecVersion  string            `json:"spec_version"`
	TaskID       string            `json:"task_id"`
	Prompt       string            `json:"prompt"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	MaxSteps     int               `json:"max_steps,omitempty"`
	Model        string            `json:"model,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Verify       *VerifySpec       `json:"verify,omitempty"`
	Context      map[string]string `json:"context,omitempty"`
}

// VerifySpec specifies the deterministic verification gate executed after harness exit.
type VerifySpec struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// ResultSpec represents the execution result record written to /run/buddy/result.json.
type ResultSpec struct {
	SpecVersion      string            `json:"spec_version"`
	Status           string            `json:"status"` // succeeded, failed
	ExitCode         int               `json:"exit_code"`
	Summary          string            `json:"summary,omitempty"`
	ProposedMemories []ProposedMemory  `json:"proposed_memories,omitempty"`
	Verification     *VerificationData `json:"verification,omitempty"`
}

// ProposedMemory holds proposed durable learnings emitted by the agent.
type ProposedMemory struct {
	Key     string `json:"key"`
	Content string `json:"content"`
}

// VerificationData records post-harness verification gate outcomes.
type VerificationData struct {
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

// HarnessSpec describes harness image and execution metadata.
type HarnessSpec struct {
	ID          string   `json:"id"`
	Image       string   `json:"image"`
	Entrypoint  []string `json:"entrypoint,omitempty"`
	SpecVersion string   `json:"spec_version"`
}

// RunPlan is the immutable, pure specification describing sandbox execution parameters.
type RunPlan struct {
	Image                      string
	Command                    []string
	TaskJSON                   []byte
	RequiresWorkspace          bool
	TimeoutSeconds             int
	VerificationTimeoutSeconds int
}

// Plan computes a pure, deterministic RunPlan from task, agent profile, and optional harness.
// It performs zero disk I/O, zero network calls, and zero environment lookups.
func Plan(task *store.Task, profile *store.AgentProfile, harness *HarnessSpec) (RunPlan, error) {
	if task == nil {
		return RunPlan{}, errors.New("task is required")
	}
	if task.ID == "" {
		return RunPlan{}, errors.New("task ID cannot be empty")
	}
	if profile == nil {
		return RunPlan{}, errors.New("agent profile is required")
	}

	if profile.ModelName != "" {
		if err := credentials.ValidateModelName(profile.ModelName); err != nil {
			return RunPlan{}, fmt.Errorf("invalid model name %q: %w", profile.ModelName, err)
		}
	}

	harnessType := strings.ToLower(strings.TrimSpace(profile.HarnessType))
	if harnessType == "" {
		harnessType = HarnessDefault
	}

	image := ""
	if harness != nil && harness.Image != "" {
		image = harness.Image
	} else {
		switch harnessType {
		case HarnessCodex:
			image = DefaultAgentImageCodex
		case HarnessClaude, HarnessClaudeCode:
			image = DefaultAgentImageClaude
		case HarnessShell, HarnessCustom, HarnessDefault:
			image = DefaultAgentImageBase
		default:
			return RunPlan{}, fmt.Errorf("unknown or unsupported harness type %q", profile.HarnessType)
		}
	}

	requiresWorkspace := false
	switch harnessType {
	case HarnessCodex, HarnessClaude, HarnessClaudeCode:
		requiresWorkspace = true
	}

	timeoutSeconds := profile.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 1800
	}

	verificationTimeoutSeconds := profile.VerificationTimeoutSeconds
	if verificationTimeoutSeconds <= 0 {
		verificationTimeoutSeconds = 300
	}

	taskSpec := TaskSpec{
		SpecVersion:  SpecVersion,
		TaskID:       task.ID,
		Prompt:       task.Prompt,
		SystemPrompt: profile.SystemPrompt,
		MaxSteps:     profile.MaxSteps,
		Model:        profile.ModelName,
		Context: map[string]string{
			"namespace":      task.Namespace,
			"workspace_name": task.WorkspaceName,
		},
	}

	if strings.TrimSpace(profile.VerificationCommand) != "" {
		taskSpec.Verify = &VerifySpec{
			Command: strings.TrimSpace(profile.VerificationCommand),
			Timeout: verificationTimeoutSeconds,
		}
	}

	taskJSON, err := json.MarshalIndent(taskSpec, "", "  ")
	if err != nil {
		return RunPlan{}, fmt.Errorf("marshal task spec: %w", err)
	}

	cmd := []string{
		"/opt/buddy/bin/buddy-runner", "agent-run",
		"--task-file", DefaultTaskFile,
		"--result-file", DefaultResultFile,
	}
	if harness != nil && len(harness.Entrypoint) > 0 {
		cmd = append(cmd, "--entrypoint", strings.Join(harness.Entrypoint, " "))
	}

	return RunPlan{
		Image:                      image,
		Command:                    cmd,
		TaskJSON:                   taskJSON,
		RequiresWorkspace:          requiresWorkspace,
		TimeoutSeconds:             timeoutSeconds,
		VerificationTimeoutSeconds: verificationTimeoutSeconds,
	}, nil
}
