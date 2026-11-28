package extension

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/echo"
)

const (
	terminalTargetPrefixSSH  = "ssh:"
	terminalTargetPrefixEcho = "echo:"
	terminalDemoTarget       = "echo-demo"
)

// terminalBridge exposes Ensemble's interactive PTY sessions over Persona's terminal.* JSON-RPC namespace
// (contributes.terminal_sessions, #16741).
type terminalBridge struct {
	club     *core.Club
	hub      *atomic.Pointer[ext.Hub]
	demoEcho *atomic.Bool
	pty      *core.PTYManager
}

func newTerminalBridge(club *core.Club, hub *atomic.Pointer[ext.Hub], demoEcho *atomic.Bool) *terminalBridge {
	b := &terminalBridge{
		club:     club,
		hub:      hub,
		demoEcho: demoEcho,
	}
	b.pty = core.NewPTYManager(core.PTYConfig{
		MaxSessions: core.DefaultPTYMaxSessions,
		IdleTimeout: core.DefaultPTYIdleTimeout,
		OnFrame: func(sessionID string, seq int, data []byte) error {
			if b.hub == nil {
				return nil
			}
			h := b.hub.Load()
			if h == nil {
				return nil
			}
			return h.TerminalFrame(sessionID, seq, data)
		},
		OnClosed: func(sessionID string, exitCode *int, reason string) {
			if b.hub == nil {
				return
			}
			h := b.hub.Load()
			if h == nil {
				return
			}
			_ = h.TerminalClosed(sessionID, exitCode, reason)
		},
	})
	return b
}

// extTerminalOpenURL builds a validated persona://ext-terminal/<ext_id>/<target> deep link for a full extension page.
func extTerminalOpenURL(target string) (string, bool) {
	if !validTerminalTargetToken(target) {
		return "", false
	}
	u := "persona://ext-terminal/" + ID + "/" + url.PathEscape(target)
	if err := wire.CheckExtensionOpenFor(u, ID); err != nil {
		return "", false
	}
	return u, true
}

// nativeTerminalOpenURL builds a validated persona://terminal/<machine_id> deep link for a paired Persona machine.
func nativeTerminalOpenURL(machineID string) (string, bool) {
	mid := strings.TrimSpace(machineID)
	if !validTerminalTargetToken(mid) {
		return "", false
	}
	u := "persona://terminal/" + url.PathEscape(mid)
	if err := wire.CheckExtensionOpen(u); err != nil {
		return "", false
	}
	return u, true
}

func validTerminalTargetToken(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '\\' || c == '?' || c == '#' {
			return false
		}
	}
	return true
}

func (b *terminalBridge) Open(ctx context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error) {
	sessionID := string(p.SessionID)
	target := strings.TrimSpace(p.Target)
	if !validTerminalTargetToken(target) {
		return wire.TerminalOpenResult{}, &ext.Error{Code: "invalid_target", Message: "invalid terminal target"}
	}
	spec, err := b.resolveTarget(ctx, sessionID, target)
	if err != nil {
		return wire.TerminalOpenResult{}, err
	}
	if err := b.pty.Open(spec, p.Cols, p.Rows); err != nil {
		if spec.CleanupDir != "" {
			// Open already cleans up on start error, safe to ignore if already removed.
		}
		return wire.TerminalOpenResult{}, mapPTYError(err)
	}
	return wire.TerminalOpenResult{
		SessionID: p.SessionID,
		Title:     spec.Title,
	}, nil
}

