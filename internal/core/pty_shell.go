package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	// DefaultPTYMaxSessions is the default and hard cap on concurrent interactive PTY sessions.
	DefaultPTYMaxSessions = 4
	// DefaultPTYIdleTimeout reaps inactive PTY sessions automatically.
	DefaultPTYIdleTimeout = 30 * time.Minute
	// DefaultPTYSweepInterval is how often the background sweeper checks idle sessions.
	DefaultPTYSweepInterval = time.Minute
	// PTYReadChunkBytes is the per-frame read chunk size (strictly below Persona's 64 KiB frame cap).
	PTYReadChunkBytes = 16 << 10
	// PTYMaxInputBytes is the maximum single input frame accepted.
	PTYMaxInputBytes = 64 << 10
	// PTYMinCols / PTYMaxCols / PTYMinRows / PTYMaxRows match terminal.schema.json bounds.
	PTYMinCols = 1
	PTYMaxCols = 1000
	PTYMinRows = 1
	PTYMaxRows = 1000

	ptyGracefulKillWait = 300 * time.Millisecond
)

var (
	ErrPTYTooManySessions = errors.New("too many concurrent terminal sessions")
	ErrPTYSessionNotFound = errors.New("terminal session not found")
	ErrPTYInvalidSize     = errors.New("terminal cols and rows must be between 1 and 1000")
	ErrPTYFrameTooLarge   = errors.New("terminal input frame exceeds 64 KiB")
	ErrPTYAlreadyExists   = errors.New("terminal session already exists")
)

// PTYConfig configures PTYManager limits and callbacks.
type PTYConfig struct {
	MaxSessions int
	IdleTimeout time.Duration
	SweepEvery  time.Duration
	Now         func() time.Time
	OnFrame     func(sessionID string, seq int, data []byte) error
	OnClosed    func(sessionID string, exitCode *int, reason string)
}

// PTYSessionSpec describes one validated interactive PTY command to start.
// Bin and Args are passed directly to exec.Command (never concatenated through a shell).
type PTYSessionSpec struct {
	SessionID  string
	Title      string
	Bin        string
	Args       []string
	Env        []string
	Dir        string
	CleanupDir string // optional isolated sandbox dir removed after process exit
}

type ptySession struct {
	id         string
	title      string
	ptmx       *os.File
	cmd        *exec.Cmd
	pid        int
	cleanupDir string

	mu          sync.Mutex
	lastActive  time.Time
	closed      bool
	closeReason string
	done        chan struct{}
	closeOnce   sync.Once
}

// PTYManager manages bounded, real-PTY interactive shell sessions with concurrency caps,
// idle timeout reclamation, and full process-group teardown on close.
type PTYManager struct {
	maxSessions int
	idleTimeout time.Duration
	sweepEvery  time.Duration
	now         func() time.Time
	onFrame     func(sessionID string, seq int, data []byte) error
	onClosed    func(sessionID string, exitCode *int, reason string)

	mu       sync.Mutex
	sessions map[string]*ptySession
	stopped  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewPTYManager creates a PTYManager and starts its background idle sweeper.
func NewPTYManager(cfg PTYConfig) *PTYManager {
	maxSess := cfg.MaxSessions
	if maxSess <= 0 || maxSess > DefaultPTYMaxSessions {
		maxSess = DefaultPTYMaxSessions
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = DefaultPTYIdleTimeout
	}
	sweep := cfg.SweepEvery
	if sweep <= 0 {
		sweep = DefaultPTYSweepInterval
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	m := &PTYManager{
		maxSessions: maxSess,
		idleTimeout: idle,
		sweepEvery:  sweep,
		now:         nowFn,
		onFrame:     cfg.OnFrame,
		onClosed:    cfg.OnClosed,
		sessions:    map[string]*ptySession{},
		stopCh:      make(chan struct{}),
	}
	go m.sweepLoop()
	return m
}

func (m *PTYManager) sweepLoop() {
	ticker := time.NewTicker(m.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.SweepIdle(m.now())
		}
	}
}

// BuildSSHPtyCommand validates cfg and returns the executable and argv for an interactive SSH or Tailscale SSH PTY.
// Passwords and private keys are never accepted, written to disk, or passed on the command line.
func BuildSSHPtyCommand(cfg SSHTarget) (string, []string, error) {
	dest, port, err := SplitSSHTarget(cfg.Target)
	if err != nil {
		return "", nil, err
	}
	transport := NormalizeTransport(cfg.Transport)
	if transport == TransportTailscale {
		if port != "" {
			return "", nil, errors.New("tailscale ssh does not support custom :port")
		}
		return "tailscale", []string{"ssh", "--", dest}, nil
	}
	args := []string{
		"-tt",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "NumberOfPasswordPrompts=0",
	}
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, "--", dest)
	return "ssh", args, nil
}

