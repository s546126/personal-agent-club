// Package extension is the Persona shell: club as a Persona extension program. It contributes the agent source "pac"
// (the hub's agentsrc.* calls go to the club's default space through the same core.Club.Call the serve shell uses,
// and the space's changes go back as agentsrc.updated), its pages (the home, its card and the accounts page, all
// drawn by accounts.go) and the
// connector "club" carrying the assistant's club_agent_* tools (tools.go). Its data lives in the extension's data
// directory (storage.go).
package extension

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/browser"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/provider/muse2api"
	"github.com/nwverse/ensemble/internal/provider/web"
	"github.com/nwverse/ensemble/internal/runtime"
)

// ID is the extension's id; Contribution is its agent_sources contribution, the prefix of its session ids in
// Persona (<contribution>:<sessionKey>), which must never change.
const (
	ID           = "personal-agent-club"
	Contribution = "pac"
)

// Config is the extension's configuration (persona-extension.json "configuration").
type Config struct {
	DemoEcho bool `json:"demo_echo"`
	// BrowserProviders adds Muse, Claude Duet, Gemini Tasks and Dots through a paired computer's browser. On unless
	// switched off (as the manifest's default says): nothing is reached until an account is paired on a computer.
	BrowserProviders *bool `json:"browser_providers"`
	AssistantRuntime *bool `json:"assistant_runtime"`
	// Muse2APIURL is the address of the owner's muse2api server; set, the muse2api provider is added. The manifest's
	// "config:muse2api_url:http" lets the egress proxy through to that address only.
	Muse2APIURL   string `json:"muse2api_url"`
	Muse2APIModel string `json:"muse2api_model"`
	// InboxConversations puts each chat of the paired accounts into Persona's Inbox as one row (off unless switched
	// on); InboxReply lets the owner answer there (on); InboxViaSubscription has a subscription read the rows
	// instead of the Inbox itself; TriageRules are the owner's own words for sorting them.
	InboxConversations   bool   `json:"inbox_conversations"`
	InboxReply           *bool  `json:"inbox_reply"`
	InboxViaSubscription bool   `json:"inbox_via_subscription"`
	InboxPush            *bool  `json:"inbox_push"`
	TriageRules          string `json:"triage_rules"`
}

func (c Config) browserProvidersEnabled() bool {
	return c.BrowserProviders == nil || *c.BrowserProviders
}

func (c Config) assistantRuntimeEnabled() bool {
	return c.AssistantRuntime == nil || *c.AssistantRuntime
}

func resolveRuntimeSocket() string {
	if sock := strings.TrimSpace(os.Getenv("PERSONA_RUNTIME_SOCKET")); sock != "" {
		return sock
	}
	// Never dial the machine's live ~/.persona/runtime.sock from go test by default.
	if testing.Testing() {
		return ""
	}
	return runtime.DefaultPersonaSocketPath()
}

// Program is the extension program serving club. The club is closed when the program ends.
func Program(club *core.Club, version string) ext.Program { return program(club, version, nil, nil) }

