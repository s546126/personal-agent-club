package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/cue"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/provider/groq"
	"github.com/nwverse/ensemble/internal/provider/muse2api"
)

const (
	pageTerminal = "terminal"

	actTermSaveSSH         = "term_save_ssh"
	actTermClearSSH        = "term_clear_ssh"
	actTermProbe           = "term_probe"
	actTermStage           = "term_stage"
	actTermPreset          = "term_preset"
	actTermExec            = "term_exec"
	actTermCancel          = "term_cancel"
	actTermChatStage       = "term_chat_stage"
	actTermChatSend        = "term_chat_send"
	actTermComputerRefresh = "term_computer_refresh"
	actTermComputerStage   = "term_computer_stage"
	actTermComputerPreset  = "term_computer_preset"
	actTermComputerRun     = "term_computer_run"
	actTermMachineStage    = "term_machine_stage"
	actTermMachinePreset   = "term_machine_preset"
	actTermMachineExec     = "term_machine_exec"

	fieldTermSSHTarget    = "term_ssh_target"
	fieldTermSSHTransport = "term_ssh_transport"
	fieldTermCmd          = "term_cmd"
	fieldTermChatCmd      = "term_chat_cmd"
	fieldTermComputerCmd  = "term_computer_cmd"
	fieldTermMachineCmd   = "term_machine_cmd"

	valTermDigest = "digest"
	valTermPreset = "preset"

	termModeSSH         = "ssh"
	termModeChat        = "chat"
	termModeComputer    = "computer"
	termModeMachineExec = "machines_exec"

	providerGemini         = "gemini_tasks"
	geminiSparkSettingsURL = "https://gemini.google.com/gemini-spark"

	termMaxCommandLen = 1024
	termMaxOutputSize = 16 * 1024
	termProbeTimeout  = 15 * time.Second
	termExecTimeout   = 20 * time.Second
	termComputerWait  = 115 * time.Second
	termStageTTL      = 5 * time.Minute
	termMaxHistory    = 5

	// Fixed read-only status probe: OS/kernel version and uptime/load average only.
	termProbeCommand = "uname -srm && uptime"

	geminiPresetSystem   = "uname -srm && nproc && free -h && df -h"
	geminiPresetListDir  = "ls -la"
	geminiPresetRuntimes = "python3 --version && node -v"
)

// termRunOutput is the captured outcome of one remote SSH/Tailscale command execution.
type termRunOutput struct {
	Output     string
	ExitCode   int
	DurationMs int64
	Truncated  bool
}

// termRunner executes a single command against a validated SSHTarget. Tests replace this so
// terminal logic is thoroughly tested without spawning real ssh processes.
type termRunner interface {
	Run(ctx context.Context, cfg core.SSHTarget, command string) (termRunOutput, error)
}

type execTermRunner struct{}

func (execTermRunner) Run(ctx context.Context, cfg core.SSHTarget, command string) (termRunOutput, error) {
	dest, port, err := core.SplitSSHTarget(cfg.Target)
	if err != nil {
		return termRunOutput{}, err
	}
	transport := core.NormalizeTransport(cfg.Transport)
	var bin string
	var args []string
	if transport == core.TransportTailscale {
		if port != "" {
			return termRunOutput{}, errors.New("tailscale ssh does not support custom :port")
		}
		bin = "tailscale"
		args = []string{"ssh", "--", dest, command}
	} else {
		bin = "ssh"
		args = []string{
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=10",
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "NumberOfPasswordPrompts=0",
		}
		if port != "" {
			args = append(args, "-p", port)
		}
		args = append(args, "--", dest, command)
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	buf := &limitedBuffer{max: termMaxOutputSize}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err = cmd.Run()
	dur := time.Since(start).Milliseconds()
	out := termRunOutput{
		Output:     cleanTerminalOutput(buf.String()),
		DurationMs: dur,
		Truncated:  buf.truncated,
	}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			out.ExitCode = ee.ExitCode()
			return out, nil
		}
		return out, err
	}
	return out, nil
}

type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.buf.Len() >= b.max {
		b.truncated = true
		return n, nil
	}
	avail := b.max - b.buf.Len()
	if len(p) > avail {
		b.buf.Write(p[:avail])
		b.truncated = true
		return n, nil
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// cleanTerminalOutput strips ANSI escape sequences and non-printable control characters (keeping \n and \t).
func cleanTerminalOutput(s string) string {
	s = ansiEscapeRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// capTerminalOutput cleans ANSI sequences and truncates output at maxBytes on a valid UTF-8 rune boundary.
func capTerminalOutput(s string, maxBytes int) (string, bool) {
	cleaned := cleanTerminalOutput(s)
	if len(cleaned) <= maxBytes {
		return cleaned, false
	}
	cut := cleaned[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut), true
}

type stagedCommand struct {
	Mode      string // termModeSSH | termModeChat | termModeComputer
	Target    string
	Transport string
	Command   string
	Digest    string
	StagedAt  time.Time
}

type termProbeRecord struct {
	Target     string
	Transport  string
	CheckedAt  int64
	OK         bool
	ExitCode   int
	DurationMs int64
	Output     string
	ErrMsg     string
}

type termRunRecord struct {
	Target      string
	Transport   string
	Command     string
	RanAt       int64
	ExitCode    int
	DurationMs  int64
	Output      string
	Truncated   bool
	ErrMsg      string
	Unconfirmed bool
	SessionURL  string
}

type termComputerState struct {
	Box       provider.BoxState
	CheckedAt int64
	ErrMsg    string
}

// termState holds per-account in-memory terminal confirmation state, read-only probes, and recent runs.
type termState struct {
	mu       sync.Mutex
	staged   map[string]stagedCommand
	probes   map[string]termProbeRecord
	computer map[string]termComputerState
	history  map[string][]termRunRecord
}

func newTermState() *termState {
	return &termState{
		staged:   map[string]stagedCommand{},
		probes:   map[string]termProbeRecord{},
		computer: map[string]termComputerState{},
		history:  map[string][]termRunRecord{},
	}
}

func (ts *termState) getStaged(acctID string, now time.Time) (stagedCommand, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	sc, ok := ts.staged[acctID]
	if !ok {
		return stagedCommand{}, false
	}
	if !sc.StagedAt.IsZero() && now.Sub(sc.StagedAt) > termStageTTL {
		delete(ts.staged, acctID)
		return stagedCommand{}, false
	}
	return sc, true
}

func (ts *termState) putStaged(acctID string, sc stagedCommand) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.staged[acctID] = sc
}

func (ts *termState) popStaged(acctID, mode, digest string, now time.Time) (stagedCommand, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	sc, ok := ts.staged[acctID]
	if !ok {
		return stagedCommand{}, false
	}
	if !sc.StagedAt.IsZero() && now.Sub(sc.StagedAt) > termStageTTL {
		delete(ts.staged, acctID)
		return stagedCommand{}, false
	}
	if sc.Mode != mode || sc.Digest == "" || sc.Digest != digest {
		return stagedCommand{}, false
	}
	delete(ts.staged, acctID)
	return sc, true
}

func (ts *termState) clearStaged(acctID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.staged, acctID)
}

func (ts *termState) deleteAccount(acctID string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.staged, acctID)
	delete(ts.probes, acctID)
	delete(ts.computer, acctID)
	delete(ts.history, acctID)
}

func (ts *termState) setComputer(acctID string, rec termComputerState) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.computer[acctID] = rec
}

func (ts *termState) getComputer(acctID string) (termComputerState, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	rec, ok := ts.computer[acctID]
	return rec, ok
}

func (ts *termState) setProbe(acctID string, rec termProbeRecord) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.probes[acctID] = rec
}

func (ts *termState) getProbe(acctID string) (termProbeRecord, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	rec, ok := ts.probes[acctID]
	return rec, ok
}

func (ts *termState) addHistory(acctID string, rec termRunRecord) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	list := append([]termRunRecord{rec}, ts.history[acctID]...)
	if len(list) > termMaxHistory {
		list = list[:termMaxHistory]
	}
	ts.history[acctID] = list
}

func (ts *termState) getHistory(acctID string) []termRunRecord {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	src := ts.history[acctID]
	out := make([]termRunRecord, len(src))
	copy(out, src)
	return out
}

func (a *accounts) term() *termState {
	if a.terms == nil {
		a.terms = newTermState()
	}
	return a.terms
}

func (a *accounts) runner() termRunner {
	if a.termExec != nil {
		return a.termExec
	}
	return execTermRunner{}
}

// sshCapableProvider reports whether an account's service typically backs a cloud VM or SSH-reachable environment.
func sshCapableProvider(prov string) bool {
	return prov == providerMuse || prov == grokBot || prov == echo.Name
}

// chatShellSupported reports whether the agent itself has a sandbox where it can run commands sent in chat.
func chatShellSupported(prov string) bool {
	return prov == providerMuse || prov == grokBot
}

func commandDigest(mode, target, transport, cmd string) string {
	h := sha256.Sum256([]byte(mode + "\x00" + target + "\x00" + transport + "\x00" + cmd))
	return hex.EncodeToString(h[:12])
}

func validateTerminalCommand(raw string) (string, error) {
	cmd := strings.TrimSpace(raw)
	if cmd == "" {
		return "", errors.New("empty")
	}
	if len(cmd) > termMaxCommandLen {
		return "", errors.New("too_long")
	}
	if strings.ContainsRune(cmd, 0) {
		return "", errors.New("nul")
	}
	return cmd, nil
}

