package main

import (
	"os"
	"testing"
)

func TestPresetResolution(t *testing.T) {
	tests := []struct {
		input         string
		expectedImage string
		expectedCmd   string
	}{
		{"claude", "ghcr.io/anthropics/claude-code:latest", "claude"},
		{"codex", "ghcr.io/hoangndst/buddy-sandboxes-codex:0.171.0", "codex"},
		{"aider", "paulgauthier/aider:latest", "aider"},
		{"shell", "ubuntu:24.04", "/bin/bash"},
		{"python:3.12", "python:3.12", ""},
		{"custom/agent:v1", "custom/agent:v1", ""},
	}

	for _, tc := range tests {
		p := ResolvePresetOrImage(tc.input)
		if p.Image != tc.expectedImage {
			t.Errorf("for input %q, expected image %q, got %q", tc.input, tc.expectedImage, p.Image)
		}
		if tc.expectedCmd != "" {
			if len(p.Command) == 0 || p.Command[0] != tc.expectedCmd {
				t.Errorf("for input %q, expected command %q, got %+v", tc.input, tc.expectedCmd, p.Command)
			}
		}
	}
}

func TestFilterWhitelistedEnvNeverForwardsProviderCredentials(t *testing.T) {
	// Setup test host environment
	_ = os.Setenv("ANTHROPIC_API_KEY", "test-anthropic-secret")
	_ = os.Setenv("OPENAI_API_KEY", "test-openai-secret")
	_ = os.Setenv("SENSITIVE_HOST_KEY", "must-not-leak")
	defer func() {
		_ = os.Unsetenv("ANTHROPIC_API_KEY")
		_ = os.Unsetenv("OPENAI_API_KEY")
		_ = os.Unsetenv("SENSITIVE_HOST_KEY")
	}()

	// Provider keys are stored by Buddy and are not read from the host environment.
	claudePreset := ResolvePresetOrImage("claude")
	envs, _ := FilterWhitelistedEnv(claudePreset, []string{"CUSTOM_USER_VAR=custom_val"})

	var hasAnthropic, hasOpenAI, hasSensitive, hasCustom bool
	for _, e := range envs {
		switch e.Name {
		case "ANTHROPIC_API_KEY":
			hasAnthropic = true
			if e.Value != "test-anthropic-secret" {
				t.Errorf("expected test-anthropic-secret, got %q", e.Value)
			}
		case "OPENAI_API_KEY":
			hasOpenAI = true
		case "SENSITIVE_HOST_KEY":
			hasSensitive = true
		case "CUSTOM_USER_VAR":
			hasCustom = true
			if e.Value != "custom_val" {
				t.Errorf("expected custom_val, got %q", e.Value)
			}
		}
	}

	if hasAnthropic {
		t.Error("ANTHROPIC_API_KEY leaked into sandbox environment")
	}
	if hasOpenAI {
		t.Error("OPENAI_API_KEY should NOT be forwarded for claude preset without explicit user flag")
	}
	if hasSensitive {
		t.Error("SENSITIVE_HOST_KEY leaked to sandbox environment")
	}
	if !hasCustom {
		t.Error("expected CUSTOM_USER_VAR to be present")
	}

	// Explicit provider-key flags are rejected by run validation and never make it through filtering.
	_ = os.Setenv("ANTHROPIC_API_KEY", "host-default-key")
	overrideEnvs, _ := FilterWhitelistedEnv(claudePreset, []string{"ANTHROPIC_API_KEY=explicit-user-key"})
	for _, env := range overrideEnvs {
		if env.Name == "ANTHROPIC_API_KEY" {
			t.Fatal("explicit provider key leaked into sandbox environment")
		}
	}
}

func TestParsePortSpec(t *testing.T) {
	tests := []struct {
		input     string
		wantLocal int
		wantRem   int
		wantErr   bool
	}{
		{"8080", 8080, 8080, false},
		{"3000:3000", 3000, 3000, false},
		{"127.0.0.1:8080", 0, 0, true},
		{"abc:8080", 0, 0, true},
		{"70000:80", 0, 0, true},
		{"-1", 0, 0, true},
	}

	for _, tc := range tests {
		l, r, err := parsePortSpec(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parsePortSpec(%q) unexpected err %v, wantErr %v", tc.input, err, tc.wantErr)
		}
		if !tc.wantErr {
			if l != tc.wantLocal || r != tc.wantRem {
				t.Errorf("parsePortSpec(%q) = (%d, %d), want (%d, %d)", tc.input, l, r, tc.wantLocal, tc.wantRem)
			}
		}
	}
}
