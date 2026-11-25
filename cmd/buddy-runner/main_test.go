package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
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

func TestSessionLogRetainsBoundedTail(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "session.log")
	if err != nil {
		t.Fatalf("create session log: %v", err)
	}
	defer func() { _ = file.Close() }()

	var logSize int64
	for i := byte('a'); i <= 'f'; i++ {
		logSize, err = appendBoundedSessionLog(file, logSize, bytes.Repeat([]byte{i}, int(maxSessionLogBytes/2)))
		if err != nil {
			t.Fatalf("write log chunk %q: %v", i, err)
		}
	}

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat session log: %v", err)
	}
	if logSize != info.Size() || info.Size() > sessionLogCompactionThreshold {
		t.Fatalf("session log size = %d (tracked %d), want at most %d", info.Size(), logSize, sessionLogCompactionThreshold)
	}

	got, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	want := append(bytes.Repeat([]byte("e"), int(maxSessionLogBytes/2)),
		bytes.Repeat([]byte("f"), int(maxSessionLogBytes/2))...)
	if len(got) < int(maxSessionLogBytes) || !bytes.Equal(got[len(got)-int(maxSessionLogBytes):], want) {
		t.Fatal("session log did not retain the most recent output bytes")
	}
}

func TestBuddyRunnerScrollbackReplay(t *testing.T) {
	sessionID := "test_replay_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
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
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		err := runStart([]string{sessionID, "-tty=false", "--", "sh", "-c", "read line; echo got:$line; echo err:$line >&2"})
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	// Send stdin to non-TTY process
	if err := sendFramedMessage(conn, nil, apiv1.ChannelStdin, []byte("pipe_input_test\n")); err != nil {
		t.Fatalf("failed sending stdin: %v", err)
	}

	var receivedStdout bytes.Buffer
	var receivedStderr bytes.Buffer
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
		case apiv1.ChannelStderr:
			receivedStderr.Write(payload)
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
	if got := receivedStderr.String(); !strings.Contains(got, "err:pipe_input_test") {
		t.Fatalf("stderr was not delivered on its own channel: %q", got)
	}
}

func TestSupervisorSessionBroadcastFrameStalledClient(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()

	session := &supervisorSession{
		ringBuf: NewRingBuffer(1024),
		clients: make(map[net.Conn]*clientConn),
	}
	cc := &clientConn{conn: c1, send: make(chan runnerFrame, 1)}
	session.clients[c1] = cc

	done := make(chan struct{})
	go func() {
		session.broadcastFrame(apiv1.ChannelStdout, []byte("hello-stall-test"))
		session.broadcastFrame(apiv1.ChannelStdout, []byte("queue-overflow"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("broadcastFrame waited for the stalled client")
	}

	session.clientsMu.Lock()
	_, exists := session.clients[c1]
	session.clientsMu.Unlock()
	if exists {
		t.Errorf("expected stalled client to be removed from session.clients")
	}
}

func TestTerminateProcessGroupEscalatesAfterLeaderExit(t *testing.T) {
	const leaderPID = 123
	var sent []syscall.Signal
	signal := func(pid int, sig syscall.Signal) error {
		if pid != -leaderPID {
			t.Fatalf("signal targeted PID %d, want process group %d", pid, -leaderPID)
		}
		if sig != 0 {
			sent = append(sent, sig)
		}
		return nil
	}
	if err := terminateProcessGroup(leaderPID, signal); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Fatalf("sent %v, want SIGTERM followed by SIGKILL", sent)
	}
}

func TestOutputDrainDoesNotCloseWhileIngestionIsProcessing(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	var readState outputReadState
	done := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		readState.drain(reader, done, 10*time.Millisecond)
		close(drained)
	}()
	// Simulate processing an already-read chunk for longer than the idle limit.
	time.Sleep(30 * time.Millisecond)
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		if _, err := readState.read(reader, buf); err != nil {
			t.Error(err)
		}
	}()
	if _, err := writer.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after output ingestion completed")
	}
}