// terminalEntrySection adds the Remote terminal entry row on every account detail page.
func (a *accounts) terminalEntrySection(ctx context.Context, l lang, at string, acct protocol.Account) wire.Section {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	var saved core.SSHTarget
	var hasSSH bool
	if err == nil {
		saved, hasSSH, _ = space.AccountSSHTarget(ctx, acct.AccountID)
	}
	badge := &wire.Badge{Text: l.t("只读说明", "Info"), Tone: wire.ToneNeutral}
	sub := l.t("这个服务没有可直接连接的远端终端，点这里查看说明和限制。", "This service has no direct remote shell; open to see status and limits.")
	switch {
	case hasSSH:
		badge = &wire.Badge{Text: l.t("已配 SSH", "SSH ready"), Tone: wire.TonePositive}
		sub = l.t("已配置 "+saved.Target+"：可做只读状态检查，以及经二次确认执行命令。", "Configured "+saved.Target+": read-only status check and confirmed command runner.")
	case acct.Provider == providerMuse:
		badge = &wire.Badge{Text: l.t("可配 SSH", "SSH available"), Tone: wire.TonePositive}
		sub = l.t("Muse 网页「云电脑」页只是静态快照；可在这里填 SSH 地址或经确认让它在沙箱里执行命令。", "Muse's cloud-computer page is a static snapshot; add SSH here or run confirmed commands via chat.")
	case acct.Provider == grokBot:
		badge = &wire.Badge{Text: l.t("云端沙箱", "Cloud sandbox"), Tone: wire.TonePositive}
		sub = l.t("可只读查看沙箱运行状态、配置 SSH，或经确认让它在沙箱里执行命令。", "Check read-only sandbox state, configure SSH, or run confirmed commands via chat.")
	case acct.Provider == providerGemini:
		badge = &wire.Badge{Text: l.t("云电脑", "Cloud computer"), Tone: wire.TonePositive}
		sub = l.t("可只读查看任务模式与远程计算机联网状态，或经二次确认在独立云电脑会话里执行命令。", "Check read-only task mode and remote computer internet access, or run confirmed commands in a dedicated task session.")
	case acct.Provider == echo.Name:
		badge = &wire.Badge{Text: l.t("可配 SSH", "SSH available"), Tone: wire.ToneNeutral}
		sub = l.t("可配置 SSH 地址以检查只读状态并经确认执行命令。", "Configure an SSH target for read-only checks and confirmed commands.")
	}
	return wire.Section{
		Title: l.t("远端终端与命令行", "Remote terminal & shell"),
		Items: []wire.Component{{
			Type:     "row",
			Key:      "open-terminal",
			Icon:     "code",
			Title:    l.t("打开远端终端入口", "Open remote terminal"),
			Subtitle: sub,
			Badge:    badge,
			Action: &wire.Action{
				ID: "open",
				Push: &wire.Push{
					UI:    at,
					Title: l.t("远端终端", "Remote terminal"),
					State: ext.EncodeState(pageState{Page: pageTerminal, Account: acct.AccountID}),
				},
			},
		}},
	}
}

func (a *accounts) terminalView(ctx context.Context, r *ext.UIRequest, l lang, st *pageState) *wire.View {
	acct, err := a.account(ctx, st.Account)
	if err != nil {
		return &wire.View{Title: l.t("远端终端", "Remote terminal"), Sections: []wire.Section{{Items: []wire.Component{
			{Type: "empty", Title: l.t("找不到这个账号", "Account not found"), Detail: reason(l, err, "")},
		}}}}
	}
	space, _ := a.club.Space(ctx, core.DefaultSpace)
	var saved core.SSHTarget
	var hasSSH bool
	if space != nil {
		saved, hasSSH, _ = space.AccountSSHTarget(ctx, acct.AccountID)
	}
	clk := a.clock(r)
	now := a.club.Now()
	var pairedMachine wire.MachinesStatusResult
	var hasPairedMachine bool
	if takeoverSupported(acct.Provider) && a.hub != nil {
		if mst, ok := a.machineState(ctx, takeoverMachineID(acct)); ok && mst.Exists {
			pairedMachine = mst
			hasPairedMachine = true
		}
	}
	v := &wire.View{Title: accountTitle(l, acct) + l.t(" · 远端终端", " · Terminal")}
	var sections []wire.Section

	// Section 1: Environment & read-only status (includes paired native terminal link & interactive WebShell entry).
	envItems := a.terminalEnvItems(ctx, l, space, acct, saved, hasSSH, pairedMachine, hasPairedMachine, clk)
	sections = append(sections, wire.Section{
		Title: l.t("环境与只读状态", "Environment & read-only status"),
		Items: envItems,
	})

	// Section 2: SSH connection configuration (for cloud-VM / SSH-capable accounts or if already configured).
	if sshCapableProvider(acct.Provider) || hasSSH {
		transportVal := core.TransportSSH
		if hasSSH {
			transportVal = core.NormalizeTransport(saved.Transport)
		}
		sshItems := []wire.Component{
			{
				Type:        "field",
				Name:        fieldTermSSHTarget,
				Label:       l.t("SSH 目标地址", "SSH target"),
				Kind:        wire.FieldKindText,
				Value:       saved.Target,
				Placeholder: l.t("例如 user@100.x.y.z 或 ~/.ssh/config 别名", "e.g. user@100.x.y.z or ~/.ssh/config alias"),
			},
			{
				Type:  "field",
				Name:  fieldTermSSHTransport,
				Label: l.t("连接方式", "Transport"),
				Kind:  wire.FieldKindChoice,
				Value: transportVal,
				Choices: []wire.Choice{
					{Value: core.TransportSSH, Title: "SSH"},
					{Value: core.TransportTailscale, Title: "Tailscale"},
				},
			},
			{
				Type:   "button",
				Title:  l.t("保存 SSH 目标", "Save SSH target"),
				Action: &wire.Action{ID: actTermSaveSSH, Submit: true},
			},
		}
		if hasSSH {
			sshItems = append(sshItems, wire.Component{
				Type:   "button",
				Title:  l.t("清除 SSH 目标", "Clear SSH target"),
				Role:   "destructive",
				Action: &wire.Action{ID: actTermClearSSH, Confirm: l.t("确定清除已保存的 SSH 目标地址吗？", "Clear the saved SSH target?")},
			})
		}
		sections = append(sections, wire.Section{
			Title: l.t("SSH 连接设置（不保存密码或私钥）", "SSH target (no passwords or keys stored)"),
			Footer: l.t("仅保存目标地址（如 user@host 或本机 ~/.ssh/config 里的别名），连接时复用你电脑现有的免密密钥或 Tailscale 认证；绝不接收或保存密码与私钥。",
				"Only stores the target host (user@host or ~/.ssh/config alias) and uses your existing local SSH keys or Tailscale auth; passwords and private keys are never stored."),
			Items: sshItems,
		})
	}

	// Section 3: Owner-confirmed command runner (SSH path when configured).
	staged, hasStaged := a.term().getStaged(acct.AccountID, now)
	if hasSSH {
		sections = append(sections, a.terminalSSHRunnerSection(l, acct, saved, staged, hasStaged, clk))
	}

	// Section 3b: Owner-approved machines.exec command runner for paired, confirmed Persona machines (no SSH needed).
	if hasPairedMachine && !pairedMachine.Pending {
		sections = append(sections, a.terminalMachineExecSection(l, acct, pairedMachine, staged, hasStaged))
	}

	// Section 4: Chat-assisted sandbox runner for Muse / Grok Bot (works even without SSH).
	if chatShellSupported(acct.Provider) {
		sections = append(sections, a.terminalChatRunnerSection(l, acct, staged, hasStaged, now))
	}

	// Section 4b: Gemini remote computer WebShell runner (dedicated task-mode conversation, isolated from main chat).
	if acct.Provider == providerGemini {
		sections = append(sections, a.terminalComputerRunnerSection(l, acct, staged, hasStaged, now))
	}

	// Section 5: Recent SSH / machines.exec / cloud computer outputs (if any).
	if hist := a.term().getHistory(acct.AccountID); len(hist) > 0 {
		var hItems []wire.Component
		for idx, rec := range hist {
			badge := &wire.Badge{Text: fmt.Sprintf(l.t("退出码 %d", "exit %d"), rec.ExitCode), Tone: wire.TonePositive}
			if rec.Unconfirmed {
				badge = &wire.Badge{Text: l.t("待确认", "Unconfirmed"), Tone: wire.ToneCaution}
			} else if rec.ErrMsg != "" || rec.ExitCode != 0 {
				badge.Tone = wire.ToneDanger
			}
			sub := fmt.Sprintf("%s · %dms · %s", rec.Target, rec.DurationMs, clk.at(l, rec.RanAt))
			hItems = append(hItems, wire.Component{
				Type:     "row",
				Key:      fmt.Sprintf("term-hist-%d", idx),
				Title:    oneLine(rec.Command, 72),
				Subtitle: sub,
				Badge:    badge,
			})
			outText := rec.Output
			if rec.ErrMsg != "" {
				if outText != "" {
					outText += "\n"
				}
				outText += rec.ErrMsg
			}
			if outText == "" {
				outText = l.t("（无标准输出）", "(no output)")
			}
			if rec.Truncated {
				outText += l.t("\n…（输出超过 16 KB，已截断）", "\n…(output truncated at 16 KB)")
			}
			hItems = append(hItems, wire.Component{Type: "code", Language: "text", Text: outText})
		}
		sections = append(sections, wire.Section{
			Title: l.t("最近执行记录", "Recent runs"),
			Items: hItems,
		})
	}

	// Section 6: Capabilities & limits (always visible so boundaries are transparent).
	sections = append(sections, wire.Section{
		Title: l.t("终端能力与边界说明", "Terminal capabilities & limits"),
		Footer: l.t("相关接口与协议对应 nwverse/persona#16741；托管网页服务（如 Claude Duet、ChatGPT 网页版、Manus 与推理服务）本身不提供远端命令行。",
			"Backed by nwverse/persona#16741; hosted web or inference services (Claude Duet, ChatGPT web, Manus, inference APIs) do not expose a direct shell."),
		Items: []wire.Component{
			{
				Type:     "row",
				Title:    l.t("能力 1 · 已配对机器直达原生终端", "Capability 1 · Direct native terminal link"),
				Subtitle: l.t("已配对并确认的 Persona 机器可经二次确认直达原生终端，零终端流或凭据经由本扩展。", "Confirmed paired Persona machines open their native terminal after confirmation, with zero traffic or credentials passing through this extension."),
			},
			{
				Type:     "row",
				Title:    l.t("能力 2 · 无 SSH 确认制单条命令", "Capability 2 · Owner-approved machines.exec"),
				Subtitle: l.t("通过可选权限发起命令，经 Persona 主人审批卡同意后在配对电脑执行并回显输出，拒绝即拦截。", "Runs single commands on paired computers via optional machines.exec permission, gated by a Persona owner approval card."),
			},
			{
				Type:     "row",
				Title:    l.t("能力 3 · 全功能交互式网页终端", "Capability 3 · Interactive PTY WebShell"),
				Subtitle: l.t("已配置 SSH / Tailscale 主机或本地演练账号可打开真实交互式终端，支持 vim、tmux、htop、窗口缩放与断开清理。", "Configured SSH / Tailscale targets and local test accounts open real interactive PTY sessions with vim, tmux, htop, resize, and clean teardown."),
			},
		},
	})

	v.Sections = sections
	return v
}

