package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hoangndst/buddy-sandboxes/internal/agents"
)

func TestWriteFileSubcommand(t *testing.T) {
	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "sub", "test.txt")

	// Test writing inline content
	err := runWriteFile([]string{"--content", "hello buddy-runner", targetFile})
	if err != nil {
		t.Fatalf("runWriteFile failed: %v", err)
	}

	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if string(content) != "hello buddy-runner" {
		t.Fatalf("expected 'hello buddy-runner', got %q", string(content))
	}
}

func TestAgentRunVerificationSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	harnessScript := filepath.Join(tmpDir, "harness.sh")

	// Create dummy harness script that creates a file
	err := os.WriteFile(harnessScript, []byte("#!/bin/sh\necho 'harness completed'\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create task.json with a passing verification command
	taskSpec := agents.TaskSpec{
		SpecVersion: agents.SpecVersion,
		TaskID:      "task-test-1",
		Prompt:      "run verification test",
		Verify: &agents.VerifySpec{
			Command: "echo 'verification passed'",
			Timeout: 10,
		},
	}
	taskData, _ := json.Marshal(taskSpec)
	if err := os.WriteFile(taskFile, taskData, 0644); err != nil {
		t.Fatal(err)
	}

	err = runAgentRun([]string{
		"--task-file", taskFile,
		"--result-file", resultFile,
		"--entrypoint", harnessScript,
		"--workdir", tmpDir,
	})
	if err != nil {
		t.Fatalf("runAgentRun failed: %v", err)
	}

	// Verify result.json
	resData, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var res agents.ResultSpec
	if err := json.Unmarshal(resData, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("expected status succeeded, got %q", res.Status)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
	if res.Verification == nil || !res.Verification.Passed {
		t.Fatalf("expected verification to pass: %+v", res.Verification)
	}
}

func TestAgentRunVerificationFailsHarnessSucceeded(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	harnessScript := filepath.Join(tmpDir, "harness.sh")

	// Dummy harness exits 0
	err := os.WriteFile(harnessScript, []byte("#!/bin/sh\nexit 0\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Verification command exits 1 (failure)
	taskSpec := agents.TaskSpec{
		SpecVersion: agents.SpecVersion,
		TaskID:      "task-test-2",
		Prompt:      "failing verify test",
		Verify: &agents.VerifySpec{
			Command: "echo 'test failure' >&2 && exit 1",
			Timeout: 10,
		},
	}
	taskData, _ := json.Marshal(taskSpec)
	if err := os.WriteFile(taskFile, taskData, 0644); err != nil {
		t.Fatal(err)
	}

	err = runAgentRun([]string{
		"--task-file", taskFile,
		"--result-file", resultFile,
		"--entrypoint", harnessScript,
		"--workdir", tmpDir,
	})
	// Supervisor must return exit error matching verification exit code
	if err == nil {
		t.Fatal("expected runAgentRun to fail due to verification gate failure")
	}

	resData, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var res agents.ResultSpec
	if err := json.Unmarshal(resData, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("expected status failed when verification gate fails, got %q", res.Status)
	}
	if res.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", res.ExitCode)
	}
	if res.Verification == nil || res.Verification.Passed {
		t.Fatalf("expected verification to be failed: %+v", res.Verification)
	}
}
