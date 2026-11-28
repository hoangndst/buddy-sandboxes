package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var allowedWritePrefix = "/run/buddy"

func runWriteFile(args []string) error {
	fs := flag.NewFlagSet("write-file", flag.ContinueOnError)
	modeStr := fs.String("mode", "0600", "File permission mode in octal")
	content := fs.String("content", "", "Inline content string; if omitted, reads from stdin")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: buddy-runner write-file [flags] <target_path>")
	}
	targetPath := fs.Arg(0)

	cleanTarget := filepath.Clean(targetPath)
	rel, err := filepath.Rel(allowedWritePrefix, cleanTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf(
			"path traversal forbidden: target path %q must be located strictly within %q",
			targetPath, allowedWritePrefix,
		)
	}

	parentDir := filepath.Dir(cleanTarget)
	if realParent, evalErr := filepath.EvalSymlinks(parentDir); evalErr == nil {
		realRel, relErr := filepath.Rel(allowedWritePrefix, realParent)
		if relErr != nil || (realRel != "." && strings.HasPrefix(realRel, ".."+string(filepath.Separator))) {
			return fmt.Errorf("path traversal forbidden: parent %q escapes %q", parentDir, allowedWritePrefix)
		}
	}
	if fi, lerr := os.Lstat(cleanTarget); lerr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path traversal forbidden: target path %q cannot be a symlink", targetPath)
		}
	}

	modeVal, err := strconv.ParseUint(*modeStr, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q: %w", *modeStr, err)
	}
	perm := os.FileMode(modeVal)

	var data []byte
	if *content != "" {
		data = []byte(*content)
	} else {
		stdinData, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		data = stdinData
	}

	if err := os.MkdirAll(filepath.Dir(cleanTarget), 0700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d", cleanTarget, os.Getpid())
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, cleanTarget); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to target: %w", err)
	}

	return nil
}