func TestBuddyRunnerReplayOrderingAtomicWithBroadcast(t *testing.T) {
	sessionID := "test_order_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		cmd := "for i in $(seq 1 30); do echo \"TOKEN_$i\"; sleep 0.02; done"
		err := runStart([]string{sessionID, "--", "sh", "-c", cmd})
		doneCh <- err
	}()

	sockPath := filepath.Join(socketDir, sessionID+".sock")
	time.Sleep(100 * time.Millisecond) // Let initial tokens be buffered

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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		if ch == apiv1.ChannelStdout {
			receivedStdout.Write(payload)
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}

	output := receivedStdout.String()
	lastIdx := -1
	for i := 1; i <= 30; i++ {
		token := fmt.Sprintf("TOKEN_%d", i)
		idx := strings.Index(output, token)
		if idx != -1 {
			if lastIdx != -1 && idx < lastIdx {
				t.Fatalf("tokens arrived out of order: TOKEN_%d at %d before previous at %d", i, idx, lastIdx)
			}
			lastIdx = idx
		}
	}
}

func TestBuddyRunnerControllingTerminal(t *testing.T) {
	sessionID := "test_sess_ctty_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		// When controlling terminal is properly set, opening /dev/tty succeeds.
		err := runStart([]string{sessionID, "--", "sh", "-c", "exec 3</dev/tty && echo ctty_ok; sleep 0.2"})
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		if ch == apiv1.ChannelStdout {
			receivedStdout.Write(payload)
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}

	if !strings.Contains(receivedStdout.String(), "ctty_ok") {
		t.Fatalf("expected child to have controlling terminal (/dev/tty), output was: %q", receivedStdout.String())
	}
}

func TestReplayCompletedSessionLog(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("non-existent file", func(t *testing.T) {
		var buf bytes.Buffer
		err := replayCompletedSessionLog(&buf, filepath.Join(tmpDir, "missing.log"))
		if err == nil {
			t.Fatal("expected error for non-existent log, got nil")
		}
		if buf.Len() != 0 {
			t.Fatalf("expected 0 bytes written, got %d", buf.Len())
		}
	})

	t.Run("small log file under cap", func(t *testing.T) {
		logPath := filepath.Join(tmpDir, "small.log")
		content := []byte("hello world log content\nline 2")
		if err := os.WriteFile(logPath, content, 0600); err != nil {
			t.Fatalf("failed to write small log: %v", err)
		}

		var buf bytes.Buffer
		if err := replayCompletedSessionLog(&buf, logPath); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), content) {
			t.Fatalf("expected %q, got %q", string(content), buf.String())
		}
	})

	t.Run("large log file exceeding cap", func(t *testing.T) {
		logPath := filepath.Join(tmpDir, "large.log")
		totalSize := int(maxSessionLogBytes) + 1024*500 // ~1.5MB
		content := make([]byte, totalSize)
		for i := range content {
			content[i] = byte('A' + (i % 26))
		}
		if err := os.WriteFile(logPath, content, 0600); err != nil {
			t.Fatalf("failed to write large log: %v", err)
		}

		var buf bytes.Buffer
		if err := replayCompletedSessionLog(&buf, logPath); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expectedTail := content[totalSize-int(maxSessionLogBytes):]
		if buf.Len() != int(maxSessionLogBytes) {
			t.Fatalf("expected %d bytes, got %d", maxSessionLogBytes, buf.Len())
		}
		if !bytes.Equal(buf.Bytes(), expectedTail) {
			t.Fatalf("replayed tail does not match the expected trailing bytes")
		}
	})
}

