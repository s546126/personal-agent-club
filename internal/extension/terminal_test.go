package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

type fakeTermRunner struct {
	calls []struct {
		cfg core.SSHTarget
		cmd string
	}
	answer func(core.SSHTarget, string) (termRunOutput, error)
}

func (f *fakeTermRunner) Run(_ context.Context, cfg core.SSHTarget, command string) (termRunOutput, error) {
	f.calls = append(f.calls, struct {
		cfg core.SSHTarget
		cmd string
	}{cfg: cfg, cmd: command})
	if f.answer != nil {
		return f.answer(cfg, command)
	}
	return termRunOutput{Output: "ok", ExitCode: 0, DurationMs: 42}, nil
}

type termBoxProvider struct {
	quotaTestProvider
	box          provider.BoxState
	boxErr       error
	supportsBox  bool
	supportsComp bool
	sentText     []string
	compCmds     []string
	compAnswer   func(string) (provider.ComputerRunResult, error)
}

func (p *termBoxProvider) SupportsBoxState() bool {
	return p.supportsBox || p.name == grokBot || p.name == providerGemini
}

func (p *termBoxProvider) BoxState(context.Context, string) (provider.BoxState, error) {
	if p.boxErr != nil {
		return provider.BoxState{}, p.boxErr
	}
	return p.box, nil
}

func (p *termBoxProvider) SupportsComputer() bool {
	return p.supportsComp || p.name == providerGemini
}

func (p *termBoxProvider) RunComputer(_ context.Context, _, cmd string) (provider.ComputerRunResult, error) {
	p.compCmds = append(p.compCmds, cmd)
	if p.compAnswer != nil {
		return p.compAnswer(cmd)
	}
	return provider.ComputerRunResult{
		Status:     provider.Sent,
		SessionID:  "comp_b8f40e9a12",
		SessionURL: "https://gemini.google.com/app/comp_b8f40e9a12",
		TurnID:     "r_1:u",
		Output:     "Linux localhost 6.6.137+ x86_64",
		DurationMs: 320,
		Answered:   true,
	}, nil
}

func (p *termBoxProvider) Send(_ context.Context, _ string, _ provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	p.sentText = append(p.sentText, msg.Text)
	return provider.SendResult{Status: provider.Sent}, nil
}

