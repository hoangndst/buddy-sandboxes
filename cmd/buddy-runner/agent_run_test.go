package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/agents"
)

func TestWriteFileSubcommand(t *testing.T) {
	tmpDir := t.TempDir()
	origPrefix := allowedWritePrefix
	allowedWritePrefix = tmpDir
	defer func() { allowedWritePrefix = origPrefix }()

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

	fi, err := os.Stat(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("expected mode 0600, got %o", fi.Mode().Perm())
	}
}

func TestWriteFilePathTraversalRejected(t *testing.T) {
	tmpDir := t.TempDir()
	origPrefix := allowedWritePrefix
	allowedWritePrefix = tmpDir
	defer func() { allowedWritePrefix = origPrefix }()

	// Try writing outside allowed prefix
	escapedFile := filepath.Join(tmpDir, "..", "escape.txt")
	err := runWriteFile([]string{"--content", "evil", escapedFile})
	if err == nil || !strings.Contains(err.Error(), "path traversal forbidden") {
		t.Fatalf("expected path traversal forbidden error, got: %v", err)
	}

	// Absolute path outside allowed prefix
	err = runWriteFile([]string{"--content", "evil", "/etc/passwd"})
	if err == nil || !strings.Contains(err.Error(), "path traversal forbidden") {
		t.Fatalf("expected path traversal forbidden error for /etc/passwd, got: %v", err)
	}
}

func TestAgentRunVerificationSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	harnessScript := filepath.Join(tmpDir, "harness.sh")

	err := os.WriteFile(harnessScript, []byte("#!/bin/sh\necho 'harness completed'\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

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

	err := os.WriteFile(harnessScript, []byte("#!/bin/sh\nexit 0\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

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

func TestAgentRunVerificationTimeoutKillsProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	harnessScript := filepath.Join(tmpDir, "harness.sh")

	err := os.WriteFile(harnessScript, []byte("#!/bin/sh\nexit 0\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Children like sleep spawned in background must be killed when verify times out
	taskSpec := agents.TaskSpec{
		SpecVersion: agents.SpecVersion,
		TaskID:      "task-test-timeout",
		Prompt:      "timeout test",
		Verify: &agents.VerifySpec{
			Command: "sh -c 'sleep 60 & wait'",
			Timeout: 1, // 1 second timeout
		},
	}
	taskData, _ := json.Marshal(taskSpec)
	if err := os.WriteFile(taskFile, taskData, 0644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = runAgentRun([]string{
		"--task-file", taskFile,
		"--result-file", resultFile,
		"--entrypoint", harnessScript,
		"--workdir", tmpDir,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected runAgentRun to fail on verification timeout")
	}

	// Must finish within 4s (1s timeout + wait delay slack), not wait 60s
	if elapsed > 4*time.Second {
		t.Fatalf("verification timeout did not terminate child process promptly, elapsed: %v", elapsed)
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
		t.Fatalf("expected status failed, got %q", res.Status)
	}
	if res.ExitCode != 124 {
		t.Fatalf("expected exit code 124 for timeout, got %d", res.ExitCode)
	}
	if res.Verification == nil || res.Verification.Passed {
		t.Fatalf("expected verification to record failure: %+v", res.Verification)
	}
	if res.Verification.ExitCode != 124 {
		t.Fatalf("expected verification exit code 124, got %d", res.Verification.ExitCode)
	}
}

func TestAgentRunExit78OnMissingEntrypoint(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	missingEntrypoint := filepath.Join(tmpDir, "nonexistent-harness.sh")

	taskSpec := agents.TaskSpec{
		SpecVersion: agents.SpecVersion,
		TaskID:      "task-missing-ep",
		Prompt:      "missing entrypoint test",
	}
	taskData, _ := json.Marshal(taskSpec)
	if err := os.WriteFile(taskFile, taskData, 0644); err != nil {
		t.Fatal(err)
	}

	err := runAgentRun([]string{
		"--task-file", taskFile,
		"--result-file", resultFile,
		"--entrypoint", missingEntrypoint,
		"--workdir", tmpDir,
	})
	if err == nil {
		t.Fatal("expected error for missing entrypoint")
	}

	resData, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var res agents.ResultSpec
	if err := json.Unmarshal(resData, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.ExitCode != 78 {
		t.Fatalf("expected exit code 78 for missing entrypoint, got %d", res.ExitCode)
	}
}

func TestAgentRunExit78OnUnsupportedSpecVersion(t *testing.T) {
	tmpDir := t.TempDir()
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	harnessScript := filepath.Join(tmpDir, "harness.sh")
	_ = os.WriteFile(harnessScript, []byte("#!/bin/sh\nexit 0\n"), 0755)

	taskSpec := agents.TaskSpec{
		SpecVersion: "buddy.agent/v999", // unsupported
		TaskID:      "task-bad-version",
		Prompt:      "version test",
	}
	taskData, _ := json.Marshal(taskSpec)
	if err := os.WriteFile(taskFile, taskData, 0644); err != nil {
		t.Fatal(err)
	}

	err := runAgentRun([]string{
		"--task-file", taskFile,
		"--result-file", resultFile,
		"--entrypoint", harnessScript,
		"--workdir", tmpDir,
	})
	if err == nil {
		t.Fatal("expected error for unsupported spec version")
	}

	resData, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var res agents.ResultSpec
	if err := json.Unmarshal(resData, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.ExitCode != 78 {
		t.Fatalf("expected exit code 78 for unsupported spec version, got %d", res.ExitCode)
	}
}