// program is Program with the phone-alert sender given (tests); nil sends through the hub's notify route with the
// token the hub gave this run. onRound, if any, is called after each round of alerts.
func program(club *core.Club, version string, push pushFunc, onRound func()) ext.Program {
	var hubAPIPtr atomic.Pointer[hubAPI]
	hubAPIPtr.Store(&hubAPI{})
	if push == nil {
		push = hubPush(HTTPClient(), func() hubAPI { return *hubAPIPtr.Load() })
	}
	var hubPtr atomic.Pointer[ext.Hub]
	driver := browser.NewPersonaHubDriver(&hubPtr)
	var runtimeOn atomic.Bool
	runtimeOn.Store(true)
	var demoEchoOn atomic.Bool
	var machinesExecScope atomic.Int32 // 0 = unknown (tests without Init), 1 = granted, -1 = not granted
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(&Config{})
	inboxCfg := func() inboxConfig { return cfgPtr.Load().inbox() }
	in := newInbox(club, driver, inboxCfg)
	note := newNotifier(in, hubPtr.Load, push)
	note.onRound = onRound
	page := accountsPage(club, &hubPtr, driver, &runtimeOn, &machinesExecScope, inboxCfg)
	tool := toolsHandler(club, driver)
	termBridge := newTerminalBridge(club, &hubPtr, &demoEchoOn)
	return ext.Program{
		ID:       ID,
		Version:  version,
		Manifest: json.RawMessage(pac.Manifest),
		Init: func(ctx context.Context, init *ext.Init, hub *ext.Hub) (map[string]any, error) {
			hubPtr.Store(hub)
			var cfg Config
			if err := init.Config(&cfg); err != nil {
				return nil, err
			}
			runtimeOn.Store(cfg.assistantRuntimeEnabled())
			demoEchoOn.Store(cfg.DemoEcho)
			if slices.Contains(init.Hub.Scopes, "machines.exec") {
				machinesExecScope.Store(1)
			} else if len(init.Hub.Scopes) > 0 {
				machinesExecScope.Store(-1)
			}
			cfgPtr.Store(&cfg)
			if slices.Contains(init.Hub.Scopes, "notify") { // only when the owner granted it
				hubAPIPtr.Store(&hubAPI{URL: init.Hub.URL, Token: init.Hub.Token})
			}
			if cfg.browserProvidersEnabled() {
				for _, site := range []browser.Site{browser.SiteMuse, browser.SiteClaudeDuet, browser.SiteGeminiTasks, browser.SiteDots} {
					if _, ok := club.Registry().Get(site.Provider); !ok {
						club.Registry().Register(browser.New(site, driver))
					}
				}
			}
			if strings.TrimSpace(cfg.Muse2APIURL) != "" {
				// Through the hub's egress proxy, like every provider (storage.go), but waiting as long for an answer's
				// headers as for the answer: muse2api answers in one piece once muse.ai has finished. A bad address
				// leaves muse2api out (listed as unavailable) rather than stopping the whole extension.
				client := HTTPClient()
				client.Transport.(*http.Transport).ResponseHeaderTimeout = muse2api.AnswerTimeout
				m, err := muse2api.New(muse2api.Options{Client: web.UseClient(client), URL: cfg.Muse2APIURL,
					Model: cfg.Muse2APIModel})
				if err != nil {
					slog.Warn("muse2api left out", "err", err)
				} else {
					club.Registry().Register(m)
				}
			}
			if cfg.DemoEcho {
				if _, ok := club.Registry().Get(echo.Name); !ok {
					club.Registry().Register(echo.New())
				}
			}
			if _, err := club.Space(ctx, core.DefaultSpace); err != nil {
				return nil, err
			}
			var bridgeDone chan struct{}
			if cfg.assistantRuntimeEnabled() {
				if sock := resolveRuntimeSocket(); sock != "" {
					dataDir := cmp.Or(init.Info().DataDir, ext.Env().Data)
					statePath := ""
					if dataDir != "" {
						statePath = filepath.Join(dataDir, "runtime-state.json")
					}
					bridge := runtime.New(runtime.Options{
						Client:     runtime.NewDirectClient(club, core.DefaultSpace),
						SocketPath: sock,
						StatePath:  statePath,
						Version:    version,
						ToolRelay:  true,
					})
					bridgeDone = make(chan struct{})
					go func() {
						defer close(bridgeDone)
						_ = bridge.Run(ctx)
					}()
				}
			}
			sub := club.Subscribe(core.DefaultSpace)
			note.seed(ctx) // the log of alerts starts now: what waits already is remembered, not alerted
			go note.run(ctx)
			go forward(ctx, sub, hub, club, bridgeDone, note, termBridge)
			// The connector as the manifest has it: the hub wants the program to say it has it.
			var m struct {
				Contributes struct {
					Connectors json.RawMessage `json:"connectors"`
				} `json:"contributes"`
			}
			if err := json.Unmarshal(pac.Manifest, &m); err != nil {
				return nil, err
			}
			return map[string]any{"connectors": m.Contributes.Connectors}, nil
		},
		UI: map[string]ext.Page{HomeUI: page, HomeCardUI: page, AccountsUI: page, InboxItemUI: in.itemPage()},
		Namespaces: map[string]ext.Handler{
			ext.ConnectorNamespace: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				if ext.Contribution(params) == InboxConnector {
					return in.handler()(ctx, method, params)
				}
				return tool(ctx, method, params)
			},
			ext.TerminalNamespace: ext.HandleTerminal(termBridge),
			protocol.AgentSourceNamespace: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				if c := ext.Contribution(params); c != "" && c != Contribution {
					return nil, ext.NoContribution(c)
				}
				// The hub tells "no such method" (unknown_method) from "this source cannot" (unsupported).
				if !slices.Contains(protocol.AgentSourceMethods, method) {
					return nil, ext.UnknownMethod(protocol.AgentSourceNamespace + "." + method)
				}
				return club.Call(ctx, core.DefaultSpace, protocol.AgentSourceNamespace+"."+method, params)
			},
		},
	}
}

// forward sends the space's changes to the hub until the program ends, then closes the club. A change also redraws
// the extension's pages where they are on screen (the hub coalesces these).
func forward(ctx context.Context, sub *core.Subscription, hub *ext.Hub, club *core.Club, bridgeDone <-chan struct{}, note *notifier, termBridge *terminalBridge) {
	defer club.Close()
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			termBridge.CloseAll("extension_stopped")
			if bridgeDone != nil {
				<-bridgeDone
			}
			<-note.done // the alerts stop before the club closes under them
			return
		case <-sub.Ready():
			for _, e := range sub.Take() {
				_ = hub.Notify(protocol.EventUpdated, protocol.UpdatedEvent{Contribution: Contribution, SessionKeys: e.SessionKeys})
			}
			for _, ui := range []string{HomeUI, HomeCardUI, AccountsUI} {
				_ = hub.Invalidate(ui)
			}
			note.poke(ctx)
		}
	}
}
