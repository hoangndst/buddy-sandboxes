//go:build windows

package main

import "os"

func terminalResizeSignal() os.Signal {
	return nil
}
