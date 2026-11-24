package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
