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
	"os/signal"
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

	// Configure environment variables: apply user env first (excluding BUDDY_ control overrides)
	setupAgentRunEnv(*taskFile, *resultFile, &taskSpec)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	harnessExitCode, err := executeHarnessSubprocess(epBinary, epFields[1:], *workDir, sigCh)
	if err != nil {
		resultSpec := agents.ResultSpec{
			SpecVersion: agents.SpecVersion,
			Status:      statusFailed,
			ExitCode:    harnessExitCode,
		}
		writeResultFile(*resultFile, resultSpec)
		return sessionExitError(harnessExitCode)
	}

	resultSpec := agents.ResultSpec{
		SpecVersion: agents.SpecVersion,
		ExitCode:    harnessExitCode,
	}

	// Supervisor Verification Gate
	if harnessExitCode == 0 && taskSpec.Verify != nil && strings.TrimSpace(taskSpec.Verify.Command) != "" {
		vData := runVerificationGate(taskSpec.Verify, *workDir, sigCh)
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

func setupAgentRunEnv(taskFile, resultFile string, taskSpec *agents.TaskSpec) {
	for k, v := range taskSpec.Env {
		kTrim := strings.TrimSpace(k)
		if kTrim == "" || strings.Contains(kTrim, "=") {
			continue
		}
		upper := strings.ToUpper(kTrim)
		if upper == "PATH" || upper == "HOME" || upper == "USER" || upper == "SHELL" {
			continue
		}
		if strings.HasPrefix(upper, "BUDDY_") || strings.HasPrefix(upper, "OPENAI_") ||
			strings.HasPrefix(upper, "ANTHROPIC_") ||
			strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
			continue
		}
		_ = os.Setenv(kTrim, v)
	}

	_ = os.Setenv("BUDDY_TASK_FILE", taskFile)
	_ = os.Setenv("BUDDY_RESULT_FILE", resultFile)
	promptPath := filepath.Join(filepath.Dir(taskFile), "prompt.txt")
	if _, err := os.Stat(promptPath); err == nil {
		_ = os.Setenv("BUDDY_PROMPT_FILE", promptPath)
	}
	sysPromptPath := filepath.Join(filepath.Dir(taskFile), "system-prompt.txt")
	if _, err := os.Stat(sysPromptPath); err == nil {
		_ = os.Setenv("BUDDY_SYSTEM_PROMPT_FILE", sysPromptPath)
	}
	if taskSpec.Model != "" {
		_ = os.Setenv("BUDDY_MODEL", taskSpec.Model)
	}
	if taskSpec.MaxSteps > 0 {
		_ = os.Setenv("BUDDY_MAX_STEPS", fmt.Sprintf("%d", taskSpec.MaxSteps))
	}
}

func executeHarnessSubprocess(
	epBinary string,
	epArgs []string,
	workDir string,
	sigCh <-chan os.Signal,
) (int, error) {
	harnessCmd := exec.Command(epBinary, epArgs...)
	harnessCmd.Dir = workDir
	harnessCmd.Stdin = os.Stdin
	harnessCmd.Stdout = os.Stdout
	harnessCmd.Stderr = os.Stderr
	harnessCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := harnessCmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return exitConfig, err
		}
		return 1, err
	}

	harnessDone := make(chan error, 1)
	go func() {
		harnessDone <- harnessCmd.Wait()
	}()

	select {
	case sig := <-sigCh:
		if harnessCmd.Process != nil && harnessCmd.Process.Pid > 0 {
			_ = syscall.Kill(-harnessCmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-harnessCmd.Process.Pid, syscall.SIGKILL)
			case <-harnessDone:
			}
		}
		termCode := 143
		if sig == syscall.SIGINT {
			termCode = 130
		}
		return termCode, fmt.Errorf("interrupted by signal %v", sig)
	case harnessErr := <-harnessDone:
		if harnessErr != nil {
			var exitErr *exec.ExitError
			if errors.As(harnessErr, &exitErr) {
				return exitErr.ExitCode(), nil
			} else if errors.Is(harnessErr, exec.ErrNotFound) {
				return exitConfig, nil
			}
			return 1, nil
		}
		return 0, nil
	}
}

func runVerificationGate(
	verifySpec *agents.VerifySpec,
	workDir string,
	sigCh <-chan os.Signal,
) *agents.VerificationData {
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

	if err := verifyCmd.Start(); err != nil {
		return &agents.VerificationData{
			Passed:   false,
			ExitCode: 1,
			Output:   fmt.Sprintf("start verification: %v", err),
		}
	}

	verifyDone := make(chan error, 1)
	go func() {
		verifyDone <- verifyCmd.Wait()
	}()

	var verifyErr error
	var sigInterrupted bool
	var sigCode int

	select {
	case sig := <-sigCh:
		sigInterrupted = true
		sigCode = 143
		if sig == syscall.SIGINT {
			sigCode = 130
		}
		if verifyCmd.Process != nil && verifyCmd.Process.Pid > 0 {
			_ = syscall.Kill(-verifyCmd.Process.Pid, syscall.SIGKILL)
		}
		verifyErr = <-verifyDone
	case verifyErr = <-verifyDone:
	}

	verifyExitCode := 0
	if sigInterrupted {
		verifyExitCode = sigCode
	} else if verifyErr != nil {
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
