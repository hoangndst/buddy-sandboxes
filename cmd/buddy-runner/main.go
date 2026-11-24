package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

const (
	socketDir = "/tmp/buddy"
)

type exitRecord struct {
	ExitCode    int       `json:"exitCode"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
	Error       string    `json:"error,omitempty"`
}

type clientConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "install":
		if err := runInstall(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Install error: %v\n", err)
			os.Exit(1)
		}
	case "start":
		if err := runStart(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Start error: %v\n", err)
			os.Exit(1)
		}
	case "attach":
		if err := runAttach(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Attach error: %v\n", err)
			os.Exit(1)
		}
	case "stop":
		if err := runStop(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Stop error: %v\n", err)
			os.Exit(1)
		}
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: buddy-runner <command> [args...]

Commands:
  install <target_dir>                    Copy supervisor binary to target directory
  start <session_id> [flags] -- <cmd...>  Launch child command under PTY supervision
  attach <session_id> [flags]             Attach stdio to a running supervisor session
  stop <session_id>                       Terminate a running supervisor session
`)
}

func runInstall(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("target directory is required")
	}
	targetDir := args[0]
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	srcFile, err := os.Open(selfPath)
	if err != nil {
		return fmt.Errorf("open self binary: %w", err)
	}
	defer func() { _ = srcFile.Close() }()

	destPath := filepath.Join(targetDir, "buddy-runner")
	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create destination binary: %w", err)
	}
	defer func() { _ = destFile.Close() }()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	return destFile.Chmod(0755)
}

func parseStartArguments(args []string) (string, string, bool, []string, error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	workingDir := fs.String("dir", "", "Working directory")
	useTTY := fs.Bool("tty", true, "Allocate pseudo-terminal")

	sepIdx := -1
	for i, arg := range args {
		if arg == "--" {
			sepIdx = i
			break
		}
	}

	var sessionID string
	var flagArgs []string
	var cmdArgs []string

	if sepIdx != -1 {
		flagArgs = args[:sepIdx]
		cmdArgs = args[sepIdx+1:]
	} else {
		flagArgs = args
	}

	if len(flagArgs) > 0 && flagArgs[0] != "" && flagArgs[0][0] != '-' {
		sessionID = flagArgs[0]
		flagArgs = flagArgs[1:]
	}

	if err := fs.Parse(flagArgs); err != nil {
		return "", "", false, nil, err
	}

	if sessionID == "" {
		return "", "", false, nil, fmt.Errorf("session ID is required")
	}
	if len(cmdArgs) == 0 {
		return "", "", false, nil, fmt.Errorf("child command is required after '--'")
	}

	return sessionID, *workingDir, *useTTY, cmdArgs, nil
}

type supervisorSession struct {
	id           string
	workingDir   string
	useTTY       bool
	cmdArgs      []string
	sockPath     string
	exitPath     string
	logPath      string
	ringBuf      *RingBuffer
	logFile      *os.File
	listener     net.Listener
	clientsMu    sync.Mutex
	clients      map[net.Conn]*clientConn
	ptmx         *os.File
	stdinMu      sync.Mutex
	stdinWriter  io.WriteCloser
	stdoutReader io.ReadCloser
}

func newSupervisorSession(id, workingDir string, useTTY bool, cmdArgs []string) (*supervisorSession, error) {
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	sockPath := filepath.Join(socketDir, id+".sock")
	exitPath := filepath.Join(socketDir, id+".exit")
	logPath := filepath.Join(socketDir, id+".log")

	_ = os.Remove(sockPath)
	_ = os.Remove(exitPath)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("create session log file: %w", err)
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("listen on unix socket %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0600)

	return &supervisorSession{
		id:         id,
		workingDir: workingDir,
		useTTY:     useTTY,
		cmdArgs:    cmdArgs,
		sockPath:   sockPath,
		exitPath:   exitPath,
		logPath:    logPath,
		ringBuf:    NewRingBuffer(1024 * 1024),
		logFile:    logFile,
		listener:   listener,
		clients:    make(map[net.Conn]*clientConn),
	}, nil
}

func (s *supervisorSession) broadcastFrame(channel byte, data []byte) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for _, c := range s.clients {
		_ = sendFramedMessage(c.conn, &c.mu, channel, data)
	}
}

