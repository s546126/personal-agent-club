package core

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildSSHPtyCommandValidation(t *testing.T) {
	bin, args, err := BuildSSHPtyCommand(SSHTarget{Target: "ubuntu@muse.ts.net:2222", Transport: TransportSSH})
	if err != nil {
		t.Fatalf("valid ssh target: %v", err)
	}
	if bin != "ssh" {
		t.Fatalf("bin = %q, want ssh", bin)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-tt", "BatchMode=yes", "NumberOfPasswordPrompts=0", "-p 2222", "-- ubuntu@muse.ts.net"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ssh args %v missing %q", args, want)
		}
	}

	tsBin, tsArgs, err := BuildSSHPtyCommand(SSHTarget{Target: "ubuntu@grok-box", Transport: TransportTailscale})
	if err != nil {
		t.Fatalf("valid tailscale target: %v", err)
	}
	if tsBin != "tailscale" || strings.Join(tsArgs, " ") != "ssh -- ubuntu@grok-box" {
		t.Fatalf("tailscale cmd = %s %v", tsBin, tsArgs)
	}

	for _, bad := range []SSHTarget{
		{Target: "", Transport: TransportSSH},
		{Target: "-oProxyCommand=id", Transport: TransportSSH},
		{Target: "user@host;rm -rf /", Transport: TransportSSH},
		{Target: "user@host:99999", Transport: TransportSSH},
		{Target: "user@host:2222", Transport: TransportTailscale},
	} {
		if _, _, err := BuildSSHPtyCommand(bad); err == nil {
			t.Errorf("BuildSSHPtyCommand(%+v) accepted invalid target", bad)
		}
	}
}

