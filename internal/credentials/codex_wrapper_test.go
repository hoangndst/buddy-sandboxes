package credentials

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexWrapperModelReplacement(t *testing.T) {
	wrapperPath, err := filepath.Abs("../../images/agent-codex/codex-wrapper.sh")
	if err != nil {
		t.Fatalf("failed to resolve wrapper path: %v", err)
	}
	if _, err := os.Stat(wrapperPath); err != nil {
		t.Fatalf("wrapper script not found at %s: %v", wrapperPath, err)
	}

	testModels := []struct {
		name     string
		model    string
		expected string
	}{
		{
			name:     "standard model identifier",
			model:    "gpt-4o",
			expected: "model = \"gpt-4o\"",
		},
		{
			name:     "namespaced slash model identifier",
			model:    "deepseek/deepseek-chat",
			expected: "model = \"deepseek/deepseek-chat\"",
		},
		{
			name:     "namespaced model with tag and colon",
			model:    "deepseek/deepseek-r1:70b",
			expected: "model = \"deepseek/deepseek-r1:70b\"",
		},
		{
			name:     "namespaced meta-llama model",
			model:    "meta-llama/llama-3.3-70b-instruct",
			expected: "model = \"meta-llama/llama-3.3-70b-instruct\"",
		},
		{
			name:     "multi-level namespaced model",
			model:    "custom/suborg/model-v1:beta",
			expected: "model = \"custom/suborg/model-v1:beta\"",
		},
	}

	for _, tc := range testModels {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configSrc := filepath.Join(tmpDir, "template-config.toml")
			templateContent := "model = \"placeholder\"\nmodel_provider = \"buddy\"\n"
			if err := os.WriteFile(configSrc, []byte(templateContent), 0600); err != nil {
				t.Fatalf("failed to write template config: %v", err)
			}

			cmd := exec.Command("bash", wrapperPath)
			cmd.Env = append(
				os.Environ(),
				"HOME="+tmpDir,
				"BUDDY_PROVIDER_MODE=broker",
				"BUDDY_CODEX_CONFIG_PATH="+configSrc,
				"BUDDY_CODEX_SKIP_EXEC=1",
				"OPENAI_MODEL="+tc.model,
			)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("wrapper execution failed for model %q: %v\noutput: %s", tc.model, err, string(out))
			}

			destConfig := filepath.Join(tmpDir, ".codex", "config.toml")
			data, err := os.ReadFile(destConfig)
			if err != nil {
				t.Fatalf("failed to read generated config: %v", err)
			}

			content := string(data)
			if !strings.Contains(content, tc.expected) {
				t.Errorf("generated config for model %q does not contain %q\nactual content:\n%s", tc.model, tc.expected, content)
			}
		})
	}
}

func TestCodexWrapperUnsetOpenAIModel(t *testing.T) {
	wrapperPath, err := filepath.Abs("../../images/agent-codex/codex-wrapper.sh")
	if err != nil {
		t.Fatalf("failed to resolve wrapper path: %v", err)
	}

	tmpDir := t.TempDir()
	configSrc := filepath.Join(tmpDir, "template-config.toml")
	templateContent := "model = \"original-default\"\n"
	if err := os.WriteFile(configSrc, []byte(templateContent), 0600); err != nil {
		t.Fatalf("failed to write template config: %v", err)
	}

	cmd := exec.Command("bash", wrapperPath)
	cmd.Env = append(
		os.Environ(),
		"HOME="+tmpDir,
		"BUDDY_PROVIDER_MODE=broker",
		"BUDDY_CODEX_CONFIG_PATH="+configSrc,
		"BUDDY_CODEX_SKIP_EXEC=1",
		"OPENAI_MODEL=",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper execution failed with empty OPENAI_MODEL: %v\noutput: %s", err, string(out))
	}

	destConfig := filepath.Join(tmpDir, ".codex", "config.toml")
	data, err := os.ReadFile(destConfig)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "model = \"original-default\"") {
		t.Errorf("expected copied config to retain original model, got:\n%s", content)
	}
}
