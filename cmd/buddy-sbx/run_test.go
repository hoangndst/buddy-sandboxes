package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestBuildRunConfigRuntimeClass(t *testing.T) {
	preset := ResolvePresetOrImage("golang:1.25")

	// 1. Without runtime class
	req, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, false, "", "", "", 0, "", "")
	if req.RuntimeClassName != nil {
		t.Errorf("expected RuntimeClassName to be nil, got %v", *req.RuntimeClassName)
	}

	// 2. With runtime class gvisor
	reqGvisor, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, false, "gvisor", "", "", 0, "", "")
	if reqGvisor.RuntimeClassName == nil || *reqGvisor.RuntimeClassName != "gvisor" {
		t.Errorf("expected RuntimeClassName to be 'gvisor', got %v", reqGvisor.RuntimeClassName)
	}

	// 3. With runtime class kata
	reqKata, _ := buildRunConfig(preset, []string{"golang:1.25"}, false, "", "test-sbx",
		nil, "", "", 0, false, "kata-containers", "", "", 0, "", "")
	if reqKata.RuntimeClassName == nil || *reqKata.RuntimeClassName != "kata-containers" {
		t.Errorf("expected RuntimeClassName to be 'kata-containers', got %v", reqKata.RuntimeClassName)
	}

	// 4. With Git repository and PVC
	reqGit, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "", "", 0, false, "", "https://github.com/example/repo.git", "main", 1, "sub", "test-pvc")
	if reqGit.Workspace == nil {
		t.Fatalf("expected Workspace to be populated")
	}
	if reqGit.Workspace.Ephemeral {
		t.Errorf("expected Ephemeral to be false when PVC is set")
	}
	if reqGit.Workspace.Git == nil || reqGit.Workspace.Git.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected Git URL to match")
	}
	if reqGit.Workspace.PersistentVolumeClaim == nil || reqGit.Workspace.PersistentVolumeClaim.ClaimName != "test-pvc" {
		t.Errorf("expected PVC to match")
	}
}

func TestParseMCPServerFlag(t *testing.T) {
	// 1. Valid full specification
	flagVal := "name=filesystem,image=mcp/filesystem:latest,port=8080," +
		"mountWorkspace=true,mountWorkspaceReadOnly=true"
	spec, err := parseMCPServerFlag(flagVal)
	if err != nil {
		t.Fatalf("unexpected error parsing valid MCP server flag: %v", err)
	}
	if spec.Name != "filesystem" || spec.Image != "mcp/filesystem:latest" ||
		spec.Port != 8080 || !spec.MountWorkspace || !spec.MountWorkspaceReadOnly {
		t.Errorf("unexpected parsed spec: %+v", spec)
	}

	// 2. Valid minimal specification
	minimal, err := parseMCPServerFlag("name=db,image=redis:7-alpine,port=6379")
	if err != nil {
		t.Fatalf("unexpected error parsing minimal MCP server flag: %v", err)
	}
	if minimal.Name != "db" || minimal.Image != "redis:7-alpine" || minimal.Port != 6379 || minimal.MountWorkspace {
		t.Errorf("unexpected parsed minimal spec: %+v", minimal)
	}

	// 3. Missing name
	if _, err := parseMCPServerFlag("image=redis:7-alpine,port=6379"); err == nil {
		t.Errorf("expected error for missing name")
	}

	// 4. Missing image
	if _, err := parseMCPServerFlag("name=db,port=6379"); err == nil {
		t.Errorf("expected error for missing image")
	}

	// 5. Invalid port range
	if _, err := parseMCPServerFlag("name=db,image=redis,port=70000"); err == nil {
		t.Errorf("expected error for port > 65535")
	}
	if _, err := parseMCPServerFlag("name=db,image=redis,port=0"); err == nil {
		t.Errorf("expected error for port 0")
	}
	if _, err := parseMCPServerFlag("name=db,image=redis,port=invalid"); err == nil {
		t.Errorf("expected error for non-integer port")
	}

	// 6. Invalid mountWorkspace bool
	if _, err := parseMCPServerFlag("name=fs,image=mcp/fs,port=8080,mountWorkspace=notabool"); err == nil {
		t.Errorf("expected error for non-bool mountWorkspace")
	}

	// 7. Unknown key
	if _, err := parseMCPServerFlag("name=fs,image=mcp/fs,port=8080,unknownKey=val"); err == nil {
		t.Errorf("expected error for unknown key")
	}
}