// CleanSSHPTYEnv returns a minimal environment for an SSH/Tailscale PTY without leaking Persona hub tokens.
func CleanSSHPTYEnv() []string {
	env := []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=C.UTF-8",
	}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "SSH_AUTH_SOCK", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// demoSafeCommands are read-only / benign utilities symlinked into the isolated echo demo sandbox PATH.
var demoSafeCommands = []string{
	"echo", "pwd", "ls", "cat", "date", "uname", "whoami", "id",
	"stty", "tput", "clear", "head", "tail", "wc", "sleep", "env",
	"printf", "true", "false",
}

// BuildDemoEchoPtyCommand creates an isolated temporary sandbox directory with a restricted safe command PATH
// and returns an interactive shell spec for reproducible end-to-end PTY testing without external credentials.
func BuildDemoEchoPtyCommand() (bin string, args []string, env []string, dir string, err error) {
	dir, err = os.MkdirTemp("", "ensemble-demo-pty-*")
	if err != nil {
		return "", nil, nil, "", err
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, nil, "", err
	}
	for _, name := range demoSafeCommands {
		if resolved, lookErr := exec.LookPath(name); lookErr == nil {
			_ = os.Symlink(resolved, filepath.Join(binDir, name))
		}
	}
	readme := "Ensemble Demo Interactive PTY Sandbox\nRead-only safe commands available (echo, pwd, ls, uname, stty, date, sleep, exit).\n"
	_ = os.WriteFile(filepath.Join(dir, "README.txt"), []byte(readme), 0o600)

	if bashPath, lookErr := exec.LookPath("bash"); lookErr == nil {
		bin = bashPath
		args = []string{"--norc", "--noprofile", "--restricted"}
	} else {
		bin = "/bin/sh"
		args = nil
	}
	env = []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=C.UTF-8",
		"HOME=" + dir,
		"PWD=" + dir,
		"PATH=" + binDir,
		"PS1=ensemble-demo$ ",
	}
	return bin, args, env, dir, nil
}

func validPTYSize(cols, rows int) bool {
	return cols >= PTYMinCols && cols <= PTYMaxCols && rows >= PTYMinRows && rows <= PTYMaxRows
}

// Open starts a real PTY child process for spec at cols x rows and begins streaming output frames.
func (m *PTYManager) Open(spec PTYSessionSpec, cols, rows int) error {
	if strings.TrimSpace(spec.SessionID) == "" || strings.TrimSpace(spec.Bin) == "" {
		return errors.New("session_id and executable are required")
	}
	if !validPTYSize(cols, rows) {
		return ErrPTYInvalidSize
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return errors.New("pty manager is closed")
	}
	if _, exists := m.sessions[spec.SessionID]; exists {
		m.mu.Unlock()
		return ErrPTYAlreadyExists
	}
	if len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return ErrPTYTooManySessions
	}
	// Reserve slot before starting the child so concurrent Open calls cannot exceed maxSessions.
	placeholder := &ptySession{
		id:         spec.SessionID,
		title:      spec.Title,
		cleanupDir: spec.CleanupDir,
		lastActive: m.now(),
		done:       make(chan struct{}),
	}
	m.sessions[spec.SessionID] = placeholder
	m.mu.Unlock()

	cmd := exec.Command(spec.Bin, spec.Args...)
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, spec.SessionID)
		m.mu.Unlock()
		if spec.CleanupDir != "" {
			_ = os.RemoveAll(spec.CleanupDir)
		}
		return fmt.Errorf("start pty: %w", err)
	}

	placeholder.mu.Lock()
	placeholder.ptmx = ptmx
	placeholder.cmd = cmd
	if cmd.Process != nil {
		placeholder.pid = cmd.Process.Pid
	}
	placeholder.lastActive = m.now()
	alreadyClosed := placeholder.closed
	reason := placeholder.closeReason
	placeholder.mu.Unlock()

	if alreadyClosed {
		// Close was requested while pty.StartWithSize was in flight.
		m.teardownSession(placeholder, reason)
		go m.pump(placeholder)
		return nil
	}

	go m.pump(placeholder)
	return nil
}

