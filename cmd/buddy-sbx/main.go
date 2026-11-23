package main

import (
	"errors"
	"fmt"
	"os"
)

// ExitCodeError indicates that the CLI command finished with a specific process exit code.
type ExitCodeError struct {
	Code int
}

func (e ExitCodeError) Error() string {
	return ""
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		var exitErr ExitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
