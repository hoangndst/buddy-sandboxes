package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hoangndst/buddy-sandboxes/internal/agents"
)

const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	exitConfig      = 78
)

func runAgentRun(args []string) error {
	fs := flag.NewFlagSet("agent-run", flag.ContinueOnError)
	taskFile := fs.String("task-file", agents.DefaultTaskFile, "Path to task specification JSON")
	resultFile := fs.String("result-file", agents.DefaultResultFile, "Path to write result JSON")
	entrypoint := fs.String("entrypoint", agents.DefaultEntrypoint, "Path to agent harness entrypoint script")
	workDir := fs.String("workdir", "/workspace", "Working directory for agent and verification")

	if err := fs.Parse(args); err != nil {
		return err
	}

	taskData, err := os.ReadFile(*taskFile)
	if err != nil {
		return fmt.Errorf("read task file %q: %w", *taskFile, err)
	}

	var taskSpec agents.TaskSpec
	if err := json.Unmarshal(taskData, &taskSpec); err != nil {
		return fmt.Errorf("unmarshal task file %q: %w", *taskFile, err)
	}

	// Validate spec version: exit 78 on unsupported spec version
	if taskSpec.SpecVersion != agents.SpecVersion {
		resultSpec := agents.ResultSpec{
			SpecVersion: agents.SpecVersion,
			Status:      statusFailed,
			ExitCode:    exitConfig,
		}
		writeResultFile(*resultFile, resultSpec)
		return sessionExitError(exitConfig)
	}

	epFields := strings.Fields(*entrypoint)
	if len(epFields) == 0 {
		epFields = []string{agents.DefaultEntrypoint}
	}
	epBinary := epFields[0]
	if _, statErr := os.Stat(epBinary); statErr != nil {
		if _, lookErr := exec.LookPath(epBinary); lookErr != nil {
			resultSpec := agents.ResultSpec{
				SpecVersion: agents.SpecVersion,
				Status:      statusFailed,
				ExitCode:    exitConfig,
			}
			writeResultFile(*resultFile, resultSpec)
			return sessionExitError(exitConfig)
		}
	}

	// Configure environment variables (avoid exporting large prompts into environment)
	_ = os.Setenv("BUDDY_TASK_FILE", *taskFile)
	_ = os.Setenv("BUDDY_RESULT_FILE", *resultFile)
	promptPath := filepath.Join(filepath.Dir(*taskFile), "prompt.txt")
	if _, err := os.Stat(promptPath); err == nil {
		_ = os.Setenv("BUDDY_PROMPT_FILE", promptPath)
	}
	sysPromptPath := filepath.Join(filepath.Dir(*taskFile), "system-prompt.txt")
	if _, err := os.Stat(sysPromptPath); err == nil {
		_ = os.Setenv("BUDDY_SYSTEM_PROMPT_FILE", sysPromptPath)
	}
	if taskSpec.Model != "" {
		_ = os.Setenv("BUDDY_MODEL", taskSpec.Model)
	}
	if taskSpec.MaxSteps > 0 {
		_ = os.Setenv("BUDDY_MAX_STEPS", fmt.Sprintf("%d", taskSpec.MaxSteps))
	}
	for k, v := range taskSpec.Env {
		_ = os.Setenv(k, v)
	}

	harnessCmd := exec.Command(epBinary, epFields[1:]...)
	harnessCmd.Dir = *workDir
	harnessCmd.Stdin = os.Stdin
	harnessCmd.Stdout = os.Stdout
	harnessCmd.Stderr = os.Stderr

	harnessErr := harnessCmd.Run()
	harnessExitCode := 0
	if harnessErr != nil {
		var exitErr *exec.ExitError
		if errors.As(harnessErr, &exitErr) {
			harnessExitCode = exitErr.ExitCode()
		} else if errors.Is(harnessErr, exec.ErrNotFound) {
			harnessExitCode = exitConfig
		} else {
			harnessExitCode = 1
		}
	}

	resultSpec := agents.ResultSpec{
		SpecVersion: agents.SpecVersion,
		ExitCode:    harnessExitCode,
	}

	// Supervisor Verification Gate
	if harnessExitCode == 0 && taskSpec.Verify != nil && strings.TrimSpace(taskSpec.Verify.Command) != "" {
		vData := runVerificationGate(taskSpec.Verify, *workDir)
		resultSpec.Verification = vData
		if vData.Passed {
			resultSpec.Status = statusSucceeded
		} else {
			resultSpec.Status = statusFailed
			resultSpec.ExitCode = vData.ExitCode
		}
	} else if harnessExitCode == 0 {
		resultSpec.Status = statusSucceeded
	} else {
		resultSpec.Status = statusFailed
	}

	writeResultFile(*resultFile, resultSpec)

	if resultSpec.ExitCode != 0 {
		return sessionExitError(resultSpec.ExitCode)
	}
	return nil
}

func runVerificationGate(verifySpec *agents.VerifySpec, workDir string) *agents.VerificationData {
	verifyTimeout := verifySpec.Timeout
	if verifyTimeout <= 0 {
		verifyTimeout = 300
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), time.Duration(verifyTimeout)*time.Second)
	defer verifyCancel()

	verifyCmd := exec.CommandContext(verifyCtx, "/bin/sh", "-c", verifySpec.Command)
	verifyCmd.Dir = workDir
	verifyCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	verifyCmd.Cancel = func() error {
		if verifyCmd.Process != nil && verifyCmd.Process.Pid > 0 {
			return syscall.Kill(-verifyCmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	verifyCmd.WaitDelay = 2 * time.Second

	verifyBuf := NewRingBuffer(64 * 1024)
	verifyCmd.Stdout = io.MultiWriter(os.Stdout, verifyBuf)
	verifyCmd.Stderr = io.MultiWriter(os.Stderr, verifyBuf)

	verifyErr := verifyCmd.Run()
	verifyExitCode := 0
	if verifyErr != nil {
		var exitErr *exec.ExitError
		if errors.Is(verifyCtx.Err(), context.DeadlineExceeded) {
			verifyExitCode = 124
			_, _ = fmt.Fprintln(os.Stderr, "\n[VERIFICATION TIMED OUT]")
		} else if errors.As(verifyErr, &exitErr) {
			ws, ok := exitErr.Sys().(syscall.WaitStatus)
			if ok && ws.Signaled() && verifyCtx.Err() != nil {
				verifyExitCode = 124
				_, _ = fmt.Fprintln(os.Stderr, "\n[VERIFICATION TIMED OUT]")
			} else {
				verifyExitCode = exitErr.ExitCode()
			}
		} else {
			verifyExitCode = 1
		}
	}

	rawBytes := verifyBuf.Bytes()
	rawOutput := string(rawBytes)
	if verifyBuf.IsWrapped() {
		for len(rawBytes) > 0 && !utf8.RuneStart(rawBytes[0]) {
			rawBytes = rawBytes[1:]
		}
		rawOutput = "[TRUNCATED]\n" + string(rawBytes)
	}

	return &agents.VerificationData{
		Passed:   verifyExitCode == 0,
		ExitCode: verifyExitCode,
		Output:   rawOutput,
	}
}

func writeResultFile(path string, resultSpec agents.ResultSpec) {
	resultData, err := json.MarshalIndent(resultSpec, "", "  ")
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
		if writeErr := os.WriteFile(tmpPath, resultData, 0600); writeErr == nil {
			_ = os.Rename(tmpPath, path)
		}
	}
}
