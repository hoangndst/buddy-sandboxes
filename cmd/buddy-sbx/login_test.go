package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestLoginCmdFlags(t *testing.T) {
	cmd := newLoginCmd()
	err := cmd.ParseFlags([]string{"--token", "my-secret-token", "--context", "prod", "http://localhost:8080"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	tok, _ := cmd.Flags().GetString("token")
	if tok != "my-secret-token" {
		t.Errorf("expected token to be 'my-secret-token', got %q", tok)
	}

	ctxFlag, _ := cmd.Flags().GetString("context")
	if ctxFlag != "prod" {
		t.Errorf("expected context to be 'prod', got %q", ctxFlag)
	}
}

func TestLoginCmdReadsPrivateTokenFile(t *testing.T) {
	const token = "a-long-random-token-that-is-never-printed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(apiv1.WhoAmIResponse{User: "developer", Namespace: "team-a"})
	}))
	defer server.Close()
	root := t.TempDir()
	tokenPath := filepath.Join(root, "api-token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	flagConfig = configPath
	defer func() { flagConfig = "" }()
	cmd := newLoginCmd()
	cmd.SetArgs([]string{"--token-file", tokenPath, server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	loaded, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if ctx := loaded.ActiveContext(); ctx == nil || ctx.Token != token {
		t.Fatalf("token file was not stored in config: %+v", ctx)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoginCmdExecution(t *testing.T) {
	// Mock whoami server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/whoami" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer valid-token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiv1.WhoAmIResponse{
			User:      "developer",
			Namespace: "team-a",
			Roles:     []string{"developer"},
			AuthMode:  "bearer-token",
		})
	}))
	defer server.Close()

	tempDir := t.TempDir()
	cfgFile := filepath.Join(tempDir, "config.yaml")
	flagConfig = cfgFile
	defer func() { flagConfig = "" }()

	cmd := newLoginCmd()
	cmd.SetArgs([]string{"--token", "valid-token", "--context", "team-a", server.URL})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("login execution failed: %v", err)
	}

	loaded, err := loadConfig(cfgFile)
	if err != nil {
		t.Fatalf("failed to load saved config: %v", err)
	}
	if loaded.CurrentContext != "team-a" {
		t.Errorf("expected current context 'team-a', got %q", loaded.CurrentContext)
	}
	ctx := loaded.GetContext("team-a")
	if ctx == nil {
		t.Fatalf("expected context 'team-a' in config")
	}
	if ctx.DefaultNamespace != "team-a" {
		t.Errorf("expected namespace 'team-a', got %q", ctx.DefaultNamespace)
	}
	if ctx.Token != "valid-token" {
		t.Errorf("expected token 'valid-token', got %q", ctx.Token)
	}
}
