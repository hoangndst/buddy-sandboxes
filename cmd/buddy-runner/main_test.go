package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestBuddyRunnerStartAndAttach(t *testing.T) {
	sessionID := "test_sess_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		err := runStart([]string{sessionID, "--", "sh", "-c", "echo hello_runner; sleep 0.2; echo second_line"})
		doneCh <- err
	}()

	sockPath := filepath.Join(socketDir, sessionID+".sock")
	var conn net.Conn
	var dialErr error
	for i := 0; i < 20; i++ {
		conn, dialErr = net.Dial("unix", sockPath)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("failed dialing unix socket: %v", dialErr)
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	receivedExit := false
	var exitCode int

	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err == nil {
				receivedExit = true
				exitCode = exitMsg.ExitCode
			}
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}
	if !receivedExit {
		t.Fatalf("did not receive exit frame")
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	outputStr := receivedStdout.String()
	hasFirst := bytes.Contains([]byte(outputStr), []byte("hello_runner"))
	hasSecond := bytes.Contains([]byte(outputStr), []byte("second_line"))
	if !hasFirst || !hasSecond {
		t.Fatalf("expected output containing hello_runner and second_line, got: %q", outputStr)
	}
}

func TestBuddyRunnerScrollbackReplay(t *testing.T) {
	sessionID := "test_replay_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		// Output early lines, wait, then exit
		cmd := "echo early_line_1; echo early_line_2; sleep 0.4; echo final_line"
		err := runStart([]string{sessionID, "--", "sh", "-c", cmd})
		doneCh <- err
	}()

	sockPath := filepath.Join(socketDir, sessionID+".sock")
	// Wait a bit so early lines are produced and buffered in the ring buffer
	time.Sleep(200 * time.Millisecond)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("failed dialing unix socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	receivedExit := false
	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
		case apiv1.ChannelExit:
			receivedExit = true
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}
	if !receivedExit {
		t.Fatalf("expected exit frame")
	}
	output := receivedStdout.String()
	if !bytes.Contains([]byte(output), []byte("early_line_1")) ||
		!bytes.Contains([]byte(output), []byte("early_line_2")) ||
		!bytes.Contains([]byte(output), []byte("final_line")) {
		t.Fatalf("scrollback missing lines, got: %q", output)
	}
}

func TestBuddyRunnerNonTTYStdin(t *testing.T) {
	sessionID := "test_nontty_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		err := runStart([]string{sessionID, "-tty=false", "--", "sh", "-c", "read line; echo got:$line"})
		doneCh <- err
	}()

	sockPath := filepath.Join(socketDir, sessionID+".sock")
	var conn net.Conn
	var dialErr error
	for i := 0; i < 20; i++ {
		conn, dialErr = net.Dial("unix", sockPath)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("failed dialing unix socket: %v", dialErr)
	}
	defer func() { _ = conn.Close() }()

	// Send stdin to non-TTY process
	if err := sendFramedMessage(conn, nil, apiv1.ChannelStdin, []byte("pipe_input_test\n")); err != nil {
		t.Fatalf("failed sending stdin: %v", err)
	}

	var receivedStdout bytes.Buffer
	receivedExit := false
	var exitCode int

	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err == nil {
				receivedExit = true
				exitCode = exitMsg.ExitCode
			}
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}
	if !receivedExit {
		t.Fatalf("did not receive exit frame")
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	output := receivedStdout.String()
	if !bytes.Contains([]byte(output), []byte("got:pipe_input_test")) {
		t.Fatalf("expected output containing 'got:pipe_input_test', got: %q", output)
	}
}
