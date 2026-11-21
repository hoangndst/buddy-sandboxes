package main

import (
	"testing"
	"time"
)

func TestBuildRunConfigRuntimeClass(t *testing.T) {
	preset := ResolvePresetOrImage("golang:1.25")

	// 1. Without runtime class
	req, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, "")
	if req.RuntimeClassName != nil {
		t.Errorf("expected RuntimeClassName to be nil, got %v", *req.RuntimeClassName)
	}

	// 2. With runtime class gvisor
	reqGvisor, _ := buildRunConfig(preset, []string{"golang:1.25"}, true, "", "test-sbx",
		nil, "1", "1Gi", 30*time.Minute, "gvisor")
	if reqGvisor.RuntimeClassName == nil || *reqGvisor.RuntimeClassName != "gvisor" {
		t.Errorf("expected RuntimeClassName to be 'gvisor', got %v", reqGvisor.RuntimeClassName)
	}

	// 3. With runtime class kata
	reqKata, _ := buildRunConfig(preset, []string{"golang:1.25"}, false, "", "test-sbx",
		nil, "", "", 0, "kata-containers")
	if reqKata.RuntimeClassName == nil || *reqKata.RuntimeClassName != "kata-containers" {
		t.Errorf("expected RuntimeClassName to be 'kata-containers', got %v", reqKata.RuntimeClassName)
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
