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

type frameSender func(channel byte, data []byte) error

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
	defer func() {
		_ = conn.Close()
	}()

	var writeMu sync.Mutex
	sendFrame := func(channel byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		payload := append([]byte{channel}, data...)
		return conn.WriteMessage(websocket.BinaryMessage, payload)
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()

	if opts.Stdin != nil {
		go c.pumpStdin(sessionCtx, opts.Stdin, sendFrame)
	}

	if opts.ResizeChan != nil {
		go c.pumpResize(sessionCtx, opts.ResizeChan, sendFrame)
	}

	return c.readFrames(ctx, conn, opts)
}

func (c *Client) pumpStdin(ctx context.Context, stdin io.Reader, send frameSender) {
	buf := make([]byte, 16384)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, err := stdin.Read(buf)
			if n > 0 {
				if err := send(apiv1.ChannelStdin, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				_ = send(apiv1.ChannelStdin, nil)
				return
			}
		}
	}
}

func (c *Client) pumpResize(ctx context.Context, resizeChan <-chan apiv1.ResizeMessage, send frameSender) {
	for {
		select {
		case <-ctx.Done():
			return
		case resize, ok := <-resizeChan:
			if !ok {
				return
			}
			data, err := json.Marshal(resize)
			if err == nil {
				_ = send(apiv1.ChannelResize, data)
			}
		}
	}
}

func (c *Client) readFrames(ctx context.Context, conn *websocket.Conn, opts ExecOptions) (int, error) {
	exitCode := 0
	var exitErr error
	receivedExit := false

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
				receivedExit = true
				exitCode = exitMsg.ExitCode
				if exitMsg.Error != "" {
					exitErr = fmt.Errorf("remote execution error: %s", exitMsg.Error)
				}
			}
		}
	}

	if !receivedExit && exitErr == nil {
		if ctx.Err() != nil {
			return 1, ctx.Err()
		}
		return 1, fmt.Errorf("connection closed by server before exit frame received")
	}

	return exitCode, exitErr
}