func (a *accounts) terminalEnvItems(ctx context.Context, l lang, space *core.Space, acct protocol.Account, saved core.SSHTarget, hasSSH bool, pairedMachine wire.MachinesStatusResult, hasPairedMachine bool, clk clock) []wire.Component {
	var items []wire.Component
	switch acct.Provider {
	case providerMuse:
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("环境类型", "Environment"),
			Value:    l.t("Muse 云端电脑", "Muse cloud computer"),
			Subtitle: l.t("muse.ai 侧栏「云电脑」页只是静态配置快照，不能在上面敲命令。两条真实路径：在下方填入 SSH / Tailscale 地址直接连接，或经确认让 Muse 在沙箱里代为执行。", "Muse's cloud-computer page is a static snapshot, not an interactive shell. Two real paths: configure SSH / Tailscale below, or confirm a chat command for Muse to run in its sandbox."),
			Badge:    &wire.Badge{Text: l.t("支持 SSH", "SSH capable"), Tone: wire.TonePositive},
		})
	case grokBot:
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("环境类型", "Environment"),
			Value:    l.t("Grok Bot 云端沙箱", "Grok Bot cloud sandbox"),
			Subtitle: l.t("Grok Bot 桌面端的图形终端通过每个沙箱独立的动态令牌隧道连接（独立终端与图形端口），扩展沙箱外网白名单与声明式页面暂不能直接嵌入该隧道；这里提供只读沙箱状态、SSH 入口与对话委托执行。", "Grok Bot's desktop terminal uses per-sandbox dynamic tunnel tokens and live PTY/noVNC ports that extension pages cannot embed yet; read-only sandbox state, SSH, and confirmed chat execution are available here."),
			Badge:    &wire.Badge{Text: l.t("云端沙箱", "Cloud sandbox"), Tone: wire.ToneNeutral},
		})
		if space != nil {
			if box, supported, err := space.AccountBoxState(ctx, acct.AccountID); supported {
				val, tone := boxStateLabel(l, box.State)
				sub := l.t("只读查询，不会唤醒休眠中的沙箱。", "Read-only check; does not wake a sleeping sandbox.")
				if box.Image != "" {
					sub = l.t("镜像：", "Image: ") + box.Image + " · " + sub
				}
				if err != nil {
					val = l.t("暂时读取不到", "Unavailable right now")
					tone = wire.ToneNeutral
				}
				items = append(items, wire.Component{
					Type:     "row",
					Title:    l.t("云端沙箱状态（只读）", "Sandbox state (read-only)"),
					Value:    val,
					Subtitle: sub,
					Badge:    &wire.Badge{Text: val, Tone: tone},
				})
			}
		}
	case providerGemini:
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("环境类型", "Environment"),
			Value:    l.t("Gemini 云电脑（任务模式）", "Gemini cloud computer (task mode)"),
			Subtitle: l.t("配备 2 核 CPU / 8 GB 内存 / Debian 12，工作目录中的代码与文件会在同一台远程计算机上跨任务持久保存；命令通过独立的任务模式会话执行，不会混入你的主对话。", "2 vCPU / 8 GB RAM / Debian 12 with a persistent working directory shared across tasks; commands run in a dedicated task-mode conversation without touching your main chat."),
			Badge:    &wire.Badge{Text: l.t("云电脑", "Cloud computer"), Tone: wire.TonePositive},
		})
		var box provider.BoxState
		var boxSupported bool
		var boxErr error
		cached, hasCached := a.term().getComputer(acct.AccountID)
		if space != nil {
			bctx, cancel := context.WithTimeout(ctx, termProbeTimeout)
			box, boxSupported, boxErr = space.AccountBoxState(bctx, acct.AccountID)
			cancel()
		}
		if hasCached {
			if !boxSupported {
				box = cached.Box
				boxSupported = true
			} else if boxErr != nil || acct.Status == protocol.AccountPaused {
				if box.InternetAccess == nil {
					box.InternetAccess = cached.Box.InternetAccess
				}
				if box.SessionURL == "" {
					box.SessionURL = cached.Box.SessionURL
					box.SessionTitle = cached.Box.SessionTitle
				}
			}
		}
		if boxSupported {
			stateVal, stateTone := boxStateLabel(l, box.State)
			taskCounts := fmt.Sprintf(
				l.t("任务进行中 %d / 待处理 %d / 已就绪 %d · 只读查询，不唤醒无关会话。",
					"%d running / %d waiting / %d ready · read-only check."),
				box.Running, box.Waiting, box.Ready,
			)
			stateSub := taskCounts
			if box.TaskMode != nil && *box.TaskMode {
				stateSub = l.t("使用任务模式委托工作：已开启 · ", "Task mode enabled · ") + taskCounts
			}
			if box.Image != "" {
				stateSub = l.t("规格：", "Spec: ") + box.Image + " · " + stateSub
			}
			if acct.Status == protocol.AccountPaused {
				stateVal = l.t("已暂停同步", "Paused")
				stateTone = wire.ToneNeutral
				stateSub = l.t("这个账号已暂停同步，未发起实时检查；恢复同步后再刷新状态。", "This account is paused, so no live check was run; resume syncing first.")
			} else if boxErr != nil {
				stateVal = l.t("暂时读取不到", "Unavailable right now")
				stateTone = wire.ToneCaution
				stateSub = a.terminalProblemShort(ctx, l, space, acct, boxErr)
			} else if box.TaskMode != nil && !*box.TaskMode {
				stateVal = l.t("未开启任务模式", "Task mode off")
				stateTone = wire.ToneCaution
				stateSub = l.t("请先前往下方 Gemini 任务设置页开启「使用任务模式委托工作」；这里只做只读检查，不会替你改动任何开关。",
					"Turn on task mode on Gemini's task settings page below first; this page is read-only and never changes your settings.")
			}
			items = append(items, wire.Component{
				Type:     "row",
				Title:    l.t("任务模式与云电脑状态（只读）", "Task mode & computer state (read-only)"),
				Value:    stateVal,
				Subtitle: stateSub,
				Badge:    &wire.Badge{Text: stateVal, Tone: stateTone},
			})

			netVal := l.t("暂时读取不到", "Unavailable right now")
			netTone := wire.ToneNeutral
			netSub := l.t("点下方「只读刷新云电脑状态」重新检查，或打开 Gemini 任务设置页查看。",
				"Tap Refresh below to check again, or open Gemini's task settings page.")
			if box.InternetAccess != nil {
				if *box.InternetAccess {
					netVal = l.t("已开启", "Enabled")
					netTone = wire.TonePositive
					netSub = l.t("已允许远程计算机在任务中访问外网（可安装软件包、拉取代码或调用外部服务）。",
						"The remote computer is allowed outbound internet access across tasks (install packages, fetch repos, or call external services).")
				} else {
					netVal = l.t("未开启", "Disabled")
					netTone = wire.ToneCaution
					netSub = l.t("当前未开启远程计算机的网络访问权限，需要联网的命令会受限；如需开启，请点下方链接前往 Gemini 任务设置页自行打开开关（这里不会擅自修改你的开关）。",
						"Remote computer internet access is currently off, so commands requiring network access will fail; open Gemini's task settings below to turn it on yourself (this page never flips your switch).")
				}
			}
			items = append(items, wire.Component{
				Type:     "row",
				Title:    l.t("远程计算机的网络访问权限（只读）", "Remote computer internet access (read-only)"),
				Value:    netVal,
				Subtitle: netSub,
				Badge:    &wire.Badge{Text: netVal, Tone: netTone},
			})
		}
		if hasCached && cached.CheckedAt > 0 {
			items = append(items, wire.Component{
				Type:     "row",
				Title:    l.t("最近一次只读检查", "Last read-only check"),
				Value:    clk.at(l, cached.CheckedAt),
				Subtitle: l.t("只读读取任务设置与任务列表，未改动任何开关。", "Read-only check of task settings and task list; no switches were changed."),
				Badge:    &wire.Badge{Text: l.t("只读", "Read-only"), Tone: wire.TonePositive},
			})
		}
		items = append(items, wire.Component{
			Type:   "button",
			Title:  l.t("只读刷新云电脑状态", "Refresh computer status (read-only)"),
			Action: &wire.Action{ID: actTermComputerRefresh},
		})
		settingsURL := geminiSparkSettingsURL
		if httpsURL(box.SettingsURL) {
			settingsURL = box.SettingsURL
		}
		items = append(items, wire.Component{
			Type:     "link",
			Icon:     "settings",
			Title:    l.t("打开 Gemini 任务设置页", "Open Gemini task settings"),
			Subtitle: l.t("直达 gemini.google.com/gemini-spark 查看或自行管理「使用任务模式委托工作」与「远程计算机的网络访问权限」。", "Open gemini.google.com/gemini-spark to view or manage task mode and remote computer internet access."),
			URL:      settingsURL,
		})
		sessionURL := box.SessionURL
		if sessionURL == "" {
			for _, rec := range a.term().getHistory(acct.AccountID) {
				if rec.SessionURL != "" {
					sessionURL = rec.SessionURL
					break
				}
			}
		}
		if httpsURL(sessionURL) {
			sessionSub := l.t("在浏览器中直达当前云电脑关联的独立任务模式会话。", "Open the dedicated task-mode conversation backing this cloud computer in your browser.")
			if box.SessionTitle != "" {
				sessionSub = l.t("当前会话：", "Current session: ") + box.SessionTitle
			}
			items = append(items, wire.Component{
				Type:     "link",
				Icon:     "link",
				Title:    l.t("打开当前云电脑任务会话", "Open current cloud computer session"),
				Subtitle: sessionSub,
				URL:      sessionURL,
			})
		}
		if space != nil && basicPlace(acct).Browser {
			if mlabel := a.browserMachineLabel(ctx, space, acct); mlabel != "" {
				items = append(items, wire.Component{
					Type:     "row",
					Title:    l.t("所在浏览器电脑", "Browser computer"),
					Value:    mlabel,
					Subtitle: l.t("此账号通过这台电脑上的浏览器连接 Gemini 云电脑。", "This account reaches Gemini's cloud computer through the browser on that computer."),
				})
			}
		}
	case echo.Name:
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("环境类型", "Environment"),
			Value:    l.t("本地演练账号", "Local test account"),
			Subtitle: l.t("自带受限只读本地演练交互终端，也可绑定受信任的 SSH 目标测试只读状态检查与二次确认命令执行。", "Includes a restricted local sandbox interactive terminal and can also bind a trusted SSH target."),
			Badge:    &wire.Badge{Text: l.t("可交互终端", "Interactive PTY"), Tone: wire.TonePositive},
		})
		if extURL, ok := extTerminalOpenURL(terminalTargetPrefixEcho + acct.AccountID); ok && acct.Status != protocol.AccountPaused {
			items = append(items, wire.Component{
				Type:  "button",
				Key:   "open-echo-interactive-terminal",
				Title: l.t("打开本地演练交互终端", "Open interactive sandbox terminal"),
				Action: &wire.Action{
					ID:   "open_echo_interactive_terminal",
					Open: extURL,
					Confirm: l.t(
						"确认打开本地演练交互终端吗？它运行在隔离的受限只读沙箱中，可用于验证真实终端输入输出、窗口缩放与断开清理，无需外部账号凭据。",
						"Open the local sandbox interactive terminal? It runs in a restricted read-only sandbox for testing live PTY I/O, resize, and clean teardown without external credentials.",
					),
				},
			})
		}
	default:
		kind := l.t("托管网页服务（无直接终端）", "Hosted web service (no direct shell)")
		detail := l.t("这个服务不对外开放 SSH 或交互式命令行入口，因此无法在 Persona 里直接打开终端。", "This service does not expose SSH or an interactive shell, so a direct terminal cannot be opened.")
		switch acct.Provider {
		case "claude_duet":
			detail = l.t("Claude Duet 运行在 Anthropic 托管云端，不对外提供 SSH 或沙箱终端入口。", "Claude Duet runs on Anthropic's hosted service without an SSH or sandbox shell.")
		case "dots":
			detail = l.t("ChatGPT 网页版不对外提供 SSH 或持续终端会话入口。", "ChatGPT web does not expose SSH or a persistent shell.")
		case cue.Name:
			kind = l.t("云端任务服务（无直接终端）", "Hosted task service (no direct shell)")
			detail = l.t("Manus 任务在对方托管沙箱内运行，目前未向外部开放 SSH 连接地址。", "Manus tasks run in a managed sandbox with no external SSH address.")
		case groq.Name, muse2api.Name:
			kind = l.t("推理服务（无远端主机）", "Inference service (no remote host)")
			detail = l.t("这类账号只提供对话推理，背后没有可供你登录的独立电脑环境。", "This account only serves chat inference and has no remote computer to log into.")
		}
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("环境类型", "Environment"),
			Value:    kind,
			Subtitle: detail,
			Badge:    &wire.Badge{Text: l.t("不支持终端", "No shell"), Tone: wire.ToneNeutral},
		})
		if space != nil && basicPlace(acct).Browser {
			if mlabel := a.browserMachineLabel(ctx, space, acct); mlabel != "" {
				items = append(items, wire.Component{
					Type:     "row",
					Title:    l.t("所在浏览器电脑", "Browser computer"),
					Value:    mlabel,
					Subtitle: l.t("此账号通过这台电脑上的浏览器同步；如要操作这台电脑本身，请在 Persona 的「机器」页打开它的终端。", "This account syncs through the browser on that computer; open its terminal from Persona's Machines page if needed."),
				})
			}
		}
	}

	if hasPairedMachine {
		st := pairedMachine
		badgeText, badgeTone := l.t("离线", "Offline"), wire.ToneDanger
		rowSub := l.t("请先在「机器」页确认这台电脑归属，确认后即可直达原生终端。", "Confirm this computer on the Machines page first to enable the native terminal link.")
		if st.Pending {
			badgeText, badgeTone = l.t("待确认", "Pending"), wire.ToneCaution
		} else {
			if st.Online {
				badgeText, badgeTone = l.t("在线", "Online"), wire.TonePositive
			}
			rowSub = l.t("点击下方按钮经主人确认后直达 Persona 原生交互终端，零终端流或凭据经由本扩展。", "Tap below to open Persona's native interactive terminal after confirmation; zero terminal traffic or credentials pass through this extension.")
		}
		machineLabel := st.Name
		if strings.TrimSpace(machineLabel) == "" {
			machineLabel = st.MachineID
		}
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("已接入的 Persona 机器", "Paired Persona machine"),
			Value:    st.MachineID,
			Subtitle: rowSub,
			Badge:    &wire.Badge{Text: badgeText, Tone: badgeTone},
		})
		if !st.Pending {
			if openURL, ok := nativeTerminalOpenURL(st.MachineID); ok {
				offlineNote := ""
				if !st.Online {
					offlineNote = l.t("（当前显示离线，打开后可查看连接状态。）", " (Currently offline; opening will show connection status.) ")
				}
				items = append(items, wire.Component{
					Type:  "button",
					Key:   "open-native-terminal",
					Title: l.t("打开原生终端", "Open native terminal"),
					Action: &wire.Action{
						ID:   "open_native_terminal",
						Open: openURL,
						Confirm: l.t(
							"确认直接打开电脑「"+machineLabel+"」（"+st.MachineID+"）的 Persona 原生终端吗？"+offlineNote+"终端会话直连到 Persona 机器，本扩展不会接触终端内容或任何凭据。",
							"Open Persona's native terminal for \""+machineLabel+"\" ("+st.MachineID+")?"+offlineNote+" The session connects directly to the machine; this extension never sees terminal traffic or credentials.",
						),
					},
				})
			}
		}
	}

	if hasSSH {
		items = append(items, wire.Component{
			Type:     "row",
			Title:    l.t("已保存的 SSH 目标", "Saved SSH target"),
			Value:    saved.Target,
			Subtitle: l.t("连接方式：", "Transport: ") + transportLabel(saved.Transport),
			Badge:    &wire.Badge{Text: transportLabel(saved.Transport), Tone: wire.TonePositive},
		})
		if extURL, ok := extTerminalOpenURL(terminalTargetPrefixSSH + acct.AccountID); ok && acct.Status != protocol.AccountPaused {
			items = append(items, wire.Component{
				Type:  "button",
				Key:   "open-ssh-interactive-terminal",
				Title: l.t("打开交互式网页终端", "Open interactive WebShell"),
				Action: &wire.Action{
					ID:   "open_ssh_interactive_terminal",
					Open: extURL,
					Confirm: l.t(
						"确认打开指向 "+saved.Target+"（"+transportLabel(saved.Transport)+"）的交互式网页终端吗？支持 vim、tmux、htop、窗口缩放与 Ctrl-C，密码和私钥绝不落盘。",
						"Open an interactive WebShell to "+saved.Target+" ("+transportLabel(saved.Transport)+")? Supports vim, tmux, htop, window resize, and Ctrl-C; passwords and keys are never stored.",
					),
				},
			})
		}
		items = append(items, wire.Component{
			Type:   "button",
			Title:  l.t("检查远端状态（只读）", "Check remote status (read-only)"),
			Action: &wire.Action{ID: actTermProbe},
		})
		if probe, ok := a.term().getProbe(acct.AccountID); ok && probe.Target == saved.Target {
			badge := &wire.Badge{Text: l.t("连通", "Reachable"), Tone: wire.TonePositive}
			if !probe.OK {
				badge = &wire.Badge{Text: l.t("连接失败", "Unreachable"), Tone: wire.ToneDanger}
			}
			items = append(items, wire.Component{
				Type:     "row",
				Title:    l.t("最近一次只读检查", "Last read-only check"),
				Value:    fmt.Sprintf("%dms", probe.DurationMs),
				Subtitle: l.t("检查时间：", "Checked ") + clk.at(l, probe.CheckedAt),
				Badge:    badge,
			})
			txt := probe.Output
			if probe.ErrMsg != "" {
				if txt != "" {
					txt += "\n"
				}
				txt += probe.ErrMsg
			}
			if txt != "" {
				items = append(items, wire.Component{Type: "code", Language: "text", Text: txt})
			}
		}
	}
	return items
}