func TestTerminalSSHReadOnlyProbeAndTwoStepConfirmation(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	muse := &termBoxProvider{quotaTestProvider: quotaTestProvider{name: providerMuse}}
	grok := &termBoxProvider{
		quotaTestProvider: quotaTestProvider{
			name: grokBot,
			readings: []provider.QuotaReading{{
				Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 0,
				Exhausted: true, ResetsAt: now.Add(2 * time.Hour).UnixMilli(),
			}},
		},
		box: provider.BoxState{State: "paused", Image: "sand:v2", SourceNote: "grok_bot:GetSandBoxRunState"},
	}
	duet := &termBoxProvider{quotaTestProvider: quotaTestProvider{name: "claude_duet"}}

	club := core.New(core.Options{
		Registry:  provider.NewRegistry(muse, grok, duet),
		FollowUps: []time.Duration{},
		Now:       func() time.Time { return now },
	})
	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	museAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: providerMuse, Credential: "fixture-muse-cred"})
	if err != nil {
		t.Fatal(err)
	}
	grokAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: "fixture-grok-cred"})
	if err != nil {
		t.Fatal(err)
	}
	duetAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "claude_duet", Credential: "fixture-duet-cred"})
	if err != nil {
		t.Fatal(err)
	}

	runner := &fakeTermRunner{
		answer: func(_ core.SSHTarget, cmd string) (termRunOutput, error) {
			if cmd == termProbeCommand {
				return termRunOutput{
					Output:     cleanTerminalOutput("\x1b[32mLinux muse-vm 6.8.0 x86_64\x1b[0m\n12:00 up 3 days, load average: 0.12"),
					ExitCode:   0,
					DurationMs: 55,
				}, nil
			}
			return termRunOutput{
				Output:     cleanTerminalOutput("Filesystem Size Used Avail\n/dev/vda1  50G  12G  38G\n" + strings.Repeat("log-line-0123456789\n", 130)),
				ExitCode:   0,
				DurationMs: 88,
			}, nil
		},
	}
	a := &accounts{club: club, terms: newTermState(), termExec: runner}

	checkSchema := func(label string, v *wire.View) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s marshal: %v", label, err)
		}
		if _, err := wire.DecodeCheckedViewJSON(b); err != nil {
			t.Fatalf("%s failed wire.DecodeCheckedViewJSON: %v\n%s", label, err, string(b))
		}
	}
	renderTerm := func(acctID string) *wire.View {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
			State: ext.EncodeState(pageState{Page: pageTerminal, Account: acctID}),
		}}
		v, err := a.render(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		checkSchema("terminal "+acctID, v)
		noJargon(t, "terminal "+acctID, v)
		return v
	}
	doAct := func(acctID, action string, vals map[string]string) *wire.ActionResult {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
			State:  ext.EncodeState(pageState{Page: pageTerminal, Account: acctID}),
			Action: action,
			Values: vals,
		}}
		res, err := a.action(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil && res.View != nil {
			checkSchema("terminal act "+action, res.View)
			noJargon(t, "terminal act "+action, res.View)
		}
		return res
	}

	// 1. Account detail page includes the Remote terminal entry without jargon.
	acctReq := &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
		State: ext.EncodeState(pageState{Page: pageAccount, Account: museAcct.AccountID}),
	}}
	acctView, err := a.render(ctx, acctReq)
	if err != nil {
		t.Fatal(err)
	}
	noJargon(t, "account with terminal entry", acctView)
	if !strings.Contains(text(acctView), "远端终端与命令行") {
		t.Fatalf("account detail missing terminal entry:\n%s", text(acctView))
	}

	// 2. Muse terminal page: notes static snapshot; rejects option injection and Tailscale custom port; saves clean target.
	museInit := renderTerm(museAcct.AccountID)
	if !strings.Contains(text(museInit), "静态配置快照") {
		t.Fatalf("muse terminal missing static snapshot note:\n%s", text(museInit))
	}
	badRes := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "-oProxyCommand=id",
		fieldTermSSHTransport: core.TransportSSH,
	})
	if !strings.Contains(badRes.Toast, "格式不对") {
		t.Fatalf("expected invalid target refusal, got %q", badRes.Toast)
	}
	badPort := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "ubuntu@muse-box:2222",
		fieldTermSSHTransport: core.TransportTailscale,
	})
	if !strings.Contains(badPort.Toast, "Tailscale SSH") {
		t.Fatalf("expected tailscale port refusal, got %q", badPort.Toast)
	}
	saveRes := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "ubuntu@muse-vm.ts.net",
		fieldTermSSHTransport: core.TransportTailscale,
	})
	if !strings.Contains(saveRes.Toast, "已保存 SSH 目标") {
		t.Fatalf("save target toast = %q", saveRes.Toast)
	}

	// 3. Read-only probe runs fixed probe command and strips ANSI escape codes.
	probeRes := doAct(museAcct.AccountID, actTermProbe, nil)
	if !strings.Contains(probeRes.Toast, "只读状态检查通过") {
		t.Fatalf("probe toast = %q", probeRes.Toast)
	}
	if len(runner.calls) != 1 || runner.calls[0].cmd != termProbeCommand {
		t.Fatalf("probe calls = %+v", runner.calls)
	}
	if probeTxt := text(probeRes.View); !strings.Contains(probeTxt, "Linux muse-vm 6.8.0") || strings.Contains(probeTxt, "\x1b[") {
		t.Fatalf("probe output not cleaned: %s", probeTxt)
	}

	// 4. Two-step confirmed command execution: stage -> reject bad digest -> confirm valid digest -> reject replay.
	stageRes := doAct(museAcct.AccountID, actTermStage, map[string]string{fieldTermCmd: "df -h"})
	if !strings.Contains(stageRes.Toast, "已准备好命令") {
		t.Fatalf("stage toast = %q", stageRes.Toast)
	}
	sc, ok := a.term().getStaged(museAcct.AccountID, now)
	if !ok || sc.Command != "df -h" || sc.Digest == "" {
		t.Fatalf("staged = %+v, ok=%v", sc, ok)
	}
	// Staged view must require explicit Confirm dialog on the execute button.
	if !strings.Contains(text(stageRes.View), `"confirm":"确认在 ubuntu@muse-vm.ts.net 上执行这条命令`) {
		t.Fatalf("confirm dialog missing on execute button:\n%s", text(stageRes.View))
	}
	// Wrong digest must refuse without invoking runner.
	wrongRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: "deadbeef"})
	if !strings.Contains(wrongRes.Toast, "过期或已变动") || len(runner.calls) != 1 {
		t.Fatalf("wrong digest: toast=%q calls=%d", wrongRes.Toast, len(runner.calls))
	}
	// Matching digest runs once and records history.
	execRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(execRes.Toast, "退出码 0") || len(runner.calls) != 2 || runner.calls[1].cmd != "df -h" {
		t.Fatalf("exec: toast=%q calls=%+v", execRes.Toast, runner.calls)
	}
	if !strings.Contains(text(execRes.View), "/dev/vda1") {
		t.Fatalf("exec output missing from view:\n%s", text(execRes.View))
	}
	// Replay of the already-consumed digest must be rejected (one-shot).
	replayRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(replayRes.Toast, "过期或已变动") || len(runner.calls) != 2 {
		t.Fatalf("replay was not rejected: toast=%q calls=%d", replayRes.Toast, len(runner.calls))
	}

	// 5. Chat-assisted sandbox execution: succeeds on Muse, blocked by QUOTA_EXHAUSTED on exhausted Grok Bot.
	chatStage := doAct(museAcct.AccountID, actTermChatStage, map[string]string{fieldTermChatCmd: "uname -a"})
	if !strings.Contains(chatStage.Toast, "已准备好命令") {
		t.Fatalf("chat stage toast = %q", chatStage.Toast)
	}
	csc, ok := a.term().getStaged(museAcct.AccountID, now)
	if !ok || csc.Mode != termModeChat {
		t.Fatalf("chat staged = %+v, ok=%v", csc, ok)
	}
	chatSend := doAct(museAcct.AccountID, actTermChatSend, map[string]string{valTermDigest: csc.Digest})
	if !strings.Contains(chatSend.Toast, "已发到主对话") || len(muse.sentText) != 1 || !strings.Contains(muse.sentText[0], "uname -a") {
		t.Fatalf("chat send: toast=%q sent=%+v", chatSend.Toast, muse.sentText)
	}

	// Grok Bot view shows read-only sandbox state ("已休眠") and blocks chat send when quota is exhausted.
	grokView := renderTerm(grokAcct.AccountID)
	grokTxt := text(grokView)
	for _, want := range []string{"云端沙箱状态（只读）", "已休眠", "sand:v2", "当前额度已用完"} {
		if !strings.Contains(grokTxt, want) {
			t.Errorf("grok terminal view lacks %q:\n%s", want, grokTxt)
		}
	}
	_ = doAct(grokAcct.AccountID, actTermChatStage, map[string]string{fieldTermChatCmd: "uptime"})
	gsc, _ := a.term().getStaged(grokAcct.AccountID, now)
	grokBlocked := doAct(grokAcct.AccountID, actTermChatSend, map[string]string{valTermDigest: gsc.Digest})
	if !strings.Contains(grokBlocked.Toast, "额度已用完") || len(grok.sentText) != 0 {
		t.Fatalf("exhausted grok chat send was not blocked: toast=%q sent=%d", grokBlocked.Toast, len(grok.sentText))
	}

	// 6. Non-terminal hosted service (Claude Duet): clearly marked unsupported + capabilities note + no SSH form.
	duetView := renderTerm(duetAcct.AccountID)
	duetTxt := text(duetView)
	for _, want := range []string{"不支持终端", "托管网页服务", "nwverse/persona#16741", "能力 1", "能力 2", "能力 3"} {
		if !strings.Contains(duetTxt, want) {
			t.Errorf("duet terminal view lacks %q:\n%s", want, duetTxt)
		}
	}
	if strings.Contains(duetTxt, "保存 SSH 目标") {
		t.Errorf("non-terminal service should not offer SSH form:\n%s", duetTxt)
	}
}