func TestAttachReplaysCompletedOutputWhenAcceptedConnectionClosesBeforeFrames(t *testing.T) {
	tmpDir := t.TempDir()
	exitPath := filepath.Join(tmpDir, "session.exit")
	stdoutLogPath := filepath.Join(tmpDir, "session.log")
	stderrLogPath := filepath.Join(tmpDir, "session.stderr.log")
	if err := os.WriteFile(stdoutLogPath, []byte("completed stdout\n"), 0600); err != nil {
		t.Fatalf("write stdout log: %v", err)
	}
	if err := os.WriteFile(stderrLogPath, []byte("completed stderr\n"), 0600); err != nil {
		t.Fatalf("write stderr log: %v", err)
	}
	exitData, err := json.Marshal(exitRecord{ExitCode: 0})
	if err != nil {
		t.Fatalf("marshal exit record: %v", err)
	}
	if err := os.WriteFile(exitPath, exitData, 0600); err != nil {
		t.Fatalf("write exit record: %v", err)
	}

	attachConn, supervisorConn := net.Pipe()
	_ = supervisorConn.Close() // Simulates an accepted connection whose handler saw a completed session.
	defer func() { _ = attachConn.Close() }()

	var stdout, stderr bytes.Buffer
	if err := readAttachStream(attachConn, exitPath, stdoutLogPath, stderrLogPath, &stdout, &stderr); err != nil {
		t.Fatalf("read attach stream: %v", err)
	}
	if got, want := stdout.String(), "completed stdout\n"; got != want {
		t.Fatalf("stdout replay = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "completed stderr\n"; got != want {
		t.Fatalf("stderr replay = %q, want %q", got, want)
	}
}

func TestBuddyRunnerDrainsOutputBeforeExit(t *testing.T) {
	sessionID := "test_drain_output_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		cmd := "for i in $(seq 1 50); do printf 'chunk_%03d_' $i; done; sleep 0.2; echo 'FINAL_DRAIN_TOKEN'; exit 0"
		err := runStart([]string{sessionID, "--", "sh", "-c", cmd})
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	var receivedExit bool
	var finalTokenSeenBeforeExit bool

	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
			if strings.Contains(receivedStdout.String(), "FINAL_DRAIN_TOKEN") {
				finalTokenSeenBeforeExit = true
			}
		case apiv1.ChannelExit:
			receivedExit = true
			if !finalTokenSeenBeforeExit {
				t.Errorf("received ChannelExit before FINAL_DRAIN_TOKEN was ingested and received")
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

	outStr := receivedStdout.String()
	if !strings.Contains(outStr, "chunk_001_") ||
		!strings.Contains(outStr, "chunk_050_") ||
		!strings.Contains(outStr, "FINAL_DRAIN_TOKEN") {
		t.Fatalf("output missing expected chunks: %q", outStr)
	}

	logBytes, err := os.ReadFile(filepath.Join(socketDir, sessionID+".log"))
	if err != nil {
		t.Fatalf("failed to read session log: %v", err)
	}
	if !strings.Contains(string(logBytes), "FINAL_DRAIN_TOKEN") {
		t.Fatalf("session log missing FINAL_DRAIN_TOKEN: %q", string(logBytes))
	}

	exitBytes, err := os.ReadFile(filepath.Join(socketDir, sessionID+".exit"))
	if err != nil {
		t.Fatalf("failed to read session exit record: %v", err)
	}
	var rec exitRecord
	if err := json.Unmarshal(exitBytes, &rec); err != nil {
		t.Fatalf("failed to unmarshal exit record: %v", err)
	}
	if rec.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", rec.ExitCode)
	}
}

func TestBuddyRunnerGrandchildStdoutInheritanceDrainTimeout(t *testing.T) {
	origTimeout := ingestionDrainTimeout
	ingestionDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		ingestionDrainTimeout = origTimeout
	})

	sessionID := "test_drain_timeout_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		// Spawn a background process inheriting stdout, then exit immediately
		cmd := "exec 3>&1; (sleep 10 >&3 &); echo 'grandchild_inherited_ok'; exit 0"
		err := runStart([]string{sessionID, "-tty=false", "--", "sh", "-c", cmd})
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	var receivedStdout bytes.Buffer
	var receivedExit bool

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

	select {
	case startErr := <-doneCh:
		if startErr != nil {
			t.Fatalf("runStart error: %v", startErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("runStart did not unblock within timeout despite reader closure")
	}

	if !receivedExit {
		t.Fatalf("did not receive exit frame")
	}
	if !strings.Contains(receivedStdout.String(), "grandchild_inherited_ok") {
		t.Fatalf("expected output containing 'grandchild_inherited_ok', got: %q", receivedStdout.String())
	}
}

func TestBuddyRunnerAcceptedAttachRaceWithExit(t *testing.T) {
	session := &supervisorSession{
		ringBuf:       NewRingBuffer(1024),
		stderrRingBuf: NewRingBuffer(1024),
		finished:      true,
		exitCode:      42,
		clients:       make(map[net.Conn]*clientConn),
	}
	_, _ = session.ringBuf.Write([]byte("racing_stdout\n"))
	_, _ = session.stderrRingBuf.Write([]byte("racing_stderr\n"))

	clientConnSide, serverConnSide := net.Pipe()
	cc := &clientConn{
		conn: serverConnSide,
		send: make(chan runnerFrame, 128),
		done: make(chan struct{}),
	}

	go session.handleClient(cc)

	var receivedStdout, receivedStderr bytes.Buffer
	var receivedExit bool
	var exitCode int

	for {
		ch, payload, err := readFramedMessage(clientConnSide)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
		case apiv1.ChannelStderr:
			receivedStderr.Write(payload)
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err == nil {
				receivedExit = true
				exitCode = exitMsg.ExitCode
			}
		}
	}
	_ = clientConnSide.Close()

	if !receivedExit {
		t.Fatalf("expected exit frame upon raced handleClient")
	}
	if exitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", exitCode)
	}
	if !strings.Contains(receivedStdout.String(), "racing_stdout") {
		t.Fatalf("expected racing stdout in scrollback, got %q", receivedStdout.String())
	}
	if !strings.Contains(receivedStderr.String(), "racing_stderr") {
		t.Fatalf("expected racing stderr in scrollback, got %q", receivedStderr.String())
	}
}