func (b *terminalBridge) resolveTarget(ctx context.Context, sessionID, target string) (core.PTYSessionSpec, error) {
	space, err := b.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return core.PTYSessionSpec{}, &ext.Error{Code: "unavailable", Message: "workspace unavailable"}
	}
	switch {
	case target == terminalDemoTarget:
		if b.demoEcho == nil || !b.demoEcho.Load() {
			return core.PTYSessionSpec{}, &ext.Error{Code: "precondition_failed", Message: "demo_echo is not enabled"}
		}
		return buildDemoPTYSpec(sessionID, "Ensemble Sandbox Shell")

	case strings.HasPrefix(target, terminalTargetPrefixEcho):
		acctID := strings.TrimPrefix(target, terminalTargetPrefixEcho)
		if acctID == "" {
			return core.PTYSessionSpec{}, &ext.Error{Code: "invalid_target", Message: "missing echo account id"}
		}
		acct, err := space.Account(ctx, acctID)
		if err != nil {
			return core.PTYSessionSpec{}, &ext.Error{Code: "not_found", Message: "account not found"}
		}
		if acct.Provider != echo.Name {
			return core.PTYSessionSpec{}, &ext.Error{Code: "invalid_target", Message: "target account is not an echo demo account"}
		}
		if acct.Status == protocol.AccountPaused {
			return core.PTYSessionSpec{}, &ext.Error{Code: "precondition_failed", Message: "account is paused"}
		}
		return buildDemoPTYSpec(sessionID, accountName(acct)+" · Sandbox")

	case strings.HasPrefix(target, terminalTargetPrefixSSH):
		acctID := strings.TrimPrefix(target, terminalTargetPrefixSSH)
		if acctID == "" {
			return core.PTYSessionSpec{}, &ext.Error{Code: "invalid_target", Message: "missing ssh account id"}
		}
		acct, err := space.Account(ctx, acctID)
		if err != nil {
			return core.PTYSessionSpec{}, &ext.Error{Code: "not_found", Message: "account not found"}
		}
		if acct.Status == protocol.AccountPaused {
			return core.PTYSessionSpec{}, &ext.Error{Code: "precondition_failed", Message: "account is paused"}
		}
		cfg, ok, err := space.AccountSSHTarget(ctx, acct.AccountID)
		if err != nil {
			return core.PTYSessionSpec{}, &ext.Error{Code: "internal", Message: "failed to read ssh target"}
		}
		if !ok {
			return core.PTYSessionSpec{}, &ext.Error{Code: "precondition_failed", Message: "no ssh target configured for this account"}
		}
		bin, args, err := core.BuildSSHPtyCommand(cfg)
		if err != nil {
			return core.PTYSessionSpec{}, &ext.Error{Code: "invalid_target", Message: err.Error()}
		}
		return core.PTYSessionSpec{
			SessionID: sessionID,
			Title:     accountName(acct) + " · " + cfg.Target,
			Bin:       bin,
			Args:      args,
			Env:       core.CleanSSHPTYEnv(),
		}, nil

	default:
		return core.PTYSessionSpec{}, &ext.Error{Code: "invalid_target", Message: "unsupported terminal target"}
	}
}

func buildDemoPTYSpec(sessionID, title string) (core.PTYSessionSpec, error) {
	bin, args, env, dir, err := core.BuildDemoEchoPtyCommand()
	if err != nil {
		return core.PTYSessionSpec{}, &ext.Error{Code: "internal", Message: "failed to prepare demo sandbox"}
	}
	return core.PTYSessionSpec{
		SessionID:  sessionID,
		Title:      title,
		Bin:        bin,
		Args:       args,
		Env:        env,
		Dir:        dir,
		CleanupDir: dir,
	}, nil
}

func (b *terminalBridge) Input(_ context.Context, sessionID string, data []byte) error {
	return mapPTYError(b.pty.Input(sessionID, data))
}

func (b *terminalBridge) Resize(_ context.Context, p wire.TerminalResizeParams) error {
	return mapPTYError(b.pty.Resize(string(p.SessionID), p.Cols, p.Rows))
}

func (b *terminalBridge) Close(_ context.Context, p wire.TerminalCloseParams) error {
	return mapPTYError(b.pty.Close(string(p.SessionID), p.Reason))
}

func (b *terminalBridge) CloseAll(reason string) {
	if b != nil && b.pty != nil {
		b.pty.CloseAll(reason)
	}
}

func mapPTYError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, core.ErrPTYTooManySessions):
		return &ext.Error{Code: "too_many_sessions", Message: err.Error()}
	case errors.Is(err, core.ErrPTYSessionNotFound):
		return &ext.Error{Code: "not_found", Message: err.Error()}
	case errors.Is(err, core.ErrPTYInvalidSize):
		return &ext.Error{Code: "invalid_params", Message: err.Error()}
	case errors.Is(err, core.ErrPTYFrameTooLarge):
		return &ext.Error{Code: "frame_too_large", Message: err.Error()}
	case errors.Is(err, core.ErrPTYAlreadyExists):
		return &ext.Error{Code: "already_exists", Message: err.Error()}
	default:
		return &ext.Error{Code: "pty_error", Message: err.Error()}
	}
}
