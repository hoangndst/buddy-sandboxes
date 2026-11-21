//go:build !windows

package main

import (
	"os"
	"syscall"
)

func terminalResizeSignal() os.Signal {
	return syscall.SIGWINCH
}