func (a *accounts) terminalSSHRunnerSection(l lang, acct protocol.Account, saved core.SSHTarget, staged stagedCommand, hasStaged bool, _ clock) wire.Section {
	var items []wire.Component
	if hasStaged && staged.Mode == termModeSSH {
		items = append(items,
			wire.Component{
				Type:     "row",
				Key:      "term-staged-banner",
				Icon:     "warning",
				Title:    l.t("待你确认执行的命令", "Command waiting for your confirmation"),
				Subtitle: l.t("目标："+staged.Target+"（"+transportLabel(staged.Transport)+"）。请核对下方命令，点「确认执行」后才会真正发往远端。", "Target: "+staged.Target+" ("+transportLabel(staged.Transport)+"). Review the exact command below before confirming."),
				Badge:    &wire.Badge{Text: l.t("待确认", "Confirm"), Tone: wire.ToneCaution},
			},
			wire.Component{Type: "code", Language: "sh", Text: staged.Command},
			wire.Component{
				Type:  "button",
				Title: l.t("确认执行", "Confirm and run"),
				Action: &wire.Action{
					ID:     actTermExec,
					Values: map[string]string{valTermDigest: staged.Digest},
					Confirm: l.t("确认在 "+staged.Target+" 上执行这条命令？\n\n"+staged.Command,
						"Run this command on "+staged.Target+"?\n\n"+staged.Command),
				},
			},
			wire.Component{
				Type:   "button",
				Title:  l.t("取消待执行命令", "Cancel staged command"),
				Action: &wire.Action{ID: actTermCancel},
			},
		)
	}
	items = append(items,
		wire.Component{
			Type:        "field",
			Name:        fieldTermCmd,
			Label:       l.t("要执行的命令", "Command to run"),
			Kind:        wire.FieldKindText,
			Placeholder: l.t("例如 uptime、df -h 或 ls -la", "e.g. uptime, df -h, ls -la"),
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("准备执行（下一步确认）", "Stage command (confirm next)"),
			Action: &wire.Action{ID: actTermStage, Submit: true},
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("快速预填：查看负载与磁盘（只读）", "Quick stage: load & disk (read-only)"),
			Action: &wire.Action{ID: actTermPreset, Values: map[string]string{valTermPreset: "uptime && df -h"}},
		},
	)
	return wire.Section{
		Title: l.t("经确认执行命令（SSH）", "Confirmed command execution (SSH)"),
		Footer: l.t("所有命令必须先「准备执行」核对内容，再经「确认执行」二次确认才会发往 "+saved.Target+"；单次超时 20 秒，输出上限 16 KB。",
			"Every command must be staged and explicitly confirmed before running on "+saved.Target+"; 20 s timeout, 16 KB output cap."),
		Items: items,
	}
}

