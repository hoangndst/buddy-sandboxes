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
		Name:        "claude",
		Image:       "ghcr.io/anthropics/claude-code:latest",
		Command:     []string{"claude"},
		RequiredEnv: []string{"ANTHROPIC_API_KEY"},
	},
	"codex": {
		Name:        "codex",
		Image:       "ghcr.io/openai/codex:latest",
		Command:     []string{"codex"},
		RequiredEnv: []string{"OPENAI_API_KEY"},
	},
	"aider": {
		Name:        "aider",
		Image:       "paulgauthier/aider:latest",
		Command:     []string{"aider"},
		RequiredEnv: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
	},
	"shell": {
		Name:        "shell",
		Image:       "ubuntu:24.04",
		Command:     []string{"/bin/bash"},
		RequiredEnv: nil,
	},
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

// FilterWhitelistedEnv applies the strict environment whitelisting security policy (DEC-005).
// It forwards host environment variables only if they match preset required keys or user -e flags.
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
	}

	// 2. Forward whitelisted preset credentials from host environment if not explicitly overridden
	for _, key := range preset.RequiredEnv {
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