func (s *supervisorSession) startChild() (*exec.Cmd, error) {
	var childCmd *exec.Cmd
	if len(s.cmdArgs) == 1 {
		childCmd = exec.Command(s.cmdArgs[0])
	} else {
		childCmd = exec.Command(s.cmdArgs[0], s.cmdArgs[1:]...)
	}

	if s.workingDir != "" {
		childCmd.Dir = s.workingDir
	}

	if s.useTTY {
		ws := &pty.Winsize{Cols: 80, Rows: 24}
		ptmx, err := pty.StartWithAttrs(childCmd, ws, &syscall.SysProcAttr{Setsid: true})
		if err != nil {
			return childCmd, err
		}
		s.ptmx = ptmx
		s.stdoutReader = ptmx
		s.stdinWriter = ptmx
	} else {
		stdoutR, stdinW, err := startPipeCommand(childCmd)
		if err != nil {
			return childCmd, err
		}
		s.ptmx = nil
		s.stdoutReader = stdoutR
		s.stdinWriter = stdinW
	}
	return childCmd, nil
}

func (s *supervisorSession) runAcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		c := &clientConn{conn: conn}
		s.clientsMu.Lock()
		s.clients[conn] = c
		s.clientsMu.Unlock()

		scrollback := s.ringBuf.Bytes()
		if len(scrollback) > 0 {
			_ = sendFramedMessage(c.conn, &c.mu, apiv1.ChannelStdout, scrollback)
		}

		go s.handleClient(c)
	}
}

func (s *supervisorSession) handleClient(cc *clientConn) {
	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, cc.conn)
		s.clientsMu.Unlock()
		_ = cc.conn.Close()
	}()

	for {
		ch, payload, err := readFramedMessage(cc.conn)
		if err != nil {
			return
		}
		switch ch {
		case apiv1.ChannelStdin:
			if len(payload) > 0 {
				s.stdinMu.Lock()
				if s.stdinWriter != nil {
					_, _ = s.stdinWriter.Write(payload)
				}
				s.stdinMu.Unlock()
			}
		case apiv1.ChannelResize:
			if s.useTTY && s.ptmx != nil && len(payload) > 0 {
				var res apiv1.ResizeMessage
				if err := json.Unmarshal(payload, &res); err == nil {
					cols := res.Cols
					if cols == 0 && res.Width > 0 {
						cols = int(res.Width)
					}
					rows := res.Rows
					if rows == 0 && res.Height > 0 {
						rows = int(res.Height)
					}
					if cols > 0 && rows > 0 {
						_ = pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
					}
				}
			}
		}
	}
}