func (a *accounts) terminalMachineExecSection(l lang, acct protocol.Account, st wire.MachinesStatusResult, staged stagedCommand, hasStaged bool) wire.Section {
	var items []wire.Component
	machineLabel := st.Name
	if strings.TrimSpace(machineLabel) == "" {
		machineLabel = st.MachineID
	}
	if a.machinesExecDenied() {
		items = append(items, wire.Component{
			Type:     "row",
			Key:      "term-machine-exec-scope-note",
			Icon:     "warning",
			Title:    l.t("尚未开启「在配对电脑执行命令」权限", "Machine command permission not enabled"),
			Subtitle: l.t("这是可选权限：如要在无 SSH 时经审批执行命令，请在 Persona 扩展权限设置里开启后再使用。", "This permission is optional: enable machines.exec in Persona's extension permissions to run owner-approved commands without SSH."),
			Badge:    &wire.Badge{Text: l.t("可选权限", "Optional"), Tone: wire.ToneNeutral},
		})
	}
	if !st.Online {
		items = append(items, wire.Component{
			Type:     "row",
			Key:      "term-machine-exec-offline-note",
			Icon:     "warning",
			Title:    l.t("配对电脑当前离线", "Paired computer is offline"),
			Subtitle: l.t("机器重新上线后，经审批的单条命令即可直接在这台电脑上执行。", "Once the computer comes back online, owner-approved commands will run directly on it."),
			Badge:    &wire.Badge{Text: l.t("离线", "Offline"), Tone: wire.ToneCaution},
		})
	}
	if hasStaged && staged.Mode == termModeMachineExec {
		items = append(items,
			wire.Component{
				Type:     "row",
				Key:      "term-machine-staged-banner",
				Icon:     "warning",
				Title:    l.t("待提交主人审批执行的命令", "Command waiting to request owner approval"),
				Subtitle: l.t("目标电脑："+machineLabel+"（"+st.MachineID+"）。点下方按钮后，Persona 会弹出主人审批卡，同意后才真正执行。", "Target machine: "+machineLabel+" ("+st.MachineID+"). Tapping below triggers Persona's owner approval card before running."),
				Badge:    &wire.Badge{Text: l.t("待审批", "Approve"), Tone: wire.ToneCaution},
			},
			wire.Component{Type: "code", Language: "sh", Text: staged.Command},
			wire.Component{
				Type:  "button",
				Title: l.t("提交主人审批并执行", "Request owner approval and run"),
				Action: &wire.Action{
					ID:     actTermMachineExec,
					Values: map[string]string{valTermDigest: staged.Digest},
					Confirm: l.t("确认向 Persona 发起审批，在电脑「"+machineLabel+"」上执行这条命令？\n\n"+staged.Command,
						"Request owner approval from Persona to run this command on \""+machineLabel+"\"?\n\n"+staged.Command),
				},
			},
			wire.Component{
				Type:   "button",
				Title:  l.t("取消待执行命令", "Cancel staged command"),
				Action: &wire.Action{ID: actTermCancel},
			},
		)
	}
	items = append(items,
		wire.Component{
			Type:        "field",
			Name:        fieldTermMachineCmd,
			Label:       l.t("要在配对电脑执行的命令", "Command for paired machine"),
			Kind:        wire.FieldKindText,
			Placeholder: l.t("例如 uname -a、uptime 或 df -h", "e.g. uname -a, uptime, or df -h"),
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("准备执行（下一步提交审批）", "Stage command (approve next)"),
			Action: &wire.Action{ID: actTermMachineStage, Submit: true},
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("快速预填：查看系统与磁盘（只读）", "Quick stage: system & disk (read-only)"),
			Action: &wire.Action{ID: actTermMachinePreset, Values: map[string]string{valTermPreset: "uname -srm && df -h"}},
		},
	)
	return wire.Section{
		Title: l.t("经审批在配对电脑执行命令（无需 SSH）", "Owner-approved machine command (machines.exec)"),
		Footer: l.t("适用于沙箱无 SSH 场景：命令先在此核对，再由 Persona 主人审批卡逐次放行后在 "+st.MachineID+" 上执行并回显输出；单次超时 20 秒，输出上限 16 KB。",
			"For sandboxed environments without SSH: every command is staged here and then requires explicit Persona owner approval before running on "+st.MachineID+"; 20 s timeout, 16 KB output cap."),
		Items: items,
	}
}

func (a *accounts) terminalChatRunnerSection(l lang, acct protocol.Account, staged stagedCommand, hasStaged bool, now time.Time) wire.Section {
	var items []wire.Component
	exhaustedNote := ""
	if blocked, _ := quotaExhaustedActive(acct.Quota, now.UnixMilli()); blocked {
		exhaustedNote = l.t("注意：这个账号当前额度已用完，发送前会被提示拦截。", "Note: this account is currently out of allowance and sending will be blocked. ")
		items = append(items, wire.Component{
			Type:     "row",
			Icon:     "warning",
			Title:    l.t("当前额度已用完", "Allowance used up"),
			Subtitle: l.t("通过对话委托执行会消耗额度，当前会被拦截；请等恢复后或改用上面的 SSH 入口。", "Chat-assisted execution uses account allowance and is currently blocked; wait for reset or use SSH above."),
			Badge:    &wire.Badge{Text: l.t("额度用完", "Used up"), Tone: wire.ToneDanger},
		})
	}
	if hasStaged && staged.Mode == termModeChat {
		items = append(items,
			wire.Component{
				Type:     "row",
				Icon:     "warning",
				Title:    l.t("待确认发送给助手执行的命令", "Command ready to send to agent chat"),
				Subtitle: l.t("确认后会发到它的主对话，请它在其沙箱里执行并贴回输出。", "Confirming sends this request to its main chat so it runs the command in its sandbox and replies."),
				Badge:    &wire.Badge{Text: l.t("待确认", "Confirm"), Tone: wire.ToneCaution},
			},
			wire.Component{Type: "code", Language: "sh", Text: staged.Command},
			wire.Component{
				Type:  "button",
				Title: l.t("确认发给助手执行", "Confirm and send to chat"),
				Action: &wire.Action{
					ID:     actTermChatSend,
					Values: map[string]string{valTermDigest: staged.Digest},
					Confirm: exhaustedNote + l.t("确认把这条命令发到「"+accountTitle(l, acct)+"」的主对话里让它执行？\n\n"+staged.Command,
						"Send this command to "+accountTitle(l, acct)+"'s main chat to run in its sandbox?\n\n"+staged.Command),
				},
			},
			wire.Component{
				Type:   "button",
				Title:  l.t("取消", "Cancel"),
				Action: &wire.Action{ID: actTermCancel},
			},
		)
	}
	items = append(items,
		wire.Component{
			Type:        "field",
			Name:        fieldTermChatCmd,
			Label:       l.t("让助手在沙箱里执行的命令", "Command for agent sandbox"),
			Kind:        wire.FieldKindText,
			Placeholder: l.t("例如 uname -a && df -h", "e.g. uname -a && df -h"),
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("准备发给助手（下一步确认）", "Stage for agent chat (confirm next)"),
			Action: &wire.Action{ID: actTermChatStage, Submit: true},
		},
	)
	return wire.Section{
		Title: l.t("通过对话在沙箱执行（无需 SSH）", "Run in sandbox via chat (no SSH needed)"),
		Footer: l.t("如果没有配 SSH，也可以经确认后把命令发到这个助手的主对话，由它在自己的沙箱里执行后回复输出。",
			"Without SSH, you can still confirm and send a command to this agent's main chat to run inside its sandbox."),
		Items: items,
	}
}

func isGeminiComputerPreset(cmd string) bool {
	switch strings.TrimSpace(cmd) {
	case geminiPresetSystem, geminiPresetListDir, geminiPresetRuntimes:
		return true
	default:
		return false
	}
}

