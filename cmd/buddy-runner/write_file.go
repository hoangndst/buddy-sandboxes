package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

func runWriteFile(args []string) error {
	fs := flag.NewFlagSet("write-file", flag.ContinueOnError)
	modeStr := fs.String("mode", "0644", "File permission mode in octal")
	content := fs.String("content", "", "Inline content string; if omitted, reads from stdin")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: buddy-runner write-file [flags] <target_path>")
	}
	targetPath := fs.Arg(0)

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

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d", targetPath, os.Getpid())
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to target: %w", err)
	}

	return nil
}
