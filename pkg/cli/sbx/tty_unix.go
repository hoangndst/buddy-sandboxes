//go:build !windows

package sbx

import (
	"os"
	"syscall"
)

func terminalResizeSignal() os.Signal {
	return syscall.SIGWINCH
}