func (a *accounts) terminalComputerRunnerSection(l lang, acct protocol.Account, staged stagedCommand, hasStaged bool, now time.Time) wire.Section {
	var items []wire.Component
	exhaustedNote := ""
	if blocked, _ := quotaExhaustedActive(acct.Quota, now.UnixMilli()); blocked {
		exhaustedNote = l.t("注意：这个账号当前额度已用完，执行前会被提示拦截。", "Note: this account is currently out of allowance and execution will be blocked. ")
		items = append(items, wire.Component{
			Type:     "row",
			Icon:     "warning",
			Title:    l.t("当前额度已用完", "Allowance used up"),
			Subtitle: l.t("在 Gemini 云电脑执行命令会消耗账号额度，当前会被拦截；请等额度恢复后再试。", "Running commands on Gemini's cloud computer uses account allowance and is currently blocked; wait for reset before retrying."),
			Badge:    &wire.Badge{Text: l.t("额度用完", "Used up"), Tone: wire.ToneDanger},
		})
	}
	if hasStaged && staged.Mode == termModeComputer {
		items = append(items,
			wire.Component{
				Type:     "row",
				Key:      "term-computer-staged-banner",
				Icon:     "warning",
				Title:    l.t("待你确认在云电脑执行的命令", "Command waiting for your confirmation"),
				Subtitle: l.t("请核对下方命令，点「确认在云电脑执行」后才会发往独立的任务模式会话（不会混入主对话）。校验摘要："+staged.Digest, "Review the exact command below; confirming runs it in the dedicated task-mode session without touching your main chat. Digest: "+staged.Digest),
				Badge:    &wire.Badge{Text: l.t("待确认", "Confirm"), Tone: wire.ToneCaution},
			},
			wire.Component{Type: "code", Language: "sh", Text: staged.Command},
			wire.Component{
				Type:  "button",
				Title: l.t("确认在云电脑执行", "Confirm and run on cloud computer"),
				Action: &wire.Action{
					ID:     actTermComputerRun,
					Values: map[string]string{valTermDigest: staged.Digest},
					Confirm: exhaustedNote + l.t("确认在「"+accountTitle(l, acct)+"」的 Gemini 云电脑（独立任务模式会话）中执行这条命令？\n\n"+staged.Command,
						"Run this command on "+accountTitle(l, acct)+"'s Gemini cloud computer (dedicated task session)?\n\n"+staged.Command),
				},
			},
			wire.Component{
				Type:   "button",
				Title:  l.t("取消待执行命令", "Cancel staged command"),
				Action: &wire.Action{ID: actTermCancel},
			},
		)
	}
	items = append(items,
		wire.Component{
			Type:        "field",
			Name:        fieldTermComputerCmd,
			Label:       l.t("要在云电脑执行的命令", "Command for cloud computer"),
			Kind:        wire.FieldKindText,
			Placeholder: l.t("例如 uname -srm && df -h 或 ls -la", "e.g. uname -srm && df -h or ls -la"),
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("准备执行（下一步确认）", "Stage command (confirm next)"),
			Action: &wire.Action{ID: actTermComputerStage, Submit: true},
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("快速预填：系统内核与资源（只读）", "Quick stage: kernel & resources (read-only)"),
			Action: &wire.Action{ID: actTermComputerPreset, Values: map[string]string{valTermPreset: geminiPresetSystem}},
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("快速预填：查看持久目录文件（只读）", "Quick stage: list working directory (read-only)"),
			Action: &wire.Action{ID: actTermComputerPreset, Values: map[string]string{valTermPreset: geminiPresetListDir}},
		},
		wire.Component{
			Type:   "button",
			Title:  l.t("快速预填：查看 Python 与 Node 版本（只读）", "Quick stage: Python & Node versions (read-only)"),
			Action: &wire.Action{ID: actTermComputerPreset, Values: map[string]string{valTermPreset: geminiPresetRuntimes}},
		},
	)
	return wire.Section{
		Title: l.t("云电脑命令行（经二次确认执行）", "Cloud computer command runner (confirmed)"),
		Footer: l.t("每条命令必须先「准备执行」生成校验摘要，再经「确认在云电脑执行」二次确认后才会在独立任务模式会话中运行；输出上限 16 KB，保留最近 5 次终端回显。",
			"Every command must be staged and explicitly confirmed before running in the dedicated task-mode conversation; 16 KB output cap, last 5 runs retained."),
		Items: items,
	}
}