func TestBuddyRunnerNonTTYRetainsStderrScrollback(t *testing.T) {
	sessionID := "test_stderr_scrollback_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		shCmd := "echo early_err >&2; echo early_out; sleep 0.3; echo late_out"
		err := runStart([]string{sessionID, "-tty=false", "--", "sh", "-c", shCmd})
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

	var receivedStdout, receivedStderr bytes.Buffer
	var receivedExit bool

	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			receivedStdout.Write(payload)
		case apiv1.ChannelStderr:
			receivedStderr.Write(payload)
		case apiv1.ChannelExit:
			receivedExit = true
		}
	}

	startErr := <-doneCh
	if startErr != nil {
		t.Fatalf("runStart error: %v", startErr)
	}
	if !receivedExit {
		t.Fatalf("did not receive exit frame")
	}
	if !strings.Contains(receivedStderr.String(), "early_err") {
		t.Fatalf("expected stderr scrollback to contain 'early_err', got: %q", receivedStderr.String())
	}
	if !strings.Contains(receivedStdout.String(), "early_out") {
		t.Fatalf("expected stdout scrollback to contain 'early_out', got: %q", receivedStdout.String())
	}
}

func TestReadFramedMessagePayloadLimit(t *testing.T) {
	oversized := make([]byte, 5)
	binary.BigEndian.PutUint32(oversized[0:4], maxFramePayloadSize+2)
	oversized[4] = apiv1.ChannelStdout

	_, _, err := readFramedMessage(bytes.NewReader(oversized))
	if err == nil {
		t.Fatalf("expected error for oversized frame, got nil")
	}
	if !strings.Contains(err.Error(), "invalid frame length") {
		t.Fatalf("expected invalid frame length error, got: %v", err)
	}

	msg := []byte("hello")
	valid := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(valid[0:4], uint32(len(msg)+1))
	valid[4] = apiv1.ChannelStdout
	copy(valid[5:], msg)

	ch, payload, err := readFramedMessage(bytes.NewReader(valid))
	if err != nil {
		t.Fatalf("unexpected error for valid frame: %v", err)
	}
	if ch != apiv1.ChannelStdout || string(payload) != "hello" {
		t.Fatalf("unexpected frame content: ch=%d, payload=%s", ch, string(payload))
	}
}