func TestPTYManagerRealInteractiveResizeCtrlCAndProcessGroupCleanup(t *testing.T) {
	var mu sync.Mutex
	var outBuf bytes.Buffer
	var lastSeq int
	closedCh := make(chan struct {
		id     string
		reason string
		code   *int
	}, 4)

	mgr := NewPTYManager(PTYConfig{
		MaxSessions: 4,
		IdleTimeout: 5 * time.Minute,
		SweepEvery:  time.Minute,
		OnFrame: func(sessionID string, seq int, data []byte) error {
			mu.Lock()
			if seq <= lastSeq {
				t.Errorf("non-monotonic seq: got %d after %d", seq, lastSeq)
			}
			lastSeq = seq
			outBuf.Write(data)
			mu.Unlock()
			return nil
		},
		OnClosed: func(sessionID string, exitCode *int, reason string) {
			closedCh <- struct {
				id     string
				reason string
				code   *int
			}{id: sessionID, reason: reason, code: exitCode}
		},
	})
	t.Cleanup(func() { mgr.CloseAll("test_cleanup") })

	bin, args, env, dir, err := BuildDemoEchoPtyCommand()
	if err != nil {
		t.Fatalf("BuildDemoEchoPtyCommand: %v", err)
	}
	spec := PTYSessionSpec{
		SessionID:  "pty_test_1",
		Title:      "Echo Demo",
		Bin:        bin,
		Args:       args,
		Env:        env,
		Dir:        dir,
		CleanupDir: dir,
	}
	if err := mgr.Open(spec, 80, 24); err != nil {
		t.Fatalf("Open: %v", err)
	}
	pid, ok := mgr.PID("pty_test_1")
	if !ok || pid <= 0 {
		t.Fatalf("expected valid child PID, got %d (%v)", pid, ok)
	}

	waitOutput := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := outBuf.String()
			mu.Unlock()
			if strings.Contains(got, substr) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		got := outBuf.String()
		mu.Unlock()
		t.Fatalf("timed out waiting for %q in PTY output:\n%s", substr, got)
	}

	// 1. Bidirectional I/O and initial cols x rows via stty size.
	if err := mgr.Input("pty_test_1", []byte("echo PTY_HELLO_MARKER && stty size\n")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	waitOutput("PTY_HELLO_MARKER")
	waitOutput("24 80")

	// 2. Resize PTY to 132x42 and verify kernel TTY reports 42 132.
	if err := mgr.Resize("pty_test_1", 132, 42); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := mgr.Input("pty_test_1", []byte("stty size\n")); err != nil {
		t.Fatalf("Input after resize: %v", err)
	}
	waitOutput("42 132")

	// 3. Ctrl-C interrupts a blocking command cleanly and shell stays responsive.
	if err := mgr.Input("pty_test_1", []byte("sleep 30\n")); err != nil {
		t.Fatalf("Input sleep: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := mgr.Input("pty_test_1", []byte{0x03}); err != nil { // Ctrl-C
		t.Fatalf("Input Ctrl-C: %v", err)
	}
	if err := mgr.Input("pty_test_1", []byte("echo AFTER_CTRL_C\n")); err != nil {
		t.Fatalf("Input after Ctrl-C: %v", err)
	}
	waitOutput("AFTER_CTRL_C")

	// 4. Spawn a background child process in the session, then Close -> entire process group is reaped, no orphans, sandbox cleaned.
	if err := mgr.Input("pty_test_1", []byte("sleep 60 &\n")); err != nil {
		t.Fatalf("Input bg sleep: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := mgr.Close("pty_test_1", "client_closed"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case ev := <-closedCh:
		if ev.id != "pty_test_1" || ev.reason != "client_closed" {
			t.Fatalf("unexpected closed event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnClosed")
	}
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0", mgr.ActiveCount())
	}
	// Verify no process remains alive in the child process group.
	deadline := time.Now().Add(2 * time.Second)
	for processGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if processGroupAlive(pid) {
		t.Fatalf("orphan process group %d still alive after Close", pid)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("demo sandbox dir %s was not cleaned up: %v", dir, statErr)
	}
}

func TestPTYManagerLimitsAndIdleReclamation(t *testing.T) {
	now := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	curNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	closedReasons := map[string]string{}
	var closedMu sync.Mutex

	mgr := NewPTYManager(PTYConfig{
		MaxSessions: 2,
		IdleTimeout: 10 * time.Minute,
		SweepEvery:  time.Hour,
		Now:         curNow,
		OnClosed: func(sessionID string, _ *int, reason string) {
			closedMu.Lock()
			closedReasons[sessionID] = reason
			closedMu.Unlock()
		},
	})
	t.Cleanup(func() { mgr.CloseAll("test_cleanup") })

	openDemo := func(id string) error {
		bin, args, env, dir, err := BuildDemoEchoPtyCommand()
		if err != nil {
			return err
		}
		return mgr.Open(PTYSessionSpec{
			SessionID: id, Title: id, Bin: bin, Args: args, Env: env, Dir: dir, CleanupDir: dir,
		}, 80, 24)
	}

	// Invalid size rejected.
	if err := mgr.Open(PTYSessionSpec{SessionID: "bad", Bin: "/bin/sh"}, 0, 24); !errors.Is(err, ErrPTYInvalidSize) {
		t.Fatalf("invalid size got %v, want ErrPTYInvalidSize", err)
	}
	// MaxSessions=2: first 2 succeed, 3rd refused.
	if err := openDemo("s1"); err != nil {
		t.Fatalf("s1: %v", err)
	}
	if err := openDemo("s2"); err != nil {
		t.Fatalf("s2: %v", err)
	}
	if err := openDemo("s3"); !errors.Is(err, ErrPTYTooManySessions) {
		t.Fatalf("3rd session got %v, want ErrPTYTooManySessions", err)
	}
	// Oversized input > 64 KiB rejected.
	if err := mgr.Input("s1", make([]byte, PTYMaxInputBytes+1)); !errors.Is(err, ErrPTYFrameTooLarge) {
		t.Fatalf("oversized input got %v, want ErrPTYFrameTooLarge", err)
	}
	// Idle sweep before timeout keeps sessions; past timeout reaps them.
	mgr.SweepIdle(now.Add(9 * time.Minute))
	if mgr.ActiveCount() != 2 {
		t.Fatalf("reaped prematurely: %d", mgr.ActiveCount())
	}
	mgr.SweepIdle(now.Add(11 * time.Minute))
	if mgr.ActiveCount() != 0 {
		t.Fatalf("expected 0 active sessions after idle sweep, got %d", mgr.ActiveCount())
	}
	closedMu.Lock()
	r1, r2 := closedReasons["s1"], closedReasons["s2"]
	closedMu.Unlock()
	if r1 != "idle_timeout" || r2 != "idle_timeout" {
		t.Fatalf("idle close reasons = %q, %q; want idle_timeout", r1, r2)
	}
}