func TestTerminalGeminiCloudComputerWebShell(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	taskOn := true
	netOn := false
	gemini := &termBoxProvider{
		quotaTestProvider: quotaTestProvider{name: providerGemini},
		box: provider.BoxState{
			State:          "running",
			Image:          "Debian 12 · 2 vCPU · 8 GB",
			TaskMode:       &taskOn,
			InternetAccess: &netOn,
			Running:        1,
			Waiting:        0,
			Ready:          2,
			SettingsURL:    "https://gemini.google.com/gemini-spark",
			SessionID:      "comp_b8f40e9a12",
			SessionTitle:   "云电脑探针会话",
			SessionURL:     "https://gemini.google.com/app/comp_b8f40e9a12",
		},
	}

	club := core.New(core.Options{
		Registry:  provider.NewRegistry(gemini),
		FollowUps: []time.Duration{},
		Now:       func() time.Time { return now },
	})
	t.Cleanup(club.Close)
	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	gAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: providerGemini, Credential: "fixture-gemini-cred"})
	if err != nil {
		t.Fatal(err)
	}

	a := &accounts{club: club, terms: newTermState()}
	checkSchema := func(label string, v *wire.View) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s marshal: %v", label, err)
		}
		if _, err := wire.DecodeCheckedViewJSON(b); err != nil {
			t.Fatalf("%s failed wire.DecodeCheckedViewJSON: %v\n%s", label, err, string(b))
		}
	}
	renderLocale := func(locale string) *wire.View {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: locale,
			State: ext.EncodeState(pageState{Page: pageTerminal, Account: gAcct.AccountID}),
		}}
		v, err := a.render(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		checkSchema("gemini terminal "+locale, v)
		if strings.HasPrefix(locale, "zh") {
			noJargon(t, "gemini terminal "+locale, v)
		}
		return v
	}
	doAct := func(action string, vals map[string]string) *wire.ActionResult {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
			State:  ext.EncodeState(pageState{Page: pageTerminal, Account: gAcct.AccountID}),
			Action: action,
			Values: vals,
		}}
		res, err := a.action(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil && res.View != nil {
			checkSchema("gemini terminal act "+action, res.View)
			noJargon(t, "gemini terminal act "+action, res.View)
		}
		return res
	}

	// 1. Account detail page renders the cloud computer entry with "云电脑" badge.
	acctReq := &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
		State: ext.EncodeState(pageState{Page: pageAccount, Account: gAcct.AccountID}),
	}}
	acctView, err := a.render(ctx, acctReq)
	if err != nil {
		t.Fatal(err)
	}
	checkSchema("gemini account detail", acctView)
	noJargon(t, "gemini account detail", acctView)
	if !strings.Contains(text(acctView), "云电脑") {
		t.Fatalf("gemini account detail missing cloud computer badge:\n%s", text(acctView))
	}

	// 2. Initial terminal view (InternetAccess=false): shows status card, clear Chinese hint without flipping switch,
	// gemini-spark link and active session link, and passes schema + noJargon in both zh-Hans and en.
	vZh := renderLocale("zh-Hans")
	_ = renderLocale("en")
	txtZh := text(vZh)
	for _, want := range []string{
		"Gemini 云电脑（任务模式）",
		"2 核 CPU / 8 GB 内存 / Debian 12",
		"远程计算机的网络访问权限（只读）",
		"未开启",
		"不会擅自修改你的开关",
		"https://gemini.google.com/gemini-spark",
		"https://gemini.google.com/app/comp_b8f40e9a12",
		"云电脑命令行（经二次确认执行）",
	} {
		if !strings.Contains(txtZh, want) {
			t.Errorf("gemini terminal view (internet off) lacks %q:\n%s", want, txtZh)
		}
	}

	// 3. Flip internet on and test read-only refresh action (plus human-check / auth error classification).
	netOn = true
	refRes := doAct(actTermComputerRefresh, nil)
	if !strings.Contains(refRes.Toast, "只读状态检查通过") {
		t.Fatalf("refresh toast = %q", refRes.Toast)
	}
	if !strings.Contains(text(refRes.View), "已允许远程计算机在任务中访问外网") {
		t.Fatalf("refreshed view did not show internet enabled:\n%s", text(refRes.View))
	}
	gemini.boxErr = &provider.Error{Code: provider.CodeHumanCheck}
	hcRes := doAct(actTermComputerRefresh, nil)
	if !strings.Contains(hcRes.Toast, "我不是机器人") {
		t.Fatalf("human check refresh toast = %q", hcRes.Toast)
	}
	gemini.boxErr = nil

	// 4. Preset shortcut whitelist: unknown preset rejected, valid read-only preset stages with SHA-256 digest.
	badPreset := doAct(actTermComputerPreset, map[string]string{valTermPreset: "rm -rf /"})
	if !strings.Contains(badPreset.Toast, "不在只读预设清单里") {
		t.Fatalf("expected preset whitelist refusal, got %q", badPreset.Toast)
	}
	presetRes := doAct(actTermComputerPreset, map[string]string{valTermPreset: geminiPresetSystem})
	if !strings.Contains(presetRes.Toast, "已准备好命令") {
		t.Fatalf("preset stage toast = %q", presetRes.Toast)
	}
	sc, ok := a.term().getStaged(gAcct.AccountID, now)
	if !ok || sc.Mode != termModeComputer || sc.Command != geminiPresetSystem || sc.Digest == "" {
		t.Fatalf("staged preset = %+v, ok=%v", sc, ok)
	}
	if !strings.Contains(text(presetRes.View), `"confirm":"确认在`) {
		t.Fatalf("staged computer view missing Confirm dialog:\n%s", text(presetRes.View))
	}

	// 5. Two-step confirmation: wrong digest rejected -> valid digest executes once -> cleans ANSI & caps at 16 KB -> replay rejected.
	wrongDigest := doAct(actTermComputerRun, map[string]string{valTermDigest: "000000000000"})
	if !strings.Contains(wrongDigest.Toast, "过期或已变动") || len(gemini.compCmds) != 0 {
		t.Fatalf("wrong digest was not rejected: toast=%q calls=%d", wrongDigest.Toast, len(gemini.compCmds))
	}
	gemini.compAnswer = func(cmd string) (provider.ComputerRunResult, error) {
		return provider.ComputerRunResult{
			Status:     provider.Sent,
			SessionID:  "comp_b8f40e9a12",
			SessionURL: "https://gemini.google.com/app/comp_b8f40e9a12",
			TurnID:     "r_10:u",
			Output:     "\x1b[32mLinux localhost 6.6.137+ x86_64\x1b[0m\n2\n" + strings.Repeat("mem-line-0123456789\n", 900),
			DurationMs: 410,
			Answered:   true,
		}, nil
	}
	runRes := doAct(actTermComputerRun, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(runRes.Toast, "已在云电脑执行完成") || len(gemini.compCmds) != 1 || gemini.compCmds[0] != geminiPresetSystem {
		t.Fatalf("computer run: toast=%q cmds=%+v", runRes.Toast, gemini.compCmds)
	}
	runTxt := text(runRes.View)
	if !strings.Contains(runTxt, "Linux localhost 6.6.137+ x86_64") || strings.Contains(runTxt, "\x1b[") || !strings.Contains(runTxt, "超过 16 KB，已截断") {
		t.Fatalf("computer output not cleaned or truncated properly:\n%s", runTxt)
	}
	if len(gemini.sentText) != 0 {
		t.Fatalf("computer run must never send into main chat: sentText=%+v", gemini.sentText)
	}
	// Replay of the same digest must be rejected.
	replay := doAct(actTermComputerRun, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(replay.Toast, "过期或已变动") || len(gemini.compCmds) != 1 {
		t.Fatalf("replay was not rejected: toast=%q calls=%d", replay.Toast, len(gemini.compCmds))
	}

	// 6. Unconfirmed run records caution and prevents blind resend.
	_ = doAct(actTermComputerStage, map[string]string{fieldTermComputerCmd: "ls -la"})
	sc2, _ := a.term().getStaged(gAcct.AccountID, now)
	gemini.compAnswer = func(string) (provider.ComputerRunResult, error) {
		return provider.ComputerRunResult{
			Status:     provider.Unconfirmed,
			SessionID:  "comp_b8f40e9a12",
			SessionURL: "https://gemini.google.com/app/comp_b8f40e9a12",
			DurationMs: 900,
		}, nil
	}
	unconfRes := doAct(actTermComputerRun, map[string]string{valTermDigest: sc2.Digest})
	if !strings.Contains(unconfRes.Toast, "不要直接重发") {
		t.Fatalf("unconfirmed toast = %q", unconfRes.Toast)
	}

	// 7. Quota exhaustion blocks execution before consuming the staged command.
	gemini.readings = []provider.QuotaReading{{
		Kind: protocol.QuotaKindWindow, Window: "5h", Limit: 100, Remaining: 0,
		Exhausted: true, ResetsAt: now.Add(2 * time.Hour).UnixMilli(),
	}}
	if err := space.SyncAccount(ctx, gAcct.AccountID); err != nil {
		t.Fatal(err)
	}
	_ = doAct(actTermComputerStage, map[string]string{fieldTermComputerCmd: "python3 --version"})
	sc3, _ := a.term().getStaged(gAcct.AccountID, now)
	prevCalls := len(gemini.compCmds)
	blockedRes := doAct(actTermComputerRun, map[string]string{valTermDigest: sc3.Digest})
	if !strings.Contains(blockedRes.Toast, "额度已用完") || len(gemini.compCmds) != prevCalls {
		t.Fatalf("exhausted quota did not block computer run: toast=%q calls=%d", blockedRes.Toast, len(gemini.compCmds))
	}
}
