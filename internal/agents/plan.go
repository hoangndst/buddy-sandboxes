package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

const (
	SpecVersion = "buddy.agent/v1"

	DefaultTaskFile         = "/run/buddy/task.json"
	DefaultResultFile       = "/run/buddy/result.json"
	DefaultPromptFile       = "/run/buddy/prompt.txt"
	DefaultSystemPromptFile = "/run/buddy/system-prompt.txt"
	DefaultEntrypoint       = "/etc/buddy/agent/run.sh"

	HarnessCodex      = "codex"
	HarnessClaude     = "claude"
	HarnessClaudeCode = "claude-code"
	HarnessShell      = "shell"
	HarnessCustom     = "custom"

	DefaultAgentImageBase   = "buddy-sandboxes-base:local"
	DefaultAgentImageCodex  = "buddy-sandboxes-codex:local"
	DefaultAgentImageClaude = "buddy-sandboxes-claude-code:local"
)

// TaskSpec represents the input contract payload delivered at /run/buddy/task.json.
type TaskSpec struct {
	SpecVersion      string            `json:"spec_version"`
	TaskID           string            `json:"task_id"`
	ExecutionID      string            `json:"execution_id,omitempty"`
	Attempt          int               `json:"attempt,omitempty"`
	TargetBranch     string            `json:"target_branch,omitempty"`
	Prompt           string            `json:"prompt"`
	SystemPrompt     string            `json:"system_prompt,omitempty"`
	PromptFile       string            `json:"prompt_file,omitempty"`
	SystemPromptFile string            `json:"system_prompt_file,omitempty"`
	MaxSteps         int               `json:"max_steps,omitempty"`
	Model            string            `json:"model,omitempty"`
	Provider         string            `json:"provider,omitempty"`
	Verify           *VerifySpec       `json:"verify,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Context          map[string]string `json:"context,omitempty"`
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

// HarnessFromEnv resolves a HarnessSpec using environment overrides if configured.
func HarnessFromEnv(harnessType string) *HarnessSpec {
	norm := strings.ToLower(strings.TrimSpace(harnessType))
	var envImg string
	switch norm {
	case HarnessCodex:
		envImg = os.Getenv("BUDDY_AGENT_IMAGE_CODEX")
	case HarnessClaude, HarnessClaudeCode:
		envImg = os.Getenv("BUDDY_AGENT_IMAGE_CLAUDE")
	case HarnessShell, HarnessCustom:
		envImg = os.Getenv("BUDDY_AGENT_IMAGE_BASE")
	}
	if envImg == "" {
		envImg = os.Getenv("BUDDY_AGENT_IMAGE_BASE")
	}
	if envImg == "" {
		return nil
	}
	return &HarnessSpec{
		ID:          norm,
		Image:       envImg,
		SpecVersion: SpecVersion,
	}
}

// Agent represents the agent execution profile.
type Agent struct {
	ID                         string            `json:"id"`
	Name                       string            `json:"name"`
	HarnessType                string            `json:"harnessType"`
	Model                      string            `json:"model"`
	ModelAllowlist             []string          `json:"modelAllowlist,omitempty"`
	SystemPrompt               string            `json:"systemPrompt,omitempty"`
	MaxSteps                   int               `json:"maxSteps,omitempty"`
	VerificationCommand        string            `json:"verificationCommand,omitempty"`
	VerificationTimeoutSeconds int               `json:"verificationTimeoutSeconds,omitempty"`
	TimeoutSeconds             int               `json:"timeoutSeconds,omitempty"`
	Env                        map[string]string `json:"env,omitempty"`
}

// AgentFromProfile converts a store.AgentProfile into an Agent model.
func AgentFromProfile(p *store.AgentProfile) *Agent {
	if p == nil {
		return nil
	}
	return &Agent{
		ID:                         p.ID,
		Name:                       p.Name,
		HarnessType:                p.HarnessType,
		Model:                      p.ModelName,
		SystemPrompt:               p.SystemPrompt,
		MaxSteps:                   p.MaxSteps,
		VerificationCommand:        p.VerificationCommand,
		VerificationTimeoutSeconds: p.VerificationTimeoutSeconds,
		TimeoutSeconds:             p.TimeoutSeconds,
	}
}

// RunPlan is the immutable, pure specification describing sandbox execution parameters.
type RunPlan struct {
	Image                      string
	Provider                   string
	Command                    []string
	TaskJSON                   []byte
	PromptText                 []byte
	SystemPromptText           []byte
	RequiresWorkspace          bool
	TimeoutSeconds             int
	VerificationTimeoutSeconds int
	Env                        map[string]string
}

// Plan computes a pure, deterministic RunPlan from task, agent, and optional harness.
// It performs zero disk I/O, zero network calls, and zero environment lookups.
func Plan(task *store.Task, agent *Agent, harness *HarnessSpec) (RunPlan, error) {
	return PlanWithExecution(task, nil, agent, harness)
}

func resolveHarnessParameters(agent *Agent, harness *HarnessSpec) (string, string, bool, error) {
	harnessType := strings.ToLower(strings.TrimSpace(agent.HarnessType))
	if harnessType == "" {
		return "", "", false, errors.New("harness type is required")
	}

	var image string
	var provider string
	requiresWorkspace := false

	if harness != nil && harness.Image != "" {
		image = harness.Image
	}

	switch harnessType {
	case HarnessCodex:
		if image == "" {
			image = DefaultAgentImageCodex
		}
		provider = credentials.ServiceOpenAI
		requiresWorkspace = true
	case HarnessClaude, HarnessClaudeCode:
		if image == "" {
			image = DefaultAgentImageClaude
		}
		provider = credentials.ServiceAnthropic
		requiresWorkspace = true
	case HarnessShell, HarnessCustom:
		if image == "" {
			image = DefaultAgentImageBase
		}
		provider = ""
		requiresWorkspace = false
	default:
		return "", "", false, fmt.Errorf("unknown or unsupported harness type %q", agent.HarnessType)
	}

	return image, provider, requiresWorkspace, nil
}

// PlanWithExecution computes a pure RunPlan incorporating execution attempt metadata.
func PlanWithExecution(task *store.Task, exec *store.Execution, agent *Agent, harness *HarnessSpec) (RunPlan, error) {
	if task == nil {
		return RunPlan{}, errors.New("task is required")
	}
	if task.ID == "" {
		return RunPlan{}, errors.New("task ID cannot be empty")
	}
	if agent == nil {
		return RunPlan{}, errors.New("agent is required")
	}

	if agent.Model != "" {
		if err := credentials.ValidateModelName(agent.Model); err != nil {
			return RunPlan{}, fmt.Errorf("invalid model name %q: %w", agent.Model, err)
		}
	}

	if len(agent.ModelAllowlist) > 0 {
		allowed := false
		for _, m := range agent.ModelAllowlist {
			if m == agent.Model {
				allowed = true
				break
			}
		}
		if !allowed {
			return RunPlan{}, fmt.Errorf("model %q is not in allowed models list: %v", agent.Model, agent.ModelAllowlist)
		}
	}

	image, provider, requiresWorkspace, err := resolveHarnessParameters(agent, harness)
	if err != nil {
		return RunPlan{}, err
	}

	timeoutSeconds := agent.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 1800
	}
	verifyTimeoutSeconds := agent.VerificationTimeoutSeconds
	if verifyTimeoutSeconds <= 0 {
		verifyTimeoutSeconds = 300
	}

	maxSteps := agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 30
	}

	contextMap := make(map[string]string)
	if task.WorkspaceName != "" {
		contextMap["workspace"] = task.WorkspaceName
	}

	var verifySpec *VerifySpec
	if strings.TrimSpace(agent.VerificationCommand) != "" {
		verifySpec = &VerifySpec{
			Command: strings.TrimSpace(agent.VerificationCommand),
			Timeout: verifyTimeoutSeconds,
		}
	}

	sanitizedEnv := sanitizeAgentEnv(agent.Env)

	taskSpec := TaskSpec{
		SpecVersion: SpecVersion,
		TaskID:      task.ID,
		Prompt:      task.Prompt,
		PromptFile:  DefaultPromptFile,
		MaxSteps:    maxSteps,
		Model:       agent.Model,
		Provider:    provider,
		Verify:      verifySpec,
		Env:         sanitizedEnv,
		Context:     contextMap,
	}

	if task.TargetBranch != nil {
		taskSpec.TargetBranch = *task.TargetBranch
	}
	if exec != nil {
		taskSpec.ExecutionID = exec.ID
		taskSpec.Attempt = exec.AttemptNumber
	}
	if strings.TrimSpace(agent.SystemPrompt) != "" {
		taskSpec.SystemPrompt = agent.SystemPrompt
		taskSpec.SystemPromptFile = DefaultSystemPromptFile
	}

	taskJSON, err := json.MarshalIndent(taskSpec, "", "  ")
	if err != nil {
		return RunPlan{}, fmt.Errorf("marshal task spec: %w", err)
	}

	entrypoint := DefaultEntrypoint
	if harness != nil && len(harness.Entrypoint) > 0 {
		entrypoint = strings.Join(harness.Entrypoint, " ")
	}

	command := []string{
		"/opt/buddy/bin/buddy-runner",
		"agent-run",
		"--task-file", DefaultTaskFile,
		"--result-file", DefaultResultFile,
		"--entrypoint", entrypoint,
	}

	var promptBytes []byte
	if task.Prompt != "" {
		promptBytes = []byte(task.Prompt)
	}
	var sysPromptBytes []byte
	if agent.SystemPrompt != "" {
		sysPromptBytes = []byte(agent.SystemPrompt)
	}

	return RunPlan{
		Image:                      image,
		Provider:                   provider,
		Command:                    command,
		TaskJSON:                   taskJSON,
		PromptText:                 promptBytes,
		SystemPromptText:           sysPromptBytes,
		RequiresWorkspace:          requiresWorkspace,
		TimeoutSeconds:             timeoutSeconds,
		VerificationTimeoutSeconds: verifyTimeoutSeconds,
		Env:                        sanitizedEnv,
	}, nil
}

func sanitizeAgentEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		upper := strings.ToUpper(strings.TrimSpace(k))
		if upper == "PATH" || upper == "HOME" || upper == "USER" || upper == "SHELL" {
			continue
		}
		if strings.HasPrefix(upper, "BUDDY_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "ANTHROPIC_") {
			continue
		}
		out[k] = v
	}
	return out
}
