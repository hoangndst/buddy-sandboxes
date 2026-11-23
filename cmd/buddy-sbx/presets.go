package main

import (
	"fmt"
	"os"
	"strings"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// Preset represents an agent or runtime execution preset.
type Preset struct {
	Name        string
	Image       string
	Command     []string
	RequiredEnv []string
}

var presetCatalog = map[string]Preset{
	"claude": {
		Name:    "claude",
		Image:   "ghcr.io/hoangndst/buddy-sandboxes-claude-code:2.1.330",
		Command: []string{"claude", "--dangerously-skip-permissions"},
	},
	"codex": {
		Name:    "codex",
		Image:   "ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0",
		Command: []string{"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox"},
	},
	"aider": {
		Name:    "aider",
		Image:   "paulgauthier/aider:latest",
		Command: []string{"aider"},
	},
	"shell": {
		Name:        "shell",
		Image:       "ubuntu:24.04",
		Command:     []string{"/bin/bash"},
		RequiredEnv: nil,
	},
}

// BuildAgentCommand constructs the executable command slice for an agent preset or custom image.
// It injects autonomous bypass flags, model overrides, and task prompt subcommands.
func BuildAgentCommand(preset Preset, taskArgs []string, model string, entrypoint string) []string {
	if entrypoint != "" {
		cmd := []string{entrypoint}
		return append(cmd, taskArgs...)
	}

	target := strings.ToLower(preset.Name)
	prompt := strings.Join(taskArgs, " ")

	switch target {
	case "codex":
		cmd := []string{"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox"}
		if model != "" {
			cmd = append(cmd, "--model", model)
		}
		if len(taskArgs) > 0 {
			if taskArgs[0] == "exec" {
				cmd = append(cmd, taskArgs...)
			} else {
				cmd = append(cmd, "exec", "--skip-git-repo-check", prompt)
			}
		}
		return cmd

	case "claude":
		cmd := []string{"claude", "--dangerously-skip-permissions"}
		if model != "" {
			cmd = append(cmd, "--model", model)
		}
		if len(taskArgs) > 0 {
			if taskArgs[0] == "-p" || taskArgs[0] == "--print" {
				cmd = append(cmd, taskArgs...)
			} else {
				cmd = append(cmd, "-p", prompt)
			}
		}
		return cmd

	default:
		var cmd []string
		if len(preset.Command) > 0 {
			cmd = append([]string(nil), preset.Command...)
		}
		if model != "" && target == "aider" {
			cmd = append(cmd, "--model", model)
		}
		if len(taskArgs) > 0 {
			cmd = append(cmd, taskArgs...)
		}
		return cmd
	}
}

// ResolvePresetOrImage resolves an alias to a catalog Preset or generates a generic image preset.
func ResolvePresetOrImage(target string) Preset {
	if p, ok := presetCatalog[strings.ToLower(target)]; ok {
		return p
	}
	return Preset{
		Name:        target,
		Image:       target,
		Command:     nil,
		RequiredEnv: nil,
	}
}

// FilterWhitelistedEnv applies the strict environment whitelist. Provider keys are never forwarded.
func FilterWhitelistedEnv(preset Preset, userEnvs []string) ([]apiv1.EnvVar, []string) {
	var envs []apiv1.EnvVar
	var warnings []string
	userExplicitMap := make(map[string]string)
	userExplicitKeys := make(map[string]bool)

	// 1. Parse user-specified -e flags (KEY=VAL or KEY)
	for _, entry := range userEnvs {
		if strings.Contains(entry, "=") {
			parts := strings.SplitN(entry, "=", 2)
			userExplicitMap[parts[0]] = parts[1]
			userExplicitKeys[parts[0]] = true
		} else {
			if val, exists := os.LookupEnv(entry); exists {
				userExplicitMap[entry] = val
			}
			userExplicitKeys[entry] = true
		}
		if partsName := strings.SplitN(entry, "=", 2)[0]; partsName == openAIAPIKeyEnv || partsName == anthropicAPIKeyEnv {
			warnings = append(warnings, fmt.Sprintf("%s is managed by Buddy and cannot be passed with --env", partsName))
			delete(userExplicitMap, partsName)
			userExplicitKeys[partsName] = true
		}
	}

	// 2. Forward whitelisted preset credentials from host environment if not explicitly overridden
	for _, key := range preset.RequiredEnv {
		if key == openAIAPIKeyEnv || key == anthropicAPIKeyEnv {
			continue
		}
		if userExplicitKeys[key] {
			continue
		}
		val, exists := os.LookupEnv(key)
		if exists && val != "" {
			envs = append(envs, apiv1.EnvVar{Name: key, Value: val})
		} else {
			msg := fmt.Sprintf("Preset %q expects environment variable %s, but it is not set on host.",
				preset.Name, key)
			warnings = append(warnings, msg)
		}
	}

	// 3. Append user explicit environment variables (preserving user order)
	seen := make(map[string]bool)
	for _, entry := range userEnvs {
		k := entry
		if strings.Contains(entry, "=") {
			k = strings.SplitN(entry, "=", 2)[0]
		}
		if !seen[k] {
			seen[k] = true
			if val, ok := userExplicitMap[k]; ok {
				envs = append(envs, apiv1.EnvVar{Name: k, Value: val})
			}
		}
	}

	return envs, warnings
}
