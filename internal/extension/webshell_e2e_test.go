package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
)

// TestWebShellRealSubprocessE2E exercises the real compiled personal-agent-club binary over OS stdin/stdout pipes
// speaking the exact Persona Phase 1 Hub extension protocol:
//  1. Manifest + initialize handshake at version 0.7.14 with terminal_sessions & machines.exec.
//  2. Paired Persona machine native terminal link (persona://terminal/<machine_id>): kept on full extension page,
//     stripped by overview-card sanitization.
//  3. Owner-gated machines.exec: approved call returns cleaned stdout/stderr + truncation; declined call blocks execution.
//  4. Real interactive PTY WebShell over terminal.open / terminal.input / terminal.resize / terminal.close +
//     terminal.frame / terminal.closed streaming, verifying kernel stty size change (80x24 -> 132x42) and zero orphan
//     child processes after close.
func TestWebShellRealSubprocessE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess e2e in -short mode")
	}

	// Allow caller to point at a prebuilt binary (e.g. /tmp/webshell-e2e/personal-agent-club);
	// otherwise build into t.TempDir() so `go test ./...` is completely self-contained.
	bin := os.Getenv("ENSEMBLE_E2E_BIN")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "personal-agent-club")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/personal-agent-club")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build personal-agent-club: %v\n%s", err, out)
		}
	}

	dataDir := t.TempDir()
	extDir := t.TempDir()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"PERSONA_EXTENSION_ID="+ID,
		"PERSONA_EXTENSION_DATA="+dataDir,
		"PERSONA_RUNTIME_SOCKET=", // never dial host live socket
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start personal-agent-club: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	type rpcEnvelope struct {
		ID     string          `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Event  string          `json:"event,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *ext.Error      `json:"error,omitempty"`
	}

	var writeMu sync.Mutex
	sendFrame := func(f rpcEnvelope) error {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = stdin.Write(append(b, '\n'))
		return err
	}

	var (
		nextID       atomic.Int64
		pendingMu    sync.Mutex
		pending      = map[string]chan rpcEnvelope{}
		cbMu         sync.Mutex
		cbHandlers   = map[string]func(json.RawMessage) (any, error){}
		frameMu      sync.Mutex
		frameOut     bytes.Buffer
		frameCount   int
		closedEvents = make(chan wire.TerminalClosedEvent, 8)
	)

	setCallback := func(method string, fn func(json.RawMessage) (any, error)) {
		cbMu.Lock()
		cbHandlers[method] = fn
		cbMu.Unlock()
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			var f rpcEnvelope
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				continue
			}
			switch {
			case f.Method != "":
				cbMu.Lock()
				fn := cbHandlers[f.Method]
				cbMu.Unlock()
				go func(req rpcEnvelope, h func(json.RawMessage) (any, error)) {
					resp := rpcEnvelope{ID: req.ID}
					if h == nil {
						resp.Error = &ext.Error{Code: wire.ErrorCodeUnknownMethod, Message: "unhandled " + req.Method}
					} else {
						res, err := h(req.Params)
						if err != nil {
							if e, ok := err.(*ext.Error); ok {
								resp.Error = e
							} else {
								resp.Error = &ext.Error{Code: "internal", Message: err.Error()}
							}
						} else {
							raw, _ := json.Marshal(res)
							resp.Result = raw
						}
					}
					_ = sendFrame(resp)
				}(f, fn)

			case f.Event != "":
				switch f.Event {
				case wire.EventTerminalFrame:
					if raw, err := ext.DecodeTerminalFrameData(f.Params); err == nil {
						frameMu.Lock()
						frameOut.Write(raw)
						frameCount++
						frameMu.Unlock()
					}
				case wire.EventTerminalClosed:
					var ev wire.TerminalClosedEvent
					if json.Unmarshal(f.Params, &ev) == nil {
						closedEvents <- ev
					}
				}

			case f.ID != "":
				pendingMu.Lock()
				ch := pending[f.ID]
				delete(pending, f.ID)
				pendingMu.Unlock()
				if ch != nil {
					ch <- f
				}
			}
		}
	}()

	call := func(method string, params, out any) error {
		id := fmt.Sprintf("e2e_%d", nextID.Add(1))
		ch := make(chan rpcEnvelope, 1)
		pendingMu.Lock()
		pending[id] = ch
		pendingMu.Unlock()
		var raw json.RawMessage
		if params != nil {
			b, err := json.Marshal(params)
			if err != nil {
				return err
			}
			raw = b
		}
		if err := sendFrame(rpcEnvelope{ID: id, Method: method, Params: raw}); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return resp.Error
			}
			if out != nil && len(resp.Result) > 0 {
				return json.Unmarshal(resp.Result, out)
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("%s timed out (stderr: %s)", method, stderr.String())
		}
	}

	// 1. Handshake: initialize with demo_echo enabled and machines.exec granted.
	info := &wire.ExtensionInfo{ID: ID, Version: pac.Version(), Dir: extDir, DataDir: dataDir}
	var initRes wire.InitializeResult
	if err := call("initialize", wire.InitializeParams{
		Protocol:  ext.Protocol,
		Extension: info,
		Plugin:    info,
		Hub: wire.HubInfo{
			URL:    "http://127.0.0.1:1",
			Token:  "e2e-token",
			Scopes: []string{"machines.read", "machines.exec", "notify"},
		},
		Config: json.RawMessage(`{"demo_echo":true,"browser_providers":false,"assistant_runtime":false}`),
	}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if initRes.ID != ID || initRes.Version != "0.7.14" {
		t.Fatalf("initialize result = %+v, want id=%s version=0.7.14", initRes, ID)
	}

	// 2. Pair a demo echo account through the real UI action so we have a real stored account in SQLite.
	var pairRes wire.ActionResult
	if err := call("ui.action", wire.UIParams{
		Contribution: HomeUI,
		Placement:    wire.PlacementNavPage,
		Size:         "full",
		UIVersion:    11,
		Locale:       "zh-Hans",
		State:        stateOf(pageState{Page: pagePair, Provider: "echo"}),
		Action:       actPair,
		Values: map[string]string{
			fieldName:       "E2E Sandbox",
			fieldCredential: "echo-e2e-credential-1234",
		},
	}, &pairRes); err != nil {
		t.Fatalf("pair echo account: %v", err)
	}
	var pairedState pageState
	if pairRes.View == nil || json.Unmarshal([]byte(pairRes.View.State), &pairedState) != nil || pairedState.Account == "" {
		t.Fatalf("failed to obtain paired account id from state: %+v", pairRes)
	}
	acctID := pairedState.Account
	wantMID := "pac-echo-" + strings.ToLower(acctID)

	// 3. Register Hub machines.status returning confirmed online machine -> verify persona://terminal/<id> on full page vs card strip.
	setCallback("machines.status", func(raw json.RawMessage) (any, error) {
		var p wire.MachinesStatusParams
		_ = json.Unmarshal(raw, &p)
		return wire.MachinesStatusResult{
			MachineID:  p.MachineID,
			Name:       "E2E Box",
			Exists:     true,
			Online:     true,
			NodeOnline: true,
			Pending:    false,
		}, nil
	})

	var rawTermView json.RawMessage
	if err := call("ui.render", wire.UIParams{
		Contribution: HomeUI,
		Placement:    wire.PlacementNavPage,
		Size:         "full",
		UIVersion:    11,
		Locale:       "zh-Hans",
		State:        stateOf(pageState{Page: pageTerminal, Account: acctID}),
	}, &rawTermView); err != nil {
		t.Fatalf("ui.render terminal: %v", err)
	}
	termView, err := wire.DecodeCheckedViewJSON(rawTermView)
	if err != nil {
		t.Fatalf("DecodeCheckedViewJSON terminal: %v\n%s", err, rawTermView)
	}
	noJargon(t, "e2e terminal", &termView)
	wantNativeOpen := "persona://terminal/" + wantMID
	if !strings.Contains(string(rawTermView), `"open":"`+wantNativeOpen+`"`) {
		t.Fatalf("full page missing native terminal link %q:\n%s", wantNativeOpen, rawTermView)
	}
	fullCopy := termView
	if dropped := wire.StripAssistantFieldsForExtensionPage(&fullCopy, ID); len(dropped) != 0 {
		t.Fatalf("full extension page dropped actions: %v", dropped)
	}
	cardCopy := termView
	if dropped := wire.StripAssistantFields(&cardCopy); len(dropped) == 0 || strings.Contains(text(&cardCopy), wantNativeOpen) {
		t.Fatalf("overview card did not strip persona://terminal link: dropped=%v", dropped)
	}

	// 4. Verify machines.exec approval (allow -> output returned) and denial (user_denied -> blocked).
	var approveNext atomic.Bool
	approveNext.Store(true)
	var execObserved []wire.MachinesExecParams
	setCallback("machines.exec", func(raw json.RawMessage) (any, error) {
		var p wire.MachinesExecParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		execObserved = append(execObserved, p)
		if !approveNext.Load() {
			return nil, &ext.Error{Code: "user_denied", Message: "machines.exec: declined by the owner"}
		}
		return wire.MachinesExecResult{
			MachineID:  p.MachineID,
			ExitCode:   0,
			Output:     "E2E_MACHINES_EXEC_APPROVED_OUTPUT",
			Truncated:  false,
			DurationMs: 25,
		}, nil
	})

	doAct := func(action string, vals map[string]string) wire.ActionResult {
		var res wire.ActionResult
		if err := call("ui.action", wire.UIParams{
			Contribution: HomeUI,
			Placement:    wire.PlacementNavPage,
			Size:         "full",
			UIVersion:    11,
			Locale:       "zh-Hans",
			State:        stateOf(pageState{Page: pageTerminal, Account: acctID}),
			Action:       action,
			Values:       vals,
		}, &res); err != nil {
			t.Fatalf("ui.action %s: %v", action, err)
		}
		return res
	}
	findDigest := func(v *wire.View) string {
		for _, s := range v.Sections {
			for _, c := range s.Items {
				if c.Action != nil && c.Action.ID == actTermMachineExec {
					return c.Action.Values[valTermDigest]
				}
			}
		}
		return ""
	}

	st1 := doAct(actTermMachineStage, map[string]string{fieldTermMachineCmd: "uname -srm"})
	r1 := doAct(actTermMachineExec, map[string]string{valTermDigest: findDigest(st1.View)})
	if !strings.Contains(r1.Toast, "已审批执行完成") || !strings.Contains(text(r1.View), "E2E_MACHINES_EXEC_APPROVED_OUTPUT") {
		t.Fatalf("machines.exec approved failed: toast=%q", r1.Toast)
	}

	approveNext.Store(false)
	st2 := doAct(actTermMachineStage, map[string]string{fieldTermMachineCmd: "whoami"})
	r2 := doAct(actTermMachineExec, map[string]string{valTermDigest: findDigest(st2.View)})
	if !strings.Contains(r2.Toast, "拒绝这次审批") {
		t.Fatalf("machines.exec denied failed: toast=%q", r2.Toast)
	}
	if len(execObserved) != 2 {
		t.Fatalf("expected 2 machines.exec calls, got %d", len(execObserved))
	}

	// 5. Real interactive PTY WebShell over terminal.* RPC on the live subprocess!
	var openRes wire.TerminalOpenResult
	if err := call(wire.MethodTerminalOpen, wire.TerminalOpenParams{
		SessionID: "extterm_e2e_1",
		Target:    "echo:" + acctID,
		Cols:      80,
		Rows:      24,
	}, &openRes); err != nil {
		t.Fatalf("terminal.open: %v", err)
	}

	waitPTY := func(want string) string {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			frameMu.Lock()
			s := frameOut.String()
			frameMu.Unlock()
			if strings.Contains(s, want) {
				return s
			}
			time.Sleep(25 * time.Millisecond)
		}
		frameMu.Lock()
		s := frameOut.String()
		frameMu.Unlock()
		t.Fatalf("timed out waiting for %q in real subprocess PTY frames:\n%s", want, s)
		return ""
	}

	// Print shell PID + initial stty size (24 80).
	if err := call(wire.MethodTerminalInput, wire.TerminalInputParams{
		SessionID: "extterm_e2e_1",
		DataB64:   base64.StdEncoding.EncodeToString([]byte("echo PTY_PID=$$ && stty size\n")),
	}, nil); err != nil {
		t.Fatalf("terminal.input initial: %v", err)
	}
	out1 := waitPTY("24 80")
	pidMatch := regexp.MustCompile(`PTY_PID=(\d+)`).FindStringSubmatch(out1)
	if len(pidMatch) != 2 {
		t.Fatalf("could not parse PTY_PID from output:\n%s", out1)
	}
	shellPID, _ := strconv.Atoi(pidMatch[1])
	if shellPID <= 1 {
		t.Fatalf("invalid shellPID %d", shellPID)
	}

	// Resize to 132x42 and verify kernel TTY updated via stty size.
	if err := call(wire.MethodTerminalResize, wire.TerminalResizeParams{
		SessionID: "extterm_e2e_1",
		Cols:      132,
		Rows:      42,
	}, nil); err != nil {
		t.Fatalf("terminal.resize: %v", err)
	}
	if err := call(wire.MethodTerminalInput, wire.TerminalInputParams{
		SessionID: "extterm_e2e_1",
		DataB64:   base64.StdEncoding.EncodeToString([]byte("stty size\n")),
	}, nil); err != nil {
		t.Fatalf("terminal.input post-resize: %v", err)
	}
	waitPTY("42 132")

	// Start a background child process inside the PTY, then terminal.close -> verify terminal.closed AND zero orphan PIDs!
	if err := call(wire.MethodTerminalInput, wire.TerminalInputParams{
		SessionID: "extterm_e2e_1",
		DataB64:   base64.StdEncoding.EncodeToString([]byte("sleep 60 &\n")),
	}, nil); err != nil {
		t.Fatalf("terminal.input bg job: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := call(wire.MethodTerminalClose, wire.TerminalCloseParams{
		SessionID: "extterm_e2e_1",
		Reason:    "e2e_done",
	}, nil); err != nil {
		t.Fatalf("terminal.close: %v", err)
	}
	select {
	case ev := <-closedEvents:
		if ev.SessionID != "extterm_e2e_1" || ev.Reason != "e2e_done" {
			t.Fatalf("unexpected terminal.closed: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal.closed from subprocess")
	}

	// Verify the shell PID and its process group are completely gone (no orphans).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(shellPID, 0); err == syscall.ESRCH {
			if errPG := syscall.Kill(-shellPID, 0); errPG == syscall.ESRCH {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := syscall.Kill(shellPID, 0); err != syscall.ESRCH {
		t.Fatalf("PTY shell PID %d still alive after terminal.close (err=%v)", shellPID, err)
	}
	if err := syscall.Kill(-shellPID, 0); err != syscall.ESRCH {
		t.Fatalf("PTY process group -%d still alive after terminal.close (err=%v)", shellPID, err)
	}

	// Cleanly shut down subprocess by closing stdin.
	_ = stdin.Close()
	frameMu.Lock()
	totalFrames := frameCount
	frameMu.Unlock()
	t.Logf("E2E passed: version=%s, nativeOpen=%s, machinesExecCalls=%d, ptyFrames=%d, reapedPID=%d",
		initRes.Version, wantNativeOpen, len(execObserved), totalFrames, shellPID)
	_ = io.EOF
}
