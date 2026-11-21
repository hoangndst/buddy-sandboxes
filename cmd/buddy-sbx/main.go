package main

import (
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
		if exitErr, ok := err.(ExitCodeError); ok {
			os.Exit(exitErr.Code)
		}
		os.Exit(1)
	}
}
