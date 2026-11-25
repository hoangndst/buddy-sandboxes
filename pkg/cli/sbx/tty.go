package sbx

import (
	"os"
	"os/signal"

	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func setupTTY() (chan apiv1.ResizeMessage, func(), error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, func() {}, nil
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, func() {}, err
	}

	restore := func() {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}

	resizeChan := make(chan apiv1.ResizeMessage, 8)
	if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
		resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}
	}

	sigChan := make(chan os.Signal, 2)
	if resizeSignal := terminalResizeSignal(); resizeSignal != nil {
		signal.Notify(sigChan, resizeSignal)
	}

	go func() {
		for range sigChan {
			if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
				select {
				case resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}:
				default:
				}
			}
		}
	}()

	cleanup := func() {
		signal.Stop(sigChan)
		restore()
	}

	return resizeChan, cleanup, nil
}