func (a *accounts) handleTerminalAction(ctx context.Context, r *ext.UIRequest, l lang, st pageState, done, refuse func(string) (*wire.ActionResult, error), failed func(error, string) (*wire.ActionResult, error)) (*wire.ActionResult, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return failed(err, "")
	}
	acct, err := a.account(ctx, st.Account)
	if err != nil {
		return failed(err, "")
	}
	now := a.club.Now()
	switch r.Action {
	case actTermSaveSSH:
		target := strings.TrimSpace(r.Values[fieldTermSSHTarget])
		if target == "" {
			return refuse(l.t("还没填 SSH 目标：输入 user@host 或本机别名再保存。", "Enter an SSH target (user@host or host alias) first."))
		}
		transport := core.NormalizeTransport(r.Values[fieldTermSSHTransport])
		if transport == core.TransportTailscale {
			if _, port, perr := core.SplitSSHTarget(target); perr == nil && port != "" {
				return refuse(l.t("Tailscale SSH 不使用自定义端口，请去掉末尾的 :"+port+"，或切换为普通 SSH。", "Tailscale SSH does not take a custom port; remove :"+port+" or switch transport to SSH."))
			}
		}
		if err := space.SetAccountSSHTarget(ctx, acct.AccountID, core.SSHTarget{Target: target, Transport: transport}); err != nil {
			return refuse(l.t("SSH 目标格式不对：请只填 user@host[:端口] 或主机别名，不要带空格、引号或参数。", "Invalid SSH target: enter only user@host[:port] or a host alias with no spaces, quotes, or flags."))
		}
		a.term().clearStaged(acct.AccountID)
		return done(l.t("已保存 SSH 目标："+target, "Saved SSH target: "+target))

	case actTermClearSSH:
		if err := space.SetAccountSSHTarget(ctx, acct.AccountID, core.SSHTarget{}); err != nil {
			return failed(err, "")
		}
		a.term().clearStaged(acct.AccountID)
		return done(l.t("已清除 SSH 目标", "Cleared SSH target"))

	case actTermProbe:
		saved, ok, err := space.AccountSSHTarget(ctx, acct.AccountID)
		if err != nil {
			return failed(err, "")
		}
		if !ok {
			return refuse(l.t("还没有保存 SSH 目标：先在下面填好并保存。", "No SSH target saved yet; save one below first."))
		}
		pctx, cancel := context.WithTimeout(ctx, termProbeTimeout)
		defer cancel()
		out, rerr := a.runner().Run(pctx, saved, termProbeCommand)
		rec := termProbeRecord{
			Target:     saved.Target,
			Transport:  saved.Transport,
			CheckedAt:  now.UnixMilli(),
			OK:         rerr == nil && out.ExitCode == 0,
			ExitCode:   out.ExitCode,
			DurationMs: out.DurationMs,
			Output:     out.Output,
		}
		if rerr != nil {
			rec.ErrMsg = describeTermError(l, saved.Transport, rerr)
		} else if out.ExitCode != 0 {
			rec.ErrMsg = fmt.Sprintf(l.t("远端命令返回退出码 %d（请确认免密认证与连通性）", "Remote command exited with code %d (check key auth and reachability)"), out.ExitCode)
		}
		a.term().setProbe(acct.AccountID, rec)
		if rec.OK {
			return done(l.t("只读状态检查通过", "Read-only status check succeeded"))
		}
		return refuse(rec.ErrMsg)

	case actTermStage, actTermPreset:
		saved, ok, err := space.AccountSSHTarget(ctx, acct.AccountID)
		if err != nil {
			return failed(err, "")
		}
		if !ok {
			return refuse(l.t("还没有保存 SSH 目标：先在上面填好并保存。", "No SSH target saved yet; save one above first."))
		}
		rawCmd := r.Values[fieldTermCmd]
		if r.Action == actTermPreset {
			rawCmd = r.Values[valTermPreset]
		}
		cmd, cerr := validateTerminalCommand(rawCmd)
		if cerr != nil {
			return refuse(commandValidationWords(l, cerr))
		}
		digest := commandDigest(termModeSSH, saved.Target, saved.Transport, cmd)
		a.term().putStaged(acct.AccountID, stagedCommand{
			Mode:      termModeSSH,
			Target:    saved.Target,
			Transport: saved.Transport,
			Command:   cmd,
			Digest:    digest,
			StagedAt:  now,
		})
		return done(l.t("已准备好命令，请核对后点「确认执行」", "Command staged; review it and tap Confirm and run"))

	case actTermExec:
		saved, ok, err := space.AccountSSHTarget(ctx, acct.AccountID)
		if err != nil {
			return failed(err, "")
		}
		if !ok {
			return refuse(l.t("SSH 目标已清除，未执行任何命令。", "SSH target was cleared; nothing was run."))
		}
		sc, ok := a.term().popStaged(acct.AccountID, termModeSSH, r.Values[valTermDigest], now)
		if !ok || sc.Target != saved.Target || sc.Transport != saved.Transport {
			return refuse(l.t("待确认命令已过期或已变动，请重新准备后再确认执行。", "Staged command expired or changed; stage it again before confirming."))
		}
		ectx, cancel := context.WithTimeout(ctx, termExecTimeout)
		defer cancel()
		out, rerr := a.runner().Run(ectx, saved, sc.Command)
		rec := termRunRecord{
			Target:     saved.Target,
			Transport:  saved.Transport,
			Command:    sc.Command,
			RanAt:      now.UnixMilli(),
			ExitCode:   out.ExitCode,
			DurationMs: out.DurationMs,
			Output:     out.Output,
			Truncated:  out.Truncated,
		}
		if rerr != nil {
			rec.ErrMsg = describeTermError(l, saved.Transport, rerr)
		}
		a.term().addHistory(acct.AccountID, rec)
		if rerr != nil {
			return refuse(rec.ErrMsg)
		}
		if out.ExitCode != 0 {
			st.Note = fmt.Sprintf(l.t("命令执行完成，退出码 %d", "Command finished with exit code %d"), out.ExitCode)
			st.NoteTone = wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		return done(l.t("命令已执行完成（退出码 0）", "Command finished (exit 0)"))

	case actTermChatStage:
		if !chatShellSupported(acct.Provider) {
			return refuse(l.t("这个服务不支持在对话里执行沙箱命令。", "This service does not support running sandbox commands via chat."))
		}
		cmd, cerr := validateTerminalCommand(r.Values[fieldTermChatCmd])
		if cerr != nil {
			return refuse(commandValidationWords(l, cerr))
		}
		digest := commandDigest(termModeChat, acct.AccountID, "", cmd)
		a.term().putStaged(acct.AccountID, stagedCommand{
			Mode:     termModeChat,
			Target:   acct.AccountID,
			Command:  cmd,
			Digest:   digest,
			StagedAt: now,
		})
		return done(l.t("已准备好命令，请核对后点「确认发给助手执行」", "Command staged; review it and tap Confirm and send to chat"))

	case actTermChatSend:
		if !chatShellSupported(acct.Provider) {
			return refuse(l.t("这个服务不支持在对话里执行沙箱命令。", "This service does not support running sandbox commands via chat."))
		}
		if err := exhausted(acct.Quota, now.UnixMilli()); err != nil {
			return failed(err, "")
		}
		sc, ok := a.term().popStaged(acct.AccountID, termModeChat, r.Values[valTermDigest], now)
		if !ok {
			return refuse(l.t("待确认命令已过期或已变动，请重新准备后再确认发送。", "Staged command expired or changed; stage it again before confirming."))
		}
		sessionKey, err := takeoverSessionKey(ctx, space, acct.AccountID)
		if err != nil {
			return failed(err, "")
		}
		prompt := "请在你的沙箱终端里执行下面这条命令，并原样返回退出码与输出（不要擅自改动命令）：\n```sh\n" + sc.Command + "\n```"
		msg, err := space.Send(ctx, protocol.SendParams{
			SessionKey: sessionKey,
			ClientID:   uuid.NewString(),
			Text:       prompt,
		})
		if err != nil {
			return failed(err, "")
		}
		if msg.Local != nil {
			switch msg.Local.Status {
			case protocol.LocalFailed:
				words := l.t("没有发出去", "Not sent")
				if msg.Local.Error != "" {
					words += l.t("：", ": ") + (&tools{club: a.club, browser: a.browser}).problemWords(ctx, space, acct.AccountID, msg.Local.Error)
				}
				return refuse(words)
			case protocol.LocalUnconfirmed:
				st.Note = l.t("命令可能已经发给助手，但尚未得到对方确认；请先到对话里查看，不要直接重发。",
					"Command may have been delivered but was not confirmed; check the chat before retrying.")
				st.NoteTone = wire.ToneCaution
				return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
			}
		}
		return done(l.t("已发到主对话让它执行，稍后在对话里查看输出", "Sent to the agent's main chat; check the chat for its output"))

	case actTermCancel:
		a.term().clearStaged(acct.AccountID)
		return done(l.t("已取消待执行命令", "Canceled staged command"))

	case actTermMachineStage, actTermMachinePreset:
		if !takeoverSupported(acct.Provider) || a.hub == nil {
			return refuse(l.t("这个账号没有可用的已配对 Persona 机器。", "This account does not have a paired Persona machine."))
		}
		mid := takeoverMachineID(acct)
		mst, ok := a.machineState(ctx, mid)
		if !ok || !mst.Exists {
			return refuse(l.t("这台电脑尚未接入 Persona，请先在账号页完成接入。", "This machine is not paired with Persona yet; pair it on the account page first."))
		}
		if mst.Pending {
			return refuse(l.t("配对电脑尚未完成主人确认，请先到「机器」页确认。", "The paired computer is still pending confirmation on the Machines page."))
		}
		rawCmd := r.Values[fieldTermMachineCmd]
		if r.Action == actTermMachinePreset {
			rawCmd = r.Values[valTermPreset]
		}
		cmd, cerr := validateTerminalCommand(rawCmd)
		if cerr != nil {
			return refuse(commandValidationWords(l, cerr))
		}
		digest := commandDigest(termModeMachineExec, mst.MachineID, termModeMachineExec, cmd)
		a.term().putStaged(acct.AccountID, stagedCommand{
			Mode:      termModeMachineExec,
			Target:    mst.MachineID,
			Transport: termModeMachineExec,
			Command:   cmd,
			Digest:    digest,
			StagedAt:  now,
		})
		return done(l.t("已准备好命令，请核对后点「提交主人审批并执行」", "Command staged; review it and tap Request owner approval and run"))

	case actTermMachineExec:
		if !takeoverSupported(acct.Provider) || a.hub == nil {
			return refuse(l.t("这个账号没有可用的已配对 Persona 机器。", "This account does not have a paired Persona machine."))
		}
		hub := a.hub.Load()
		if hub == nil {
			return refuse(l.t("当前无法连接到 Persona，请稍后再试。", "Cannot reach Persona right now; try again shortly."))
		}
		mid := takeoverMachineID(acct)
		sc, ok := a.term().popStaged(acct.AccountID, termModeMachineExec, r.Values[valTermDigest], now)
		if !ok || sc.Target != mid {
			return refuse(l.t("待审批命令已过期或已变动，请重新准备后再提交审批。", "Staged command expired or changed; stage it again before requesting approval."))
		}
		ectx, cancel := context.WithTimeout(ctx, termExecTimeout+5*time.Second)
		defer cancel()
		execReason := fmt.Sprintf("Ensemble (%s) remote terminal command", accountName(acct))
		res, rerr := hub.MachinesExec(ectx, wire.MachinesExecParams{
			MachineID:      wire.HubMachineID(mid),
			Command:        sc.Command,
			TimeoutMs:      int(termExecTimeout.Milliseconds()),
			MaxOutputBytes: termMaxOutputSize,
			Reason:         execReason,
		})
		if rerr != nil {
			msg := describeMachineExecError(l, rerr)
			a.term().addHistory(acct.AccountID, termRunRecord{
				Target:    mid,
				Transport: termModeMachineExec,
				Command:   sc.Command,
				RanAt:     now.UnixMilli(),
				ExitCode:  1,
				ErrMsg:    msg,
			})
			return refuse(msg)
		}
		outText, localTrunc := capTerminalOutput(res.Output, termMaxOutputSize)
		rec := termRunRecord{
			Target:     string(res.MachineID),
			Transport:  termModeMachineExec,
			Command:    sc.Command,
			RanAt:      now.UnixMilli(),
			ExitCode:   res.ExitCode,
			DurationMs: res.DurationMs,
			Output:     outText,
			Truncated:  res.Truncated || localTrunc,
		}
		if rec.Target == "" {
			rec.Target = mid
		}
		if res.TimedOut {
			rec.ErrMsg = l.t("命令执行超时，已在规定时间终止。", "Command timed out.")
		} else if res.Cancelled {
			rec.ErrMsg = l.t("命令执行已取消。", "Command was cancelled.")
		}
		a.term().addHistory(acct.AccountID, rec)
		if res.TimedOut {
			st.Note = rec.ErrMsg
			st.NoteTone = wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if res.Cancelled {
			st.Note = rec.ErrMsg
			st.NoteTone = wire.ToneNeutral
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if res.ExitCode != 0 {
			st.Note = fmt.Sprintf(l.t("命令已在配对电脑执行完成，退出码 %d", "Command finished on paired machine with exit code %d"), res.ExitCode)
			st.NoteTone = wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if rec.Truncated {
			return done(l.t("命令已审批执行完成（退出码 0，输出已截断）", "Approved command finished (exit 0, output truncated)"))
		}
		return done(l.t("命令已审批执行完成（退出码 0）", "Approved command finished (exit 0)"))

	case actTermComputerRefresh:
		if acct.Provider != providerGemini {
			return refuse(l.t("这个账号不支持 Gemini 云电脑状态检查。", "This account does not support Gemini cloud computer status checks."))
		}
		pctx, cancel := context.WithTimeout(ctx, termProbeTimeout)
		defer cancel()
		box, supported, berr := space.AccountBoxState(pctx, acct.AccountID)
		if !supported {
			return refuse(l.t("这个账号不支持 Gemini 云电脑状态检查。", "This account does not support Gemini cloud computer status checks."))
		}
		if berr != nil {
			words := a.terminalProblemText(ctx, l, space, acct, berr)
			prev, _ := a.term().getComputer(acct.AccountID)
			a.term().setComputer(acct.AccountID, termComputerState{Box: prev.Box, CheckedAt: now.UnixMilli(), ErrMsg: a.terminalProblemShort(ctx, l, space, acct, berr)})
			return refuse(words)
		}
		a.term().setComputer(acct.AccountID, termComputerState{Box: box, CheckedAt: now.UnixMilli()})
		return done(l.t("只读状态检查通过", "Read-only status check succeeded"))

	case actTermComputerStage, actTermComputerPreset:
		if acct.Provider != providerGemini {
			return refuse(l.t("这个账号不支持在 Gemini 云电脑里执行命令。", "This account does not support running commands on Gemini's cloud computer."))
		}
		rawCmd := r.Values[fieldTermComputerCmd]
		if r.Action == actTermComputerPreset {
			rawCmd = r.Values[valTermPreset]
			if !isGeminiComputerPreset(rawCmd) {
				return refuse(l.t("这个快捷命令不在只读预设清单里。", "This preset is not in the read-only preset list."))
			}
		}
		cmd, cerr := validateTerminalCommand(rawCmd)
		if cerr != nil {
			return refuse(commandValidationWords(l, cerr))
		}
		digest := commandDigest(termModeComputer, acct.AccountID, "gemini_computer", cmd)
		a.term().putStaged(acct.AccountID, stagedCommand{
			Mode:      termModeComputer,
			Target:    acct.AccountID,
			Transport: "gemini_computer",
			Command:   cmd,
			Digest:    digest,
			StagedAt:  now,
		})
		return done(l.t("已准备好命令，请核对后点「确认在云电脑执行」", "Command staged; review it and tap Confirm and run on cloud computer"))

	case actTermComputerRun:
		if acct.Provider != providerGemini {
			return refuse(l.t("这个账号不支持在 Gemini 云电脑里执行命令。", "This account does not support running commands on Gemini's cloud computer."))
		}
		if err := exhausted(acct.Quota, now.UnixMilli()); err != nil {
			return failed(err, "")
		}
		sc, ok := a.term().popStaged(acct.AccountID, termModeComputer, r.Values[valTermDigest], now)
		if !ok {
			return refuse(l.t("待确认命令已过期或已变动，请重新准备后再确认执行。", "Staged command expired or changed; stage it again before confirming."))
		}
		targetLabel := l.t("Gemini 云电脑", "Gemini cloud computer")
		if space.SupportsAccountComputer(ctx, acct.AccountID) {
			ectx, cancel := context.WithTimeout(ctx, termComputerWait)
			defer cancel()
			res, rerr := space.RunAccountComputer(ectx, acct.AccountID, sc.Command)
			if rerr != nil {
				words := a.terminalProblemText(ctx, l, space, acct, rerr)
				a.term().addHistory(acct.AccountID, termRunRecord{
					Target:    targetLabel,
					Transport: "gemini_computer",
					Command:   sc.Command,
					RanAt:     now.UnixMilli(),
					ExitCode:  1,
					ErrMsg:    a.terminalProblemShort(ctx, l, space, acct, rerr),
				})
				return refuse(words)
			}
			if res.SessionID != "" {
				targetLabel += " (" + res.SessionID + ")"
			}
			if res.Status == provider.Unconfirmed {
				a.term().addHistory(acct.AccountID, termRunRecord{
					Target:      targetLabel,
					Transport:   "gemini_computer",
					Command:     sc.Command,
					RanAt:       now.UnixMilli(),
					DurationMs:  res.DurationMs,
					Unconfirmed: true,
					SessionURL:  res.SessionURL,
					Output:      l.t("命令可能已发往云电脑任务会话，但尚未收到完成确认；请先打开会话查看，不要直接重发。", "Command may have been delivered to the cloud computer session but was not confirmed; open the session to check before retrying."),
				})
				st.Note = l.t("命令可能已发往云电脑任务会话，但尚未得到对方确认；请先到会话里查看，不要直接重发。",
					"Command may have been delivered to the cloud computer session but was not confirmed; check the session before retrying.")
				st.NoteTone = wire.ToneCaution
				return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
			}
			outText, truncated := capTerminalOutput(res.Output, termMaxOutputSize)
			if !res.Answered {
				if outText == "" {
					outText = l.t("命令已发往独立云电脑任务会话，后台仍在运行中；请点上方会话链接查看实时进度，不要直接重发。",
						"Command was sent to the dedicated cloud computer task session and is still running; open the session link above to check live progress before retrying.")
				}
				a.term().addHistory(acct.AccountID, termRunRecord{
					Target:      targetLabel,
					Transport:   "gemini_computer",
					Command:     sc.Command,
					RanAt:       now.UnixMilli(),
					DurationMs:  res.DurationMs,
					Output:      outText,
					Truncated:   truncated,
					Unconfirmed: true,
					SessionURL:  res.SessionURL,
				})
				st.Note = l.t("命令已发往独立云电脑任务会话，后台仍在运行中；请先到会话里查看，不要直接重发。",
					"Command was sent to the dedicated cloud computer task session and is still running; check the session before retrying.")
				st.NoteTone = wire.ToneCaution
				return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
			}
			a.term().addHistory(acct.AccountID, termRunRecord{
				Target:     targetLabel,
				Transport:  "gemini_computer",
				Command:    sc.Command,
				RanAt:      now.UnixMilli(),
				ExitCode:   res.ExitCode,
				DurationMs: res.DurationMs,
				Output:     outText,
				Truncated:  truncated,
				SessionURL: res.SessionURL,
			})
			if res.ExitCode != 0 {
				st.Note = fmt.Sprintf(l.t("命令已在云电脑执行完成，退出码 %d", "Command finished on cloud computer with exit code %d"), res.ExitCode)
				st.NoteTone = wire.ToneCaution
				return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
			}
			return done(l.t("已在云电脑执行完成（退出码 0）", "Command finished on cloud computer (exit 0)"))
		}
		return refuse(l.t("当前账号尚未启用云电脑命令通道。", "Cloud computer command runner is unavailable for this account."))
	}
	return nil, nil
}

func commandValidationWords(l lang, err error) string {
	switch err.Error() {
	case "empty":
		return l.t("还没输入要执行的命令。", "Enter a command first.")
	case "too_long":
		return l.t(fmt.Sprintf("命令太长了（最多 %d 个字符），请精简后再试。", termMaxCommandLen),
			fmt.Sprintf("Command is too long (at most %d characters).", termMaxCommandLen))
	default:
		return l.t("命令里包含不支持的控制字符，请检查后再试。", "Command contains unsupported control characters.")
	}
}

func describeTermError(l lang, transport string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return l.t("执行超时（未在规定时间内返回），请确认远端在线且命令不会一直等待输入。",
			"Timed out before the remote command returned; check reachability and ensure the command is non-interactive.")
	}
	if errors.Is(err, exec.ErrNotFound) {
		if transport == core.TransportTailscale {
			return l.t("当前环境里找不到 tailscale 命令：如果扩展运行在受限沙箱中，或本机未安装 Tailscale CLI，可改用普通 SSH，或使用已配对机器的审批命令执行。",
				"tailscale binary was not found in this environment; switch to plain SSH or use owner-approved machines.exec on a paired machine.")
		}
		return l.t("当前环境里找不到 ssh 命令（可能被扩展沙箱限制）：可使用已配对机器的「经审批在配对电脑执行命令」或直达原生终端。",
			"ssh binary is unavailable in this environment (likely restricted by the extension sandbox); use owner-approved machines.exec or open the native terminal on a paired machine.")
	}
	return l.t("无法启动远端连接："+err.Error(), "Failed to start remote connection: "+err.Error())
}

func describeMachineExecError(l lang, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return l.t("等待审批或执行超时，未确认命令是否完成。", "Timed out waiting for approval or execution.")
	}
	var ee *ext.Error
	if errors.As(err, &ee) {
		switch ee.Code {
		case "user_denied":
			return l.t("主人已拒绝这次审批，未在配对电脑执行命令。", "Owner declined the approval card; nothing was run on the machine.")
		case "approval_not_granted":
			return l.t("命令审批未通过或已超时，未在配对电脑执行。", "Command approval was not granted or timed out; nothing was run.")
		case "permission_denied":
			return l.t("尚未授予「在配对电脑执行命令」权限：请在 Persona 扩展权限设置中开启后再试。", "machines.exec permission is not granted; enable it in Persona's extension permissions first.")
		case "machine_offline":
			return l.t("配对电脑当前离线，无法执行命令。", "The paired computer is offline.")
		case "machine_pending":
			return l.t("配对电脑尚未完成主人确认，请先到「机器」页确认。", "The paired computer is still pending confirmation on the Machines page.")
		case "node_outdated":
			return l.t("这台电脑上的节点版本过旧，暂不支持受控命令执行，请先升级节点。", "The computer's node does not support machines.exec yet; upgrade the node first.")
		case "not_found", "other_manager":
			return l.t("找不到这台配对电脑或当前无权访问它。", "Paired computer was not found or is not owned by this manager.")
		}
	}
	return l.t("命令未能执行："+err.Error(), "Command failed: "+err.Error())
}

func transportLabel(t string) string {
	if core.NormalizeTransport(t) == core.TransportTailscale {
		return "Tailscale SSH"
	}
	return "SSH"
}

func boxStateLabel(l lang, state string) (string, string) {
	switch state {
	case "running":
		return l.t("运行中", "Running"), wire.TonePositive
	case "starting":
		return l.t("启动中", "Starting"), wire.ToneCaution
	case "paused":
		return l.t("已休眠", "Paused"), wire.ToneNeutral
	case "stopped":
		return l.t("已停止", "Stopped"), wire.ToneNeutral
	default:
		return l.t("状态未知", "Unknown"), wire.ToneNeutral
	}
}

func (a *accounts) machineState(ctx context.Context, mid string) (wire.MachinesStatusResult, bool) {
	if a.hub == nil {
		return wire.MachinesStatusResult{}, false
	}
	hub := a.hub.Load()
	if hub == nil {
		return wire.MachinesStatusResult{}, false
	}
	st, err := hub.MachinesStatus(ctx, wire.MachinesStatusParams{MachineID: mid})
	if err != nil {
		return wire.MachinesStatusResult{}, false
	}
	return st, true
}

func (a *accounts) browserMachineLabel(ctx context.Context, space *core.Space, acct protocol.Account) string {
	id, _ := space.AccountMachine(ctx, acct.AccountID)
	if id == "" {
		return ""
	}
	w := basicPlace(acct)
	placerFor(ctx, space, a.browser).name(ctx, &w, id)
	if w.Machine != "" {
		return w.Machine
	}
	return id
}

func (a *accounts) terminalProblemText(ctx context.Context, l lang, space *core.Space, acct protocol.Account, err error) string {
	code := protocol.ReasonOf(err)
	if p, ok := problemOf(l, code, placerFor(ctx, space, a.browser).of(ctx, acct, code)); ok {
		return p.Text(l)
	}
	return reason(l, err, "")
}

func (a *accounts) terminalProblemShort(ctx context.Context, l lang, space *core.Space, acct protocol.Account, err error) string {
	code := protocol.ReasonOf(err)
	if p, ok := problemOf(l, code, basicPlace(acct)); ok && p.Short != "" {
		return p.Short
	}
	return l.t("暂时无法读取状态，请稍后再试。", "Could not check status right now; try again shortly.")
}
