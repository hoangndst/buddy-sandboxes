package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestAttachSessionDetachWithEscapeHotkey(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		for {
			_, _, err := wsConn.ReadMessage()
			if err != nil {
				break
			}
		}
	}))
	defer s.Close()

	cli, err := NewClient(Config{Endpoint: s.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	stdinR, stdinW := io.Pipe()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = stdinW.Write([]byte{0x1D})
		_ = stdinW.Close()
	}()

	var stdout, stderr bytes.Buffer
	_, err = cli.AttachSession(context.Background(), "test-sbx", "test-sess", AttachOptions{
		Stdin:  stdinR,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on escape hotkey, got: %v", err)
	}
}

func TestAttachSessionStdinEOFSendsEmptyFrame(t *testing.T) {
	eofFrameReceived := make(chan bool, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				break
			}
			if len(msg) > 0 && msg[0] == apiv1.ChannelStdin {
				if len(msg) == 1 {
					eofFrameReceived <- true
					exitPayload, _ := json.Marshal(apiv1.ExitMessage{ExitCode: 0})
					_ = wsConn.WriteMessage(websocket.BinaryMessage, append([]byte{apiv1.ChannelExit}, exitPayload...))
					return
				}
			}
		}
	}))
	defer s.Close()

	cli, err := NewClient(Config{Endpoint: s.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	stdinR, stdinW := io.Pipe()
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = stdinW.Write([]byte("hello piped input"))
		_ = stdinW.Close()
	}()

	var stdout, stderr bytes.Buffer
	exitCode, err := cli.AttachSession(context.Background(), "test-sbx", "test-sess", AttachOptions{
		Stdin:  stdinR,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("AttachSession failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	select {
	case received := <-eofFrameReceived:
		if !received {
			t.Fatal("expected eof frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for empty stdin EOF frame")
	}
}
