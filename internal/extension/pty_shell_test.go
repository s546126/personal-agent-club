package extension

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/echo"
)

func TestTerminalNativeLinkAndMachinesExecApprovedAndDenied(t *testing.T) {
	now := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	muse := &termBoxProvider{quotaTestProvider: quotaTestProvider{name: providerMuse}}
	club := core.New(core.Options{
		Registry:  provider.NewRegistry(muse, echo.New()),
		FollowUps: []time.Duration{},
		Now:       func() time.Time { return now },
	})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())),
		json.RawMessage(`{"demo_echo":true,"browser_providers":false}`))

	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	museAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{
		Provider:    providerMuse,
		Credential:  "fixture-muse-native-cred",
		DisplayName: "Muse Box",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMID := takeoverMachineID(museAcct)

	var machineStatus wire.MachinesStatusResult
	machineStatus = wire.MachinesStatusResult{
		MachineID:  wantMID,
		Name:       "Muse Box",
		Exists:     true,
		Online:     true,
		NodeOnline: true,
		Pending:    false,
	}
	h.Answer("machines.status", func(raw json.RawMessage) (any, error) {
		return machineStatus, nil
	})

	var execCalls []wire.MachinesExecParams
	var execMode string
	h.Answer("machines.exec", func(raw json.RawMessage) (any, error) {
		var p wire.MachinesExecParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		execCalls = append(execCalls, p)
		switch execMode {
		case "deny":
			return nil, &ext.Error{Code: "user_denied", Message: "machines.exec: declined by the owner"}
		case "perm":
			return nil, &ext.Error{Code: "permission_denied", Message: "machines.exec: requires machines.exec"}
		default:
			return wire.MachinesExecResult{
				MachineID:  p.MachineID,
				ExitCode:   0,
				Output:     "\x1b[32mLinux muse-vm 6.8.0\x1b[0m\n" + strings.Repeat("ok-line\n", 10),
				Truncated:  true,
				DurationMs: 42,
			}, nil
		}
	})

	renderTerm := func(locale string) *wire.View {
		var raw json.RawMessage
		if err := h.Call("ui.render", wire.UIParams{
			Contribution: HomeUI,
			Placement:    wire.PlacementNavPage,
			Size:         "full",
			UIVersion:    11,
			Locale:       locale,
			State:        stateOf(pageState{Page: pageTerminal, Account: museAcct.AccountID}),
		}, &raw); err != nil {
			t.Fatalf("ui.render %s: %v", locale, err)
		}
		v, err := wire.DecodeCheckedViewJSON(raw)
		if err != nil {
			t.Fatalf("DecodeCheckedViewJSON %s: %v\n%s", locale, err, raw)
		}
		if strings.HasPrefix(locale, "zh") {
			noJargon(t, "paired terminal "+locale, &v)
		}
		return &v
	}

	// 1. Confirmed paired machine shows "打开原生终端" with persona://terminal/<machine_id> and Confirm in zh-Hans & en.
	vZh := renderTerm("zh-Hans")
	vEn := renderTerm("en")
	wantOpen := "persona://terminal/" + wantMID
	if !strings.Contains(text(vZh), `"open":"`+wantOpen+`"`) || !strings.Contains(text(vZh), "打开原生终端") {
		t.Fatalf("zh terminal view missing native terminal link %q:\n%s", wantOpen, text(vZh))
	}
	if !strings.Contains(text(vEn), `"open":"`+wantOpen+`"`) || !strings.Contains(text(vEn), "Open native terminal") {
		t.Fatalf("en terminal view missing native terminal link %q:\n%s", wantOpen, text(vEn))
	}

	// Verify full extension page keeps persona://terminal/<machine_id>, while overview card strips it.
	pageCopy := *vZh
	if dropped := wire.StripAssistantFieldsForExtensionPage(&pageCopy, ID); len(dropped) != 0 {
		t.Fatalf("full extension page dropped fields: %v", dropped)
	}
	if !strings.Contains(text(&pageCopy), `"open":"`+wantOpen+`"`) {
		t.Fatalf("full extension page stripped valid persona://terminal link")
	}
	cardCopy := *vZh
	dropped := wire.StripAssistantFields(&cardCopy)
	if len(dropped) == 0 || strings.Contains(text(&cardCopy), wantOpen) {
		t.Fatalf("overview card did not strip persona://terminal link: dropped=%v", dropped)
	}

	// 2. Pending machine must NOT expose native terminal open button or machines.exec runner.
	machineStatus.Pending = true
	vPending := renderTerm("zh-Hans")
	if strings.Contains(text(vPending), wantOpen) || strings.Contains(text(vPending), actTermMachineStage) {
		t.Fatalf("pending machine should not expose native terminal or machines.exec:\n%s", text(vPending))
	}
	machineStatus.Pending = false

	// 3. machines.exec two-step stage + owner approval (approved -> output + truncated, denied -> blocked feedback).
	doAct := func(action string, vals map[string]string) wire.ActionResult {
		var res wire.ActionResult
		if err := h.Call("ui.action", wire.UIParams{
			Contribution: HomeUI,
			Placement:    wire.PlacementNavPage,
			Size:         "full",
			UIVersion:    11,
			Locale:       "zh-Hans",
			State:        stateOf(pageState{Page: pageTerminal, Account: museAcct.AccountID}),
			Action:       action,
			Values:       vals,
		}, &res); err != nil {
			t.Fatalf("ui.action %s: %v", action, err)
		}
		if res.View != nil {
			raw, _ := json.Marshal(res.View)
			if _, err := wire.DecodeCheckedViewJSON(raw); err != nil {
				t.Fatalf("action %s invalid view: %v", action, err)
			}
			noJargon(t, "action "+action, res.View)
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

	// Approved run.
	stageRes := doAct(actTermMachineStage, map[string]string{fieldTermMachineCmd: "uname -a"})
	digest := findDigest(stageRes.View)
	if digest == "" {
		t.Fatalf("staged machines.exec missing digest:\n%s", text(stageRes.View))
	}
	execMode = "ok"
	okRes := doAct(actTermMachineExec, map[string]string{valTermDigest: digest})
	if !strings.Contains(okRes.Toast, "已审批执行完成") || !strings.Contains(okRes.Toast, "已截断") {
		t.Fatalf("approved machines.exec toast = %q", okRes.Toast)
	}
	if len(execCalls) != 1 || string(execCalls[0].MachineID) != wantMID || execCalls[0].Command != "uname -a" {
		t.Fatalf("machines.exec calls = %+v", execCalls)
	}
	if !strings.Contains(text(okRes.View), "Linux muse-vm 6.8.0") || strings.Contains(text(okRes.View), "\x1b[") {
		t.Fatalf("machines.exec output not cleaned/shown:\n%s", text(okRes.View))
	}

	// Owner denied run.
	stage2 := doAct(actTermMachineStage, map[string]string{fieldTermMachineCmd: "rm -rf /tmp/x"})
	execMode = "deny"
	denyRes := doAct(actTermMachineExec, map[string]string{valTermDigest: findDigest(stage2.View)})
	if !strings.Contains(denyRes.Toast, "拒绝这次审批") {
		t.Fatalf("denied machines.exec toast = %q", denyRes.Toast)
	}

	// Permission denied run.
	stage3 := doAct(actTermMachineStage, map[string]string{fieldTermMachineCmd: "uptime"})
	execMode = "perm"
	permRes := doAct(actTermMachineExec, map[string]string{valTermDigest: findDigest(stage3.View)})
	if !strings.Contains(permRes.Toast, "尚未授予") {
		t.Fatalf("permission_denied machines.exec toast = %q", permRes.Toast)
	}
}

func TestTerminalSessionsNamespaceEchoPTYOverRPC(t *testing.T) {
	club := core.New(core.Options{
		Registry:  provider.NewRegistry(echo.New()),
		FollowUps: []time.Duration{},
	})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())),
		json.RawMessage(`{"demo_echo":true,"browser_providers":false}`))

	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	echoAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{
		Provider:    echo.Name,
		Credential:  "echo-pty-cred",
		DisplayName: "Local Sandbox",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Render echo terminal page -> contains validated persona://ext-terminal/personal-agent-club/echo:<acctID> button.
	var rawView json.RawMessage
	if err := h.Call("ui.render", wire.UIParams{
		Contribution: HomeUI,
		Placement:    wire.PlacementNavPage,
		Size:         "full",
		UIVersion:    11,
		Locale:       "zh-Hans",
		State:        stateOf(pageState{Page: pageTerminal, Account: echoAcct.AccountID}),
	}, &rawView); err != nil {
		t.Fatalf("ui.render echo terminal: %v", err)
	}
	v, err := wire.DecodeCheckedViewJSON(rawView)
	if err != nil {
		t.Fatalf("echo terminal view invalid: %v\n%s", err, rawView)
	}
	noJargon(t, "echo interactive terminal", &v)
	wantExtURL := "persona://ext-terminal/" + ID + "/echo:" + echoAcct.AccountID
	if !strings.Contains(string(rawView), `"open":"`+wantExtURL+`"`) {
		t.Fatalf("echo terminal view missing ext-terminal open link %q:\n%s", wantExtURL, rawView)
	}

	// Invalid targets must be rejected.
	var openRes wire.TerminalOpenResult
	if err := h.Call(wire.MethodTerminalOpen, wire.TerminalOpenParams{
		SessionID: "bad_1", Target: "../escape", Cols: 80, Rows: 24,
	}, &openRes); err == nil {
		t.Fatal("expected invalid_target rejection")
	}

	// Open real echo PTY session over terminal.open.
	target := "echo:" + echoAcct.AccountID
	if err := h.Call(wire.MethodTerminalOpen, wire.TerminalOpenParams{
		SessionID: "sess_rpc_1", Target: target, Cols: 80, Rows: 24,
	}, &openRes); err != nil {
		t.Fatalf("terminal.open: %v", err)
	}
	if openRes.SessionID != "sess_rpc_1" || !strings.Contains(openRes.Title, "Local Sandbox") {
		t.Fatalf("unexpected open result: %+v", openRes)
	}

	collectFrames := func() string {
		var buf bytes.Buffer
		for _, ev := range h.Events() {
			if ev.Name != wire.EventTerminalFrame {
				continue
			}
			data, err := ext.DecodeTerminalFrameData(ev.Params)
			if err != nil {
				t.Errorf("DecodeTerminalFrameData: %v", err)
				continue
			}
			buf.Write(data)
		}
		return buf.String()
	}

	waitFrame := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(collectFrames(), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %q in terminal.frame output:\n%s", want, collectFrames())
	}

	// Bidirectional input -> frame output.
	if err := h.Call(wire.MethodTerminalInput, wire.TerminalInputParams{
		SessionID: "sess_rpc_1",
		DataB64:   base64.StdEncoding.EncodeToString([]byte("echo ENSEMBLE_RPC_OK && stty size\n")),
	}, nil); err != nil {
		t.Fatalf("terminal.input: %v", err)
	}
	waitFrame("ENSEMBLE_RPC_OK")
	waitFrame("24 80")

	// Resize to 120x36 and confirm via stty size.
	if err := h.Call(wire.MethodTerminalResize, wire.TerminalResizeParams{
		SessionID: "sess_rpc_1", Cols: 120, Rows: 36,
	}, nil); err != nil {
		t.Fatalf("terminal.resize: %v", err)
	}
	if err := h.Call(wire.MethodTerminalInput, wire.TerminalInputParams{
		SessionID: "sess_rpc_1",
		DataB64:   base64.StdEncoding.EncodeToString([]byte("stty size\n")),
	}, nil); err != nil {
		t.Fatalf("terminal.input after resize: %v", err)
	}
	waitFrame("36 120")

	// Close session -> terminal.closed event emitted.
	if err := h.Call(wire.MethodTerminalClose, wire.TerminalCloseParams{
		SessionID: "sess_rpc_1", Reason: "client_closed",
	}, nil); err != nil {
		t.Fatalf("terminal.close: %v", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range h.Events() {
			if ev.Name != wire.EventTerminalClosed {
				continue
			}
			var closed wire.TerminalClosedEvent
			if json.Unmarshal(ev.Params, &closed) == nil && closed.SessionID == "sess_rpc_1" && closed.Reason == "client_closed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for terminal.closed")
}
