//go:build windows

package sbx

import "os"

func terminalResizeSignal() os.Signal {
	return nil
}
