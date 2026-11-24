package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
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

type sessionExitError int

func (e sessionExitError) Error() string { return fmt.Sprintf("session exited with status %d", int(e)) }

type clientConn struct {
	conn net.Conn
	send chan runnerFrame
	done chan struct{}
}

type runnerFrame struct {
	channel byte
	data    []byte
}

func (c *clientConn) writeFrames() {
	defer close(c.done)
	defer func() { _ = c.conn.Close() }()
	for frame := range c.send {
		_ = c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		if err := sendFramedMessage(c.conn, nil, frame.channel, frame.data); err != nil {
			return
		}
	}
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
			var exit sessionExitError
			if errors.As(err, &exit) {
				os.Exit(int(exit))
			}
			fmt.Fprintf(os.Stderr, "Start error: %v\n", err)
			os.Exit(1)
		}
	case "attach":
		if err := runAttach(os.Args[2:]); err != nil {
			var exit sessionExitError
			if errors.As(err, &exit) {
				os.Exit(int(exit))
			}
			fmt.Fprintf(os.Stderr, "Attach error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Status error: %v\n", err)
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
  status <session_id>                     Read the session's completed exit record
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
	logSize      int64
	listener     net.Listener
	clientsMu    sync.Mutex
	clients      map[net.Conn]*clientConn
	finished     bool
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

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
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

	if channel == apiv1.ChannelStdout && len(data) > 0 {
		_, _ = s.ringBuf.Write(data)
	}

	frame := runnerFrame{channel: channel, data: append([]byte(nil), data...)}
	for conn, c := range s.clients {
		select {
		case c.send <- frame:
		default:
			delete(s.clients, conn)
			close(c.send)
			_ = conn.Close()
		}
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
		ptmx, err := pty.StartWithSize(childCmd, ws)
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

		c := &clientConn{conn: conn, send: make(chan runnerFrame, 128), done: make(chan struct{})}
		go s.handleClient(c)
	}
}

