package main

import (
	"bytes"
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
	"time"
	"unicode/utf8"

	"github.com/hoangndst/buddy-sandboxes/internal/agents"
)

const (
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
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

	_ = os.Setenv("BUDDY_TASK_FILE", *taskFile)
	_ = os.Setenv("BUDDY_RESULT_FILE", *resultFile)
	_ = os.Setenv("BUDDY_PROMPT", taskSpec.Prompt)
	if taskSpec.Model != "" {
		_ = os.Setenv("BUDDY_MODEL", taskSpec.Model)
	}

	epFields := strings.Fields(*entrypoint)
	if len(epFields) == 0 {
		epFields = []string{agents.DefaultEntrypoint}
	}
	harnessCmd := exec.Command(epFields[0], epFields[1:]...)
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
		verifyTimeout := taskSpec.Verify.Timeout
		if verifyTimeout <= 0 {
			verifyTimeout = 300
		}

		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), time.Duration(verifyTimeout)*time.Second)
		defer verifyCancel()

		verifyCmd := exec.CommandContext(verifyCtx, "/bin/sh", "-c", taskSpec.Verify.Command)
		verifyCmd.Dir = *workDir

		var verifyBuf bytes.Buffer
		verifyCmd.Stdout = io.MultiWriter(os.Stdout, &verifyBuf)
		verifyCmd.Stderr = io.MultiWriter(os.Stderr, &verifyBuf)

		verifyErr := verifyCmd.Run()
		verifyExitCode := 0
		if verifyErr != nil {
			var exitErr *exec.ExitError
			if errors.As(verifyErr, &exitErr) {
				verifyExitCode = exitErr.ExitCode()
			} else if errors.Is(verifyCtx.Err(), context.DeadlineExceeded) {
				verifyExitCode = 124
				_, _ = fmt.Fprintln(os.Stderr, "\n[VERIFICATION TIMED OUT]")
			} else {
				verifyExitCode = 1
			}
		}

		rawOutput := verifyBuf.String()
		if len(rawOutput) > 64*1024 {
			avail := 64*1024 - len("[TRUNCATED]\n")
			tail := rawOutput[len(rawOutput)-avail:]
			for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
				tail = tail[1:]
			}
			rawOutput = "[TRUNCATED]\n" + tail
		}

		passed := verifyExitCode == 0
		resultSpec.Verification = &agents.VerificationData{
			Passed:   passed,
			ExitCode: verifyExitCode,
			Output:   rawOutput,
		}

		if passed {
			resultSpec.Status = statusSucceeded
		} else {
			resultSpec.Status = statusFailed
			resultSpec.ExitCode = verifyExitCode
		}
	} else if harnessExitCode == 0 {
		resultSpec.Status = statusSucceeded
	} else {
		resultSpec.Status = statusFailed
	}

	resultData, err := json.MarshalIndent(resultSpec, "", "  ")
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(*resultFile), 0755)
		tmpPath := fmt.Sprintf("%s.tmp.%d", *resultFile, os.Getpid())
		if writeErr := os.WriteFile(tmpPath, resultData, 0644); writeErr == nil {
			_ = os.Rename(tmpPath, *resultFile)
		}
	}

	if resultSpec.ExitCode != 0 {
		return sessionExitError(resultSpec.ExitCode)
	}
	return nil
}
