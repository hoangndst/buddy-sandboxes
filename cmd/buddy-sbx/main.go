package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/hoangndst/buddy-sandboxes/pkg/cli/sbx"
)

func main() {
	cmd := sbx.NewRootCmd()
	if err := cmd.Execute(); err != nil {
		var exitErr sbx.ExitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