func (s *supervisorSession) handleClient(cc *clientConn) {
	defer func() {
		s.clientsMu.Lock()
		if _, registered := s.clients[cc.conn]; registered {
			delete(s.clients, cc.conn)
			close(cc.send)
		}
		s.clientsMu.Unlock()
		_ = cc.conn.Close()
	}()

	s.clientsMu.Lock()
	if s.finished {
		s.clientsMu.Unlock()
		return
	}
	if scrollback := s.ringBuf.Bytes(); len(scrollback) > 0 {
		cc.send <- runnerFrame{channel: apiv1.ChannelStdout, data: scrollback}
	}
	s.clients[cc.conn] = cc
	s.clientsMu.Unlock()
	go cc.writeFrames()

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

var ingestionDrainTimeout = 5 * time.Second

type outputReadState struct {
	mu    sync.Mutex
	since time.Time
}

func (r *outputReadState) read(reader io.Reader, buf []byte) (int, error) {
	r.mu.Lock()
	r.since = time.Now()
	r.mu.Unlock()
	n, err := reader.Read(buf)
	r.mu.Lock()
	r.since = time.Time{}
	r.mu.Unlock()
	return n, err
}

// A child can leave inherited output descriptors open after it exits. Bound
// the wait for an idle read while allowing ingestion to finish processing data.
func (r *outputReadState) drain(reader io.ReadCloser, done <-chan struct{}, idle time.Duration) {
	exited := time.Now()
	ticker := time.NewTicker(idle)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			r.mu.Lock()
			oldRead := !r.since.IsZero() && now.Sub(r.since) >= idle && now.Sub(exited) >= idle
			if oldRead {
				r.mu.Unlock()
				_ = reader.Close()
				<-done
				return
			}
			r.mu.Unlock()
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
	var outputRead outputReadState
	go func() {
		defer close(doneIngestion)
		buf := make([]byte, 16384)
		for {
			n, err := outputRead.read(session.stdoutReader, buf)
			if n > 0 {
				chunk := buf[:n]
				session.logSize, _ = appendBoundedSessionLog(session.logFile, session.logSize, chunk)
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
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				exitCode = 128 + int(status.Signal())
			}
		} else {
			exitCode = 1
		}
	}

	outputRead.drain(session.stdoutReader, doneIngestion, ingestionDrainTimeout)
	if session.logFile != nil {
		_ = session.logFile.Sync()
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

	session.finishClients(exitCode)

	if exitCode != 0 {
		return sessionExitError(exitCode)
	}
	return nil
}

func (s *supervisorSession) finishClients(exitCode int) {
	exitPayload, _ := json.Marshal(apiv1.ExitMessage{ExitCode: exitCode})
	_ = s.listener.Close()
	s.clientsMu.Lock()
	s.finished = true
	clients := make([]*clientConn, 0, len(s.clients))
	for conn, c := range s.clients {
		delete(s.clients, conn)
		select {
		case c.send <- runnerFrame{channel: apiv1.ChannelExit, data: exitPayload}:
			clients = append(clients, c)
		default:
			_ = conn.Close()
		}
		close(c.send)
	}
	s.clientsMu.Unlock()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for _, c := range clients {
		select {
		case <-c.done:
		case <-deadline.C:
			for _, pending := range clients {
				_ = pending.conn.Close()
			}
			return
		}
	}
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

const (
	maxSessionLogBytes            int64 = 1024 * 1024 // 1 MiB replay tail matching in-memory scrollback.
	sessionLogCompactionThreshold       = 2 * maxSessionLogBytes
)

func appendBoundedSessionLog(file *os.File, currentSize int64, data []byte) (int64, error) {
	if len(data) == 0 {
		return currentSize, nil
	}
	if int64(len(data)) >= maxSessionLogBytes {
		data = data[len(data)-int(maxSessionLogBytes):]
		if _, err := file.WriteAt(data, 0); err != nil {
			return currentSize, err
		}
		if err := file.Truncate(maxSessionLogBytes); err != nil {
			return currentSize, err
		}
		return maxSessionLogBytes, nil
	}

	newSize := currentSize + int64(len(data))
	if newSize <= sessionLogCompactionThreshold {
		if _, err := file.WriteAt(data, currentSize); err != nil {
			return currentSize, err
		}
		return newSize, nil
	}

	keepSize := maxSessionLogBytes - int64(len(data))
	oldTail := make([]byte, int(keepSize))
	if keepSize > 0 {
		if _, err := file.ReadAt(oldTail, currentSize-keepSize); err != nil {
			return currentSize, err
		}
	}
	if _, err := file.WriteAt(oldTail, 0); err != nil {
		return currentSize, err
	}
	if _, err := file.WriteAt(data, keepSize); err != nil {
		return currentSize, err
	}
	if err := file.Truncate(maxSessionLogBytes); err != nil {
		return currentSize, err
	}
	return maxSessionLogBytes, nil
}

func replayCompletedSessionLog(w io.Writer, logPath string) error {
	f, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if stat, err := f.Stat(); err == nil && stat.Size() > maxSessionLogBytes {
		if _, err := f.Seek(stat.Size()-maxSessionLogBytes, io.SeekStart); err != nil {
			return err
		}
	}

	buf := make([]byte, 32*1024)
	_, err = io.CopyBuffer(w, f, buf)
	return err
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
		_ = replayCompletedSessionLog(os.Stdout, logPath)
		var rec exitRecord
		if err := json.Unmarshal(exitData, &rec); err == nil {
			if rec.ExitCode != 0 {
				return nil, sessionExitError(rec.ExitCode)
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

	fd := int(os.Stdin.Fd())
	if rawMode && term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("put attach terminal in raw mode: %w", err)
		}
		defer func() { _ = term.Restore(fd, oldState) }()
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
	defer close(done)

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

	for {
		ch, payload, err := readFramedMessage(conn)
		if err != nil {
			return fmt.Errorf("session stream ended before exit: %w", err)
		}
		switch ch {
		case apiv1.ChannelStdout:
			_, _ = os.Stdout.Write(payload)
		case apiv1.ChannelStderr:
			_, _ = os.Stderr.Write(payload)
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err != nil {
				return fmt.Errorf("decode session exit: %w", err)
			}
			if exitMsg.ExitCode != 0 {
				return sessionExitError(exitMsg.ExitCode)
			}
			return nil
		}
	}
}

func runStatus(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fmt.Errorf("session ID is required")
	}
	data, err := os.ReadFile(filepath.Join(socketDir, args[0]+".exit"))
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
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
		if _, exitErr := os.Stat(exitPath); errors.Is(err, os.ErrNotExist) && exitErr == nil {
			return nil
		}
		return fmt.Errorf("read session process record: %w", err)
	}

	var supervisorPID, childPID int
	_, err = fmt.Sscanf(string(data), "%d %d", &supervisorPID, &childPID)
	if err != nil || supervisorPID <= 1 || childPID <= 1 {
		return fmt.Errorf("invalid session process record")
	}

	if err := terminateProcessGroup(childPID, syscall.Kill); err != nil {
		return err
	}

	if supervisorPID > 0 && supervisorPID != os.Getpid() {
		if err := syscall.Kill(supervisorPID, syscall.SIGTERM); err == nil {
			time.Sleep(50 * time.Millisecond)
			if err := syscall.Kill(supervisorPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill session supervisor: %w", err)
			}
		} else if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("terminate session supervisor: %w", err)
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

func terminateProcessGroup(groupID int, signal func(int, syscall.Signal) error) error {
	target := -groupID
	if err := signal(target, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signal session process group: %w", err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		switch err := signal(target, 0); {
		case errors.Is(err, syscall.ESRCH):
			return nil
		case err != nil:
			return fmt.Errorf("check session process group: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := signal(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill session process group: %w", err)
	}
	return nil
}