func TestWriteExitRecordAtomic(t *testing.T) {
	dir := t.TempDir()
	exitPath := filepath.Join(dir, "session.exit")
	rec := exitRecord{
		ExitCode:    42,
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		Error:       "test error",
	}

	writeExitRecord(exitPath, rec)

	tmpPath := exitPath + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be removed after commit, stat err: %v", err)
	}

	data, err := os.ReadFile(exitPath)
	if err != nil {
		t.Fatalf("failed to read committed exit record: %v", err)
	}
	var readRec exitRecord
	if err := json.Unmarshal(data, &readRec); err != nil {
		t.Fatalf("failed to unmarshal exit record: %v", err)
	}
	if readRec.ExitCode != 42 || readRec.Error != "test error" {
		t.Fatalf("unexpected exit record contents: %+v", readRec)
	}
}

func TestDialSessionShortCircuitCompleted(t *testing.T) {
	dir := t.TempDir()
	exitPath := filepath.Join(dir, "completed.exit")
	sockPath := filepath.Join(dir, "nonexistent.sock")
	logPath := filepath.Join(dir, "completed.log")

	rec := exitRecord{
		ExitCode:    0,
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
	}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(exitPath, data, 0600); err != nil {
		t.Fatalf("failed to write exit record: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("completed output\n"), 0600); err != nil {
		t.Fatalf("failed to write log: %v", err)
	}

	start := time.Now()
	conn, err := dialSession("completed-session", sockPath, exitPath, logPath, "")
	elapsed := time.Since(start)

	if conn != nil {
		_ = conn.Close()
		t.Fatalf("expected nil connection for completed session, got non-nil")
	}
	if err != nil {
		t.Fatalf("expected nil error for exit code 0 completed session, got: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("dialSession took %v, expected short-circuit under 1s", elapsed)
	}
}

func TestFinishClientsBoundedSend(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() {
		_ = c1.Close()
		_ = c2.Close()
	}()

	s := &supervisorSession{
		clients: make(map[net.Conn]*clientConn),
	}

	cc := &clientConn{
		conn: c1,
		send: make(chan runnerFrame, 1),
		done: make(chan struct{}),
	}
	cc.send <- runnerFrame{channel: apiv1.ChannelStdout, data: []byte("initial")}
	s.clients[c1] = cc

	go func() {
		time.Sleep(50 * time.Millisecond)
		<-cc.send
	}()

	start := time.Now()
	s.finishClients(0)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("finishClients took too long: %v", elapsed)
	}

	select {
	case frame, ok := <-cc.send:
		if !ok || frame.channel != apiv1.ChannelExit {
			t.Fatalf("expected exit frame, got ok=%v, frame=%+v", ok, frame)
		}
	default:
		t.Fatalf("expected exit frame to be delivered to channel")
	}
}

func TestHandleClientStdinUnlockedWrite(t *testing.T) {
	cClient, cServer := net.Pipe()
	defer func() {
		_ = cClient.Close()
		_ = cServer.Close()
	}()

	pr, pw := io.Pipe()
	defer func() {
		_ = pr.Close()
		_ = pw.Close()
	}()

	s := &supervisorSession{
		ringBuf:     NewRingBuffer(1024),
		clients:     make(map[net.Conn]*clientConn),
		stdinWriter: pw,
	}

	cc := &clientConn{
		conn: cServer,
		send: make(chan runnerFrame, 10),
		done: make(chan struct{}),
	}

	go s.handleClient(cc)

	data := []byte("hello stdin")
	msg := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(msg[0:4], uint32(len(data)+1))
	msg[4] = apiv1.ChannelStdin
	copy(msg[5:], data)

	if _, err := cClient.Write(msg); err != nil {
		t.Fatalf("failed to write stdin message: %v", err)
	}

	lockAcquired := make(chan bool, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.stdinMu.Lock()
		lockAcquired <- true
		s.stdinMu.Unlock()
	}()

	select {
	case ok := <-lockAcquired:
		if !ok {
			t.Fatalf("failed to acquire stdinMu lock")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("stdinMu is locked across blocking pipe write")
	}

	buf := make([]byte, len(data))
	if _, err := io.ReadFull(pr, buf); err != nil {
		t.Fatalf("failed reading pipe: %v", err)
	}
	if string(buf) != "hello stdin" {
		t.Fatalf("unexpected pipe output: %q", string(buf))
	}
}

func TestHandleClientEmptyStdinFrameClosesStdin(t *testing.T) {
	cClient, cServer := net.Pipe()
	defer func() {
		_ = cClient.Close()
		_ = cServer.Close()
	}()

	pr, pw := io.Pipe()
	defer func() {
		_ = pr.Close()
		_ = pw.Close()
	}()

	s := &supervisorSession{
		ringBuf:     NewRingBuffer(1024),
		clients:     make(map[net.Conn]*clientConn),
		stdinWriter: pw,
	}

	cc := &clientConn{
		conn: cServer,
		send: make(chan runnerFrame, 10),
		done: make(chan struct{}),
	}

	go s.handleClient(cc)

	// Send empty ChannelStdin frame (length 1)
	msg := make([]byte, 5)
	binary.BigEndian.PutUint32(msg[0:4], 1)
	msg[4] = apiv1.ChannelStdin

	if _, err := cClient.Write(msg); err != nil {
		t.Fatalf("failed to write empty stdin frame: %v", err)
	}

	buf := make([]byte, 10)
	n, err := pr.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on child stdin pipe read, got n=%d err=%v", n, err)
	}

	s.stdinMu.Lock()
	closed := (s.stdinWriter == nil)
	s.stdinMu.Unlock()
	if !closed {
		t.Fatalf("expected supervisor stdinWriter to be nil after EOF frame")
	}
}

func TestBuddyRunnerNonTTYStdinEOFExitsWorkload(t *testing.T) {
	sessionID := "test_nontty_eof_" + time.Now().Format("150405999")
	_ = os.MkdirAll(socketDir, 0700)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(socketDir, sessionID+".sock"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".exit"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".log"))
		_ = os.Remove(filepath.Join(socketDir, sessionID+".stderr.log"))
	})

	doneCh := make(chan error, 1)
	go func() {
		err := runStart([]string{sessionID, "-tty=false", "--", "cat"})
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
		select {
		case err := <-doneCh:
			t.Fatalf("runStart failed immediately: %v; dialErr: %v", err, dialErr)
		default:
			t.Fatalf("failed dialing unix socket: %v", dialErr)
		}
	}
	defer func() { _ = conn.Close() }()

	// Send stdin data to cat
	if err := sendFramedMessage(conn, nil, apiv1.ChannelStdin, []byte("cat_input_data\n")); err != nil {
		t.Fatalf("failed sending stdin: %v", err)
	}

	// Send EOF frame to cat
	if err := sendFramedMessage(conn, nil, apiv1.ChannelStdin, nil); err != nil {
		t.Fatalf("failed sending stdin EOF: %v", err)
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
	if got := receivedStdout.String(); !strings.Contains(got, "cat_input_data") {
		t.Fatalf("expected output containing 'cat_input_data', got: %q", got)
	}
}