func runStart(args []string) error {
	sessionID, workingDir, useTTY, cmdArgs, err := parseStartArguments(args)
	if err != nil {
		return err
	}

	session, err := newSupervisorSession(sessionID, workingDir, useTTY, cmdArgs)
	if err != nil {
		return err
	}
	defer func() {
		_ = session.logFile.Close()
		_ = session.listener.Close()
		_ = os.Remove(session.sockPath)
	}()

	startedAt := time.Now().UTC()
	childCmd, childErr := session.startChild()
	if childErr != nil {
		rec := exitRecord{
			ExitCode:    1,
			StartedAt:   startedAt,
			CompletedAt: time.Now().UTC(),
			Error:       childErr.Error(),
		}
		writeExitRecord(session.exitPath, rec)
		return fmt.Errorf("start child command: %w", childErr)
	}
	defer func() {
		if session.stdoutReader != nil {
			_ = session.stdoutReader.Close()
		}
		session.stdinMu.Lock()
		if !session.useTTY && session.stdinWriter != nil {
			_ = session.stdinWriter.Close()
		}
		session.stdinWriter = nil
		session.stdinMu.Unlock()
	}()

	pidPath := filepath.Join(socketDir, sessionID+".pid")
	if childCmd.Process != nil {
		_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d %d", os.Getpid(), childCmd.Process.Pid)), 0600)
		defer func() {
			_ = os.Remove(pidPath)
		}()
	}

	doneIngestion := make(chan struct{})
	go func() {
		defer close(doneIngestion)
		buf := make([]byte, 16384)
		for {
			n, err := session.stdoutReader.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				_, _ = session.ringBuf.Write(chunk)
				_, _ = session.logFile.Write(chunk)
				session.broadcastFrame(apiv1.ChannelStdout, chunk)
			}
			if err != nil {
				return
			}
		}
	}()

	go session.runAcceptLoop()

	waitErr := childCmd.Wait()
	completedAt := time.Now().UTC()
	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	select {
	case <-doneIngestion:
	case <-time.After(200 * time.Millisecond):
	}

	rec := exitRecord{
		ExitCode:    exitCode,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	}
	if waitErr != nil && exitCode != 0 {
		rec.Error = waitErr.Error()
	}
	writeExitRecord(session.exitPath, rec)

	exitPayload, _ := json.Marshal(apiv1.ExitMessage{ExitCode: exitCode})
	session.broadcastFrame(apiv1.ChannelExit, exitPayload)

	_ = session.listener.Close()
	session.clientsMu.Lock()
	for conn := range session.clients {
		_ = conn.Close()
	}
	session.clientsMu.Unlock()

	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func startPipeCommand(cmd *exec.Cmd) (*os.File, *os.File, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, nil, err
	}
	cmd.Stdin = stdinR

	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, nil, err
	}
	_ = stdoutW.Close()
	_ = stdinR.Close()

	return stdoutR, stdinW, nil
}

func parseAttachArguments(args []string) (string, bool, error) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	raw := fs.Bool("raw", true, "Put terminal into raw mode")
	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	remaining := fs.Args()
	if len(remaining) < 1 {
		return "", false, fmt.Errorf("session ID is required")
	}
	return remaining[0], *raw, nil
}

func dialSession(sessionID, sockPath, exitPath, logPath string) (net.Conn, error) {
	deadline := time.Now().Add(3 * time.Second)
	var dialErr error
	for {
		conn, err := net.Dial("unix", sockPath)
		if err == nil {
			return conn, nil
		}
		dialErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if exitData, err := os.ReadFile(exitPath); err == nil {
		if logData, err := os.ReadFile(logPath); err == nil {
			_, _ = os.Stdout.Write(logData)
		}
		var rec exitRecord
		if err := json.Unmarshal(exitData, &rec); err == nil {
			if rec.ExitCode != 0 {
				os.Exit(rec.ExitCode)
			}
			return nil, nil
		}
	}
	return nil, fmt.Errorf("session %s not found or socket unavailable: %w", sessionID, dialErr)
}

func forwardResizeEvents(conn net.Conn, mu *sync.Mutex, done <-chan struct{}) {
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	sendResize := func() {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil && ws != nil {
			res := apiv1.ResizeMessage{Cols: int(ws.Cols), Rows: int(ws.Rows)}
			data, _ := json.Marshal(res)
			_ = sendFramedMessage(conn, mu, apiv1.ChannelResize, data)
		}
	}

	sendResize()

	for {
		select {
		case <-done:
			return
		case <-sigChan:
			sendResize()
		}
	}
}

func runAttach(args []string) error {
	sessionID, rawMode, err := parseAttachArguments(args)
	if err != nil {
		return err
	}
	sockPath := filepath.Join(socketDir, sessionID+".sock")
	exitPath := filepath.Join(socketDir, sessionID+".exit")
	logPath := filepath.Join(socketDir, sessionID+".log")

	if rawMode && term.IsTerminal(int(os.Stdin.Fd())) {
		if oldState, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil && oldState != nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
		}
	}

	conn, err := dialSession(sessionID, sockPath, exitPath, logPath)
	if err != nil {
		return err
	}
	if conn == nil {
		return nil
	}
	defer func() { _ = conn.Close() }()

	var writeMu sync.Mutex
	done := make(chan struct{})

	go forwardResizeEvents(conn, &writeMu, done)

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if err := sendFramedMessage(conn, &writeMu, apiv1.ChannelStdin, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	exitCode := 0
	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			break
		}
		switch ch {
		case apiv1.ChannelStdout:
			_, _ = os.Stdout.Write(payload)
		case apiv1.ChannelStderr:
			_, _ = os.Stderr.Write(payload)
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err == nil {
				exitCode = exitMsg.ExitCode
			}
			close(done)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
			return nil
		}
	}

	close(done)
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func sendFramedMessage(w io.Writer, mu *sync.Mutex, channel byte, data []byte) error {
	totalLen := uint32(1 + len(data))
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[0:4], totalLen)
	header[4] = channel

	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}

	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err := w.Write(data)
		return err
	}
	return nil
}