func TestRunRejectsInvalidDuration(t *testing.T) {
	for _, test := range []struct{ input, message string }{
		{input: "-1s", message: "--duration cannot be negative"},
		{input: fmt.Sprintf("%ds", sandboxesv1alpha1.MaxActiveDeadlineSeconds+1), message: "--duration cannot exceed"},
	} {
		cmd := newRunCmd()
		cmd.SetArgs([]string{"golang:1.25", "--duration=" + test.input})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("expected duration error containing %q, got %v", test.message, err)
		}
	}
}

func TestBuildRunConfigProcessNamespaceAndDuration(t *testing.T) {
	preset := ResolvePresetOrImage("golang:1.25")
	build := func(duration time.Duration, share bool) *apiv1.CreateSandboxRequest {
		req, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
			nil, "", "", duration, share, "", "", "", 0, "", "")
		return req
	}

	if req := build(0, false); req.MaxDurationSeconds != nil || req.ShareProcessNamespace != nil {
		t.Fatalf("zero duration and default sharing should be omitted: %#v", req)
	}
	if got := *build(500*time.Millisecond, false).MaxDurationSeconds; got != 1 {
		t.Fatalf("500ms should round up to 1 second, got %d", got)
	}
	if got := *build(1100*time.Millisecond, false).MaxDurationSeconds; got != 2 {
		t.Fatalf("1.1s should round up to 2 seconds, got %d", got)
	}
	if got := *build(2*time.Second, false).MaxDurationSeconds; got != 2 {
		t.Fatalf("2s should remain 2 seconds, got %d", got)
	}
	if req := build(0, true); req.ShareProcessNamespace == nil || !*req.ShareProcessNamespace {
		t.Fatalf("explicit process sharing should be sent as true: %#v", req.ShareProcessNamespace)
	}
}

func TestRunCommandRuntimeFlags(t *testing.T) {
	cmd := newRunCmd()
	err := cmd.ParseFlags([]string{"--runtime-class", "gvisor", "golang:1.25"})
	if err != nil {
		t.Fatalf("unexpected error parsing flags: %v", err)
	}

	runtimeClassVal, err := cmd.Flags().GetString("runtime-class")
	if err != nil || runtimeClassVal != "gvisor" {
		t.Errorf("expected runtime-class flag to be 'gvisor', got %s, err: %v", runtimeClassVal, err)
	}

	cmdShare := newRunCmd()
	if err := cmdShare.ParseFlags([]string{"--share-process-namespace", "golang:1.25"}); err != nil {
		t.Fatalf("unexpected error parsing process namespace flag: %v", err)
	}
	shareValue, err := cmdShare.Flags().GetBool("share-process-namespace")
	if err != nil || !shareValue {
		t.Errorf("expected share-process-namespace flag true, got %v, err: %v", shareValue, err)
	}

	cmdRuntime := newRunCmd()
	err = cmdRuntime.ParseFlags([]string{"--runtime", "kata-containers", "golang:1.25"})
	if err != nil {
		t.Fatalf("unexpected error parsing --runtime alias: %v", err)
	}

	aliasVal, err := cmdRuntime.Flags().GetString("runtime")
	if err != nil || aliasVal != "kata-containers" {
		t.Errorf("expected runtime alias flag to be 'kata-containers', got %s, err: %v", aliasVal, err)
	}
}
