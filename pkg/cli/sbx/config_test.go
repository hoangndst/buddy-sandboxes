package sbx

import (
	"os"
	"path/filepath"
	"testing"
)

const stagingContextName = "staging"

func TestConfigRoundtrip(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	dirInfoBefore, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("failed to stat tempDir before save: %v", err)
	}

	// 1. Loading nonexistent path returns default config
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if cfg.CurrentContext != defaultContextName {
		t.Fatalf("expected default context, got %q", cfg.CurrentContext)
	}

	// 2. Modify and save config
	cfg.CurrentContext = stagingContextName
	cfg.Contexts = append(cfg.Contexts, ContextConfig{
		Name:     stagingContextName,
		Endpoint: "https://staging-gateway.example.com",
		Token:    "secret-jwt",
	})

	if err := saveConfig(configPath, cfg); err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	// 3. Reload config and verify
	reloaded, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	if reloaded.CurrentContext != stagingContextName {
		t.Fatalf("expected 'staging', got %q", reloaded.CurrentContext)
	}

	ctx := reloaded.ActiveContext()
	if ctx == nil || ctx.Name != stagingContextName || ctx.Endpoint != "https://staging-gateway.example.com" {
		t.Fatalf("unexpected active context: %+v", ctx)
	}

	// 4. Verify custom parent directory permissions are unchanged
	dirInfo, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("failed to stat tempDir: %v", err)
	}
	if dirInfo.Mode().Perm() != dirInfoBefore.Mode().Perm() {
		t.Errorf("parent permissions changed: wanted %v, got %v", dirInfoBefore.Mode().Perm(), dirInfo.Mode().Perm())
	}

	// 5. GetContext
	missing := reloaded.GetContext("nonexistent")
	if missing != nil {
		t.Fatalf("expected nil for nonexistent context, got %+v", missing)
	}
}

func TestExitCodeError(t *testing.T) {
	err := ExitCodeError{Code: 42}
	if err.Code != 42 {
		t.Fatalf("expected 42, got %d", err.Code)
	}
	_ = err.Error()
}

func TestDefaultConfigPath(t *testing.T) {
	p := getDefaultConfigPath()
	if p == "" {
		t.Fatal("expected non-empty config path")
	}
}
