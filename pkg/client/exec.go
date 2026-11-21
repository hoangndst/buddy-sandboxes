package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// ExecOptions specifies configuration for an interactive or batch exec session.
type ExecOptions struct {
	Cmd        []string
	TTY        bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	ResizeChan <-chan apiv1.ResizeMessage
}

// Exec opens a multiplexed WebSocket session to execute a command inside the sandbox.
func (c *Client) Exec(ctx context.Context, id string, opts ExecOptions) (int, error) {
	q := url.Values{}
	for _, cmd := range opts.Cmd {
		q.Add("cmd", cmd)
	}
	q.Set("tty", strconv.FormatBool(opts.TTY))

	execPath := fmt.Sprintf("/api/v1/sandboxes/%s/exec?%s", url.PathEscape(id), q.Encode())
	wsURL := c.WSURL(execPath)

	headers := http.Header{}
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}

	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			return 1, fmt.Errorf("websocket dial failed with status %d: %w", resp.StatusCode, err)
		}
		return 1, fmt.Errorf("websocket dial failed: %w", err)
	}
	defer conn.Close()

	var writeMu sync.Mutex
	sendFrame := func(channel byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		payload := append([]byte{channel}, data...)
		return conn.WriteMessage(websocket.BinaryMessage, payload)
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	// Ensure connection is terminated if session context is canceled (Finding 6)
	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()

	// Goroutine 1: Stdin pump
	if opts.Stdin != nil {
		go func() {
			buf := make([]byte, 16384)
			for {
				select {
				case <-sessionCtx.Done():
					return
				default:
					n, err := opts.Stdin.Read(buf)
					if n > 0 {
						if err := sendFrame(apiv1.ChannelStdin, buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						// Propagate EOF without closing WebSocket connection (Finding 7)
						_ = sendFrame(apiv1.ChannelStdin, nil)
						return
					}
				}
			}
		}()
	}

	// Goroutine 2: Terminal resize pump
	if opts.ResizeChan != nil {
		go func() {
			for {
				select {
				case <-sessionCtx.Done():
					return
				case resize, ok := <-opts.ResizeChan:
					if !ok {
						return
					}
					data, err := json.Marshal(resize)
					if err == nil {
						_ = sendFrame(apiv1.ChannelResize, data)
					}
				}
			}
		}()
	}

	// Main loop: Read frames from remote container
	exitCode := 0
	var exitErr error
	receivedExitFrame := false

	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if msgType != websocket.BinaryMessage || len(payload) == 0 {
			continue
		}

		channel := payload[0]
		data := payload[1:]

		switch channel {
		case apiv1.ChannelStdout:
			if opts.Stdout != nil && len(data) > 0 {
				_, _ = opts.Stdout.Write(data)
			}
		case apiv1.ChannelStderr:
			if opts.Stderr != nil && len(data) > 0 {
				_, _ = opts.Stderr.Write(data)
			}
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(data, &exitMsg); err == nil {
				receivedExitFrame = true
				exitCode = exitMsg.ExitCode
				if exitMsg.Error != "" {
					exitErr = fmt.Errorf("remote execution error: %s", exitMsg.Error)
				}
			}
		}
	}

	// Report connection loss before an exit frame as an error (Finding 5)
	if !receivedExitFrame && exitErr == nil {
		if ctx.Err() != nil {
			return 1, ctx.Err()
		}
		return 1, fmt.Errorf("connection closed by server before exit frame received")
	}

	return exitCode, exitErr
}