func (m *PTYManager) pump(s *ptySession) {
	defer close(s.done)

	buf := make([]byte, PTYReadChunkBytes)
	seq := 0
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			seq++
			s.mu.Lock()
			s.lastActive = m.now()
			s.mu.Unlock()
			if m.onFrame != nil {
				_ = m.onFrame(s.id, seq, chunk)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// On Linux, reading from a PTY master after the slave closes commonly returns EIO.
			}
			break
		}
	}

	// Ensure the entire process group is terminated and reaped before emitting closed.
	_ = s.ptmx.Close()
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- s.cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-time.After(ptyGracefulKillWait):
		_ = killProcessGroup(s.pid, syscall.SIGTERM)
		select {
		case waitErr = <-waitCh:
		case <-time.After(ptyGracefulKillWait):
			_ = killProcessGroup(s.pid, syscall.SIGKILL)
			waitErr = <-waitCh
		}
	}
	// Sweep any lingering background child processes in the session's process group.
	if processGroupAlive(s.pid) {
		_ = killProcessGroup(s.pid, syscall.SIGKILL)
	}

	var exitCode *int
	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
			exitCode = &code
		}
	} else {
		exitCode = &code
	}

	s.mu.Lock()
	s.closed = true
	reason := s.closeReason
	s.mu.Unlock()
	if reason == "" {
		reason = "exited"
	}

	m.mu.Lock()
	if cur, ok := m.sessions[s.id]; ok && cur == s {
		delete(m.sessions, s.id)
	}
	m.mu.Unlock()

	if s.cleanupDir != "" {
		_ = os.RemoveAll(s.cleanupDir)
	}

	s.closeOnce.Do(func() {
		if m.onClosed != nil {
			m.onClosed(s.id, exitCode, reason)
		}
	})
}

func (m *PTYManager) teardownSession(s *ptySession, reason string) {
	s.mu.Lock()
	s.closed = true
	if s.closeReason == "" {
		s.closeReason = reason
	}
	pid := s.pid
	s.mu.Unlock()

	// Signal the child process group BEFORE closing ptmx so we never block on an active ptmx.Read().
	// Interactive shells ignore SIGTERM, so send SIGHUP followed by SIGTERM.
	if pid > 0 {
		_ = killProcessGroup(pid, syscall.SIGHUP)
		_ = killProcessGroup(pid, syscall.SIGTERM)
	}
}

// Input writes raw terminal bytes to the session's PTY master.
func (m *PTYManager) Input(sessionID string, data []byte) error {
	if len(data) > PTYMaxInputBytes {
		return ErrPTYFrameTooLarge
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return ErrPTYSessionNotFound
	}
	s.mu.Lock()
	if s.closed || s.ptmx == nil {
		s.mu.Unlock()
		return ErrPTYSessionNotFound
	}
	ptmx := s.ptmx
	s.lastActive = m.now()
	s.mu.Unlock()

	_, err := ptmx.Write(data)
	return err
}

// Resize updates the PTY window size (sending SIGWINCH to the child process group).
func (m *PTYManager) Resize(sessionID string, cols, rows int) error {
	if !validPTYSize(cols, rows) {
		return ErrPTYInvalidSize
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return ErrPTYSessionNotFound
	}
	s.mu.Lock()
	if s.closed || s.ptmx == nil {
		s.mu.Unlock()
		return ErrPTYSessionNotFound
	}
	ptmx := s.ptmx
	s.lastActive = m.now()
	s.mu.Unlock()

	return pty.Setsize(ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

// Close terminates the PTY session and its entire process group, waiting for clean reap.
func (m *PTYManager) Close(sessionID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "closed"
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	if s != nil {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if s == nil {
		return nil
	}

	m.teardownSession(s, reason)

	// Wait for pump() to reap and kill any remaining process group members.
	select {
	case <-s.done:
	case <-time.After(ptyGracefulKillWait * 2):
		s.mu.Lock()
		pid := s.pid
		s.mu.Unlock()
		if pid > 0 {
			_ = killProcessGroup(pid, syscall.SIGKILL)
		}
		select {
		case <-s.done:
		case <-time.After(time.Second):
		}
	}
	return nil
}

// SweepIdle closes all sessions whose lastActive time is older than idleTimeout.
func (m *PTYManager) SweepIdle(now time.Time) {
	m.mu.Lock()
	var expired []string
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := !s.closed && now.Sub(s.lastActive) >= m.idleTimeout
		s.mu.Unlock()
		if idle {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()
	for _, id := range expired {
		_ = m.Close(id, "idle_timeout")
	}
}

// CloseAll stops the sweeper and cleanly terminates every active PTY session and its process group.
func (m *PTYManager) CloseAll(reason string) {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	m.stopped = true
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Close(id, reason)
	}
}

// ActiveCount returns the number of currently active PTY sessions.
func (m *PTYManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// PID returns the process group leader PID for an active session (used by tests to verify zero orphan processes).
func (m *PTYManager) PID(sessionID string) (int, bool) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid, s.pid > 0
}