func readFramedMessage(r io.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	totalLen := binary.BigEndian.Uint32(header[0:4])
	if totalLen < 1 {
		return 0, nil, fmt.Errorf("invalid frame length %d", totalLen)
	}
	channel := header[4]
	dataLen := int(totalLen - 1)
	if dataLen == 0 {
		return channel, nil, nil
	}
	payload := make([]byte, dataLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return channel, payload, nil
}

func writeExitRecord(path string, rec exitRecord) {
	data, err := json.Marshal(rec)
	if err == nil {
		_ = os.WriteFile(path, data, 0600)
	}
}

func runStop(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("session ID is required")
	}
	sessionID := args[0]
	pidPath := filepath.Join(socketDir, sessionID+".pid")
	sockPath := filepath.Join(socketDir, sessionID+".sock")
	exitPath := filepath.Join(socketDir, sessionID+".exit")

	data, err := os.ReadFile(pidPath)
	if err != nil {
		if _, sockErr := os.Stat(sockPath); os.IsNotExist(sockErr) {
			return nil
		}
		_ = os.Remove(sockPath)
		return nil
	}

	var supervisorPID, childPID int
	_, err = fmt.Sscanf(string(data), "%d %d", &supervisorPID, &childPID)
	if err != nil {
		_, _ = fmt.Sscanf(string(data), "%d", &childPID)
	}

	if childPID > 0 {
		if err := syscall.Kill(-childPID, syscall.SIGTERM); err != nil {
			_ = syscall.Kill(childPID, syscall.SIGTERM)
		}
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if childPID > 0 && syscall.Kill(childPID, 0) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if childPID > 0 && syscall.Kill(childPID, 0) == nil {
		if err := syscall.Kill(-childPID, syscall.SIGKILL); err != nil {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}

	if supervisorPID > 0 && supervisorPID != os.Getpid() {
		_ = syscall.Kill(supervisorPID, syscall.SIGTERM)
		time.Sleep(50 * time.Millisecond)
		if syscall.Kill(supervisorPID, 0) == nil {
			_ = syscall.Kill(supervisorPID, syscall.SIGKILL)
		}
	}

	if _, err := os.Stat(exitPath); os.IsNotExist(err) {
		rec := exitRecord{
			ExitCode:    137,
			StartedAt:   time.Now().UTC(),
			CompletedAt: time.Now().UTC(),
			Error:       "terminated by user",
		}
		writeExitRecord(exitPath, rec)
	}

	_ = os.Remove(sockPath)
	_ = os.Remove(pidPath)
	return nil
}
