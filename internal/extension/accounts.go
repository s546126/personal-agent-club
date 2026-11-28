package extension

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf16"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/browser"
	"github.com/nwverse/ensemble/internal/provider/cue"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/provider/muse2api"
)

// productName is the product as the pages name it. It must read as the manifest's "name" does, since the pages send
// people to Settings › Extensions › <name>; a rename of the product is this one line and the manifest's.
const productName = "Ensemble"

// The extension's pages (contributes.ui). The home is the page a first-time user lands on: what this is, where it
// stands, the one thing to do next, then the accounts at a glance (those that need the user first) and one row to add
// another, or, before the first account, the ways to add one; an account's own page, the add-account page and the
// pairing pages open from there.
//
//   - HomeUI is the home, an entry in the navigation (nav_page). The web's sidebar lists only a few extension pages
//     and puts the rest under 更多, so copy that sends people there says so (homeWhere).
//   - HomeCardUI is the card on Overview, and in Settings › Extensions on the phones (overview_card): the counts, the
//     account that needs the user first and the one next step, opening HomeUI.
//   - AccountsUI is Settings › Extensions › <name> › 账号 (plugin_page), which the assistant's guide points at: the
//     management list, every account with its details and one row to add another, without the home's introduction,
//     state and next step.
//
// They list the paired accounts and pair, pause, resume, sync, rename and unpair them through the core's club.*
// methods, the same ones the serve shell's admin token reaches.
//
// Credentials go in and never come out: the API key field is a secret field (the apps never echo it and the
// extension cannot prefill it), the key goes straight to club.accounts.add and is kept nowhere in the page's state,
// and nothing the page draws or logs carries it.
const (
	HomeUI     = "home"
	HomeCardUI = "home-card"
	AccountsUI = "accounts"
)

// settingsPath is where the extension's settings are, for the pages to send people there (a page cannot link to
// the host's settings page).
func settingsPath(l lang) string {
	return l.t("设置 › 扩展 › "+productName+" › 设置", "Settings › Extensions › "+productName+" › Settings")
}

// The settings' titles as the manifest has them, for the pages that send people to one. The manifest has one
// language (Chinese), so the English pages quote these titles too: they are what the settings page shows.
const (
	settingBrowser  = "用电脑上的浏览器连接网页版助手"
	settingRuntime  = "让这些账号可以当主助手"
	settingInbox    = "把 AI 对话放进收件箱"
	settingMuse2API = "自建 Muse 转接服务的地址"
)

// homeTitle is the home's title, as the manifest's navigation entry has it (one language there too).
const homeTitle = "我的 AI 助手"

// homeCardTitle is the Overview card's title, as the manifest's overview_card has it. The hub lists every
// contribution by its title on the extension's settings page, so the card must not read as the home does; the
// Overview page draws it as the card's header too, where "· 概况" (at a glance) reads as what the card is.
const homeCardTitle = homeTitle + " · 概况"

// accountsTitle is the settings page's link to the accounts, as the manifest has it.
const accountsTitle = "账号"

// addTitle is the add-account page's title, and the row that opens it.
const addTitle = "添加账号"

// homeWhere is where the home is, for the assistant to tell people (its words are Chinese): true on every app, where
// the navigation may keep it under 更多 and the phones show the card in Settings › Extensions.
const homeWhere = "「" + homeTitle + "」页（在导航里，导航放不下时收在「更多」里；也可以点概览页上的「" + homeTitle +
	"」卡片，或者走 设置 › 扩展 › " + productName + " › " + accountsTitle + "）"

// browserName is the browser Persona drives on a computer (browser.run): the one a browser account must be signed
// in to. Pairing runs in a task space of its own inside it, with that browser's sign-ins.
const browserName = "ego lite"

// Pages of the accounts UI (pageState.Page).
const (
	pageList    = ""
	pageAccount = "account"
	pagePair    = "pair"
	pageGrok    = "grok"
	pageAdd     = "add" // the ways to add an account, opened from the home and from 账号
)

// Actions of the accounts UI.
const (
	actSync               = "sync"
	actPause              = "pause"
	actResume             = "resume"
	actRemove             = "remove"
	actPurge              = "purge"
	actControl            = "control"
	actControlAutoConfirm = "control_auto_confirm"
	actControlAutoCancel  = "control_auto_cancel"
	actLeaseGrant         = "lease_grant"
	actLeaseRevoke        = "lease_revoke"
	actLeaseRevokeAll     = "lease_revoke_all"
	actAssistantBackend   = "assistant_backend"
	actQuotaDisplay       = "quota_display"
	actInbox              = "inbox"
	actRename             = "rename"
	actPair               = "pair"
	actGrokStart          = "grok_start"
	actGrokCheck          = "grok_check"
	actGrokStop           = "grok_cancel"
	actTakeover           = "takeover"
	actWake               = "wake"
	actRecheck            = "recheck"
)

// Field names on the pages.
const (
	fieldCredential       = "credential"
	fieldName             = "display_name"
	fieldControl          = "assistant_control"
	fieldAssistantBackend = "assistant_backend"
	fieldQuotaDisplay     = "quota_display"
	fieldInbox            = "inbox"
	fieldLeaseSession     = "lease_session"
	fieldLeaseTTL         = "lease_ttl"
	fieldLeaseTurns       = "lease_turns"
	fieldLeaseBackground  = "lease_background"
	fieldLeaseNote        = "lease_note"
	valLeaseID            = "lease_id"
)

// What the core takes (core/space.go): a credential of 8 to 8192 characters, a name of at most 80 UTF-16 units. The
// pages check them first so the refusal comes in words.
const (
	minCredential  = 8
	maxCredential  = 8192
	maxDisplayName = 80
)

// pageState is the page's state as the apps send it back: which page, about what, and the last action's outcome.
// Never a credential.
type pageState struct {
	Page        string `json:"p,omitempty"`
	Account     string `json:"a,omitempty"`
	Provider    string `json:"v,omitempty"`
	Login       string `json:"l,omitempty"`
	LoginURL    string `json:"u,omitempty"`
	Note        string `json:"n,omitempty"`
	NoteTone    string `json:"t,omitempty"`
	ConfirmAuto bool   `json:"ca,omitempty"`
	// Chan and Repos are the 仓库 page's channel and repositories (repos_page.go), for 重新检测并挂载.
	Chan  string `json:"c,omitempty"`
	Repos string `json:"r,omitempty"`
}

// Providers paired by pasting a key on the accounts page. Grok Bot signs in through its own page instead, and
// muse2api (a key too) has its own row, since it needs its server's address first.
var keyProviders = []string{"cue", "groq"}

const grokBot = "grok_bot"

// Reasons a Grok Bot sign-in poll fails for now and may still succeed: the sign-in is kept, and asked again.
var retryableLogin = []string{"NETWORK_ERROR", "UPSTREAM_ERROR", "RATE_LIMITED"}

// accountsPage is the accounts UI. runtimeOn says whether the main assistant bridge runs (the setting
// assistant_runtime), which decides whether an account's 主助手 switch does anything.
func accountsPage(club *core.Club, hub *atomic.Pointer[ext.Hub], drv browser.Driver, runtimeOn *atomic.Bool, machinesExecScope *atomic.Int32, inboxCfg func() inboxConfig) ext.Page {
	a := &accounts{club: club, hub: hub, browser: drv, runtimeOn: runtimeOn, machinesExecScope: machinesExecScope, inboxCfg: inboxCfg, terms: newTermState()}
	return ext.Page{Render: a.render, Action: a.action, Image: a.servePicture}
}

// ownRefPrefix starts the ref of an account's own picture that its service gave as image data (avatarOf).
const ownRefPrefix = "own/"

func ownRef(accountID string) string { return ownRefPrefix + accountID }

// servable says whether an own picture given as image data is one the hub takes from ui.image (no SVG).
func servable(own string) bool {
	switch names.DataType(own) {
	case "png", "jpeg", "webp", "gif":
		return true
	}
	return false
}

// servePicture serves the pictures the pages name by ref: an account's own picture its service gave as image data
// (decoded from what the last sync stored, nothing fetched; kept a minute, as a sync may change it), and a service's
// tile (avatarTile).
func (a *accounts) servePicture(ctx context.Context, r *ext.ImageRequest) (*wire.UIImage, error) {
	id, ok := strings.CutPrefix(r.Ref, ownRefPrefix)
	if !ok {
		return avatarTile(ctx, r)
	}
	notFound := &ext.Error{Code: "not_found", Message: "no picture " + r.Ref}
	acct, err := a.account(ctx, id)
	if err != nil || !servable(acct.AvatarURL) {
		return nil, notFound
	}
	_, b64, _ := strings.Cut(acct.AvatarURL, ",")
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) == 0 {
		return nil, notFound
	}
	return ext.Image("", data, 60), nil
}

// avatarTile serves a service's avatar tile (names.Tile), the picture its avatar names by ref: built into the
// program, nothing fetched. A day is as long as the hub keeps a picture; a new tile ships with a new version, whose
// install drops the hub's copies.
func avatarTile(_ context.Context, r *ext.ImageRequest) (*wire.UIImage, error) {
	data, ok := names.Tile(r.Ref)
	if !ok {
		return nil, &ext.Error{Code: "not_found", Message: "no picture " + r.Ref}
	}
	return ext.Image("image/png", data, wire.ImageMaxAgeMost), nil
}

type accounts struct {
	club              *core.Club
	hub               *atomic.Pointer[ext.Hub]
	browser           browser.Driver
	runtimeOn         *atomic.Bool
	machinesExecScope *atomic.Int32
	inboxCfg          func() inboxConfig
	terms             *termState
	termExec          termRunner
}

func (a *accounts) runtimeEnabled() bool { return a.runtimeOn == nil || a.runtimeOn.Load() }

func (a *accounts) machinesExecDenied() bool {
	return a.machinesExecScope != nil && a.machinesExecScope.Load() < 0
}

func (a *accounts) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var st pageState
	_ = r.DecodeState(&st)
	return a.view(ctx, r, st), nil
}

// ui is the page a request is for, so the pages it opens stay in it.
func ui(r *ext.UIRequest) string {
	switch r.Contribution {
	case HomeUI, AccountsUI:
		return r.Contribution
	}
	return AccountsUI
}

func (a *accounts) view(ctx context.Context, r *ext.UIRequest, st pageState) *wire.View {
	l := lang(r.Locale)
	var v *wire.View
	switch {
	case r.Contribution == HomeCardUI || r.Placement == wire.PlacementOverviewCard:
		v = a.cardView(ctx, l)
	case st.Page == pageAccount:
		v = a.accountView(ctx, r, l, &st)
	case st.Page == pageTerminal:
		v = a.terminalView(ctx, r, l, &st)
	case st.Page == pageRepos:
		v = a.reposView(ctx, r, l, &st)
	case st.Page == pagePair:
		v = a.pairView(l, st)
	case st.Page == pageGrok:
		v = a.grokView(ctx, l, &st)
	case st.Page == pageBrowserPair:
		v = a.browserPairView(ctx, l, st)
	case st.Page == pageDone:
		v = a.doneView(ctx, l, st)
	case st.Page == pageAdd:
		v = a.addView(ctx, r, l)
	default:
		v = a.listView(ctx, r, l)
	}
	if st.Note != "" {
		tone := st.NoteTone
		if tone == "" {
			tone = wire.ToneNeutral
		}
		v.Sections = append([]wire.Section{{Items: []wire.Component{{Type: "row", Title: st.Note,
			Badge: noteBadge(l, tone)}}}}, v.Sections...)
	}
	v.State = ext.EncodeState(st)
	return v
}

// noteBadge says what kind of outcome a note is.
func noteBadge(l lang, tone string) *wire.Badge {
	switch tone {
	case wire.TonePositive:
		return &wire.Badge{Text: l.t("完成", "Done"), Tone: tone}
	case wire.ToneDanger:
		return &wire.Badge{Text: l.t("没成功", "Failed"), Tone: tone}
	case wire.ToneCaution:
		return &wire.Badge{Text: l.t("请注意", "Note"), Tone: tone}
	}
	return &wire.Badge{Text: l.t("提示", "Note"), Tone: tone}
}

// overview is where the accounts stand, for the home and its card.
type overview struct {
	accounts  []protocol.Account
	broken    []protocol.Account // need the user: signed out, or failing
	exhausted []protocol.Account // active accounts whose allowance is currently used up
	low       []protocol.Account // active accounts whose allowance is <= 20%
	paused    int
	read      int // the assistant may read
	send      int // ... and send, asking each time (or not)
	backend   int // main assistant
}

func overviewOf(accts []protocol.Account) overview {
	o := overview{accounts: accts}
	for _, acct := range accts {
		exhaustedNow, _ := quotaExhaustedActive(acct.Quota, 0)
		switch {
		case acct.Status == protocol.AccountError:
			o.broken = append(o.broken, acct)
		case acct.Status == protocol.AccountPaused:
			o.paused++
		case exhaustedNow:
			o.exhausted = append(o.exhausted, acct)
		case acct.Quota != nil && acct.Quota.State == protocol.QuotaLow:
			o.low = append(o.low, acct)
		}
		switch core.Control(acct.AssistantControl) {
		case core.ControlRead:
			o.read++
		case core.ControlConfirm, core.ControlFollow, core.ControlAuto:
			o.send++
		}
		if acct.AssistantBackend {
			o.backend++
		}
	}
	return o
}

// stateLine is the state in one line: how many accounts, how many need the user, how many are out of quota, how many
// the assistant may use.
func (o overview) stateLine(l lang) string {
	if len(o.accounts) == 0 {
		return l.t("还没有接入任何账号。", "No accounts yet.")
	}
	s := l.t(fmt.Sprintf("已接入 %d 个账号", len(o.accounts)), plural(len(o.accounts), "account"))
	if n := len(o.broken); n > 0 {
		s += l.t(fmt.Sprintf("，其中 %d 个要你处理", n), fmt.Sprintf(", %d need you", n))
	}
	if n := len(o.exhausted); n > 0 {
		s += l.t(fmt.Sprintf("，%d 个额度用完", n), fmt.Sprintf(", %d out of allowance", n))
	}
	if n := len(o.low); n > 0 {
		s += l.t(fmt.Sprintf("，%d 个额度快用完", n), fmt.Sprintf(", %d low on allowance", n))
	}
	if o.paused > 0 {
		s += l.t(fmt.Sprintf("，%d 个已暂停", o.paused), fmt.Sprintf(", %d paused", o.paused))
	}
	if o.read+o.send == 0 {
		return s + l.t("；Persona 助手现在一个都用不了。", "; the Persona assistant can use none of them yet.")
	}
	return s + l.t(fmt.Sprintf("；Persona 助手能用其中 %d 个。", o.read+o.send),
		fmt.Sprintf("; the Persona assistant can use %d of them.", o.read+o.send))
}

// plural is "1 account", "2 accounts".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// nextStep is the one thing most worth doing now: add the first account, fix a broken one, or let the assistant
// use one. Nothing when all is well. push says whether the step opens a page (an account's); otherwise it points
// at the rows below.
func (o overview) nextStep(l lang, at string) (wire.Component, bool) {
	switch {
	case len(o.accounts) == 0:
		return wire.Component{Type: "row", Icon: "plus", Title: l.t("连接你的第一个账号", "Connect your first account"),
			Subtitle: l.t("在下面「添加账号」里选你常用的那个服务，点进去照提示做，一两分钟就好。",
				"Pick the service you use under Add an account below and follow the steps; it takes a minute or two.")}, true
	case len(o.broken) > 0:
		acct := o.broken[0]
		return wire.Component{Type: "row", Key: "next-" + acct.AccountID, Icon: "warning",
			Title:    l.t("先修好「", "First fix ") + accountTitle(l, acct) + l.t("」", ""),
			Subtitle: problemWords(l, acct) + l.t(" 点这里打开它。", " Tap to open it."),
			Badge:    &wire.Badge{Text: l.t("要处理", "Needs you"), Tone: wire.ToneCaution},
			Action:   openAccount(l, at, acct)}, true
	case len(o.exhausted) > 0:
		acct := o.exhausted[0]
		return wire.Component{Type: "row", Key: "next-" + acct.AccountID, Icon: "warning",
			Title:    l.t("「", "") + accountTitle(l, acct) + l.t("」的额度用完了", " is out of allowance"),
			Subtitle: l.t("发消息前会先提示拦截；点这里查看重置时间或立即同步。", "Sends pause with a warning; tap to see the reset time or sync."),
			Badge:    &wire.Badge{Text: l.t("额度用完", "Used up"), Tone: wire.ToneDanger},
			Action:   openAccount(l, at, acct)}, true
	case o.read+o.send == 0:
		acct := o.accounts[0]
		return wire.Component{Type: "row", Key: "next-" + acct.AccountID, Icon: "robot",
			Title: l.t("让 Persona 助手能帮你看这些账号", "Let the Persona assistant use these accounts"),
			Subtitle: l.t("为了安全，新账号默认不让 Persona 助手碰。点开一个账号，在「助手指挥」里选「只读」（它能帮你看对话、做总结）或「可代发」（还能替你发消息，每次先问你）。",
				"New accounts are off limits to the Persona assistant. Open one and set Assistant control to Read (it can read and sum up) or Confirm (it can also send, asking you each time)."),
			Action: openAccount(l, at, acct)}, true
	}
	return wire.Component{}, false
}

func openAccount(l lang, at string, acct protocol.Account) *wire.Action {
	return &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: accountTitle(l, acct),
		State: ext.EncodeState(pageState{Page: pageAccount, Account: acct.AccountID})}}
}

// cardView is the home at a glance, for Overview: how many accounts there are, how many need the user (naming the
// first), how many the Persona assistant may use, and the one next step. A card has room for a few lines only
// (exttest.CheckView: 2 sections, 12 components), so it is one section: the counts, then the step.
func (a *accounts) cardView(ctx context.Context, l lang) *wire.View {
	v := &wire.View{Title: l.t(homeCardTitle, homeCardTitle+" (My AI assistants at a glance)"), RefreshAfter: 300}
	accts, err := a.accounts(ctx)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	o := overviewOf(accts)
	var items []wire.Component
	if len(o.accounts) == 0 {
		items = append(items, wire.Component{Type: "text", Text: l.t(
			"还没有接入任何账号。接好后，你在 Grok、Muse、Claude 等 AI 服务上的对话会出现在 Persona 的「Agents」页。",
			"No accounts yet. Once you connect one, your chats at AI services such as Grok, Muse and Claude show on Persona's Agents page.")})
	} else {
		items = append(items, wire.Component{Type: "stat_grid", Key: "counts", Stats: o.cardStats(l)})
	}
	v.Sections = []wire.Section{{Items: append(items, o.cardStep(l))}}
	return v
}

// cardStats are the card's counts: connected, needing the user (the first one named), usable by the assistant.
func (o overview) cardStats(l lang) []wire.Stat {
	connected := wire.Stat{Label: l.t("已接入", "Connected"), Value: l.t(fmt.Sprintf("%d 个", len(o.accounts)),
		fmt.Sprint(len(o.accounts)))}
	if o.paused > 0 {
		connected.Detail = l.t(fmt.Sprintf("%d 个已暂停", o.paused), fmt.Sprintf("%d paused", o.paused))
	}
	need := wire.Stat{Label: l.t("要你处理", "Need you"), Value: l.t(fmt.Sprintf("%d 个", len(o.broken)),
		fmt.Sprint(len(o.broken))), Tone: wire.TonePositive, Detail: l.t("都没问题", "none")}
	if n := len(o.broken); n > 0 {
		need.Tone = wire.ToneCaution
		need.Detail = l.t("「", "") + accountTitle(l, o.broken[0]) + l.t("」", "")
		if n > 1 {
			need.Detail += l.t(" 等", " and more")
		}
	} else if n := len(o.exhausted); n > 0 {
		need.Value = l.t(fmt.Sprintf("%d 个", n), fmt.Sprint(n))
		need.Tone = wire.ToneDanger
		need.Detail = l.t("「"+accountTitle(l, o.exhausted[0])+"」额度用完", accountTitle(l, o.exhausted[0])+" out of allowance")
	} else if n := len(o.low); n > 0 {
		need.Detail = l.t(fmt.Sprintf("%d 个额度快用完", n), fmt.Sprintf("%d low on allowance", n))
	}
	usable := o.read + o.send
	assistant := wire.Stat{Label: l.t("Persona 助手能用", "Assistant may use"), Value: l.t(fmt.Sprintf("%d 个", usable),
		fmt.Sprint(usable)), Detail: l.t("还没交给它", "none handed over yet")}
	switch {
	case usable > 0 && o.send > 0:
		assistant.Detail = l.t(fmt.Sprintf("%d 个可代发", o.send), fmt.Sprintf("%d may send", o.send))
	case usable > 0:
		assistant.Detail = l.t("只读", "read only")
	}
	return []wire.Stat{connected, need, assistant}
}

// cardStep is the card's one next step, as a row: the home's next step in words that fit a card, or, when nothing
// needs doing, where the chats are.
func (o overview) cardStep(l lang) wire.Component {
	openHome := &wire.Action{ID: "open", Push: &wire.Push{UI: HomeUI, Title: l.t(homeTitle, homeTitle+" (My AI assistants)")}}
	step, ok := o.nextStep(l, HomeUI)
	switch {
	case !ok:
		return wire.Component{Type: "row", Key: "next", Icon: "check",
			Title: l.t("没有要你处理的事", "Nothing needs you"),
			Subtitle: l.t("对话在「Agents」页。要添加或修改账号，点这里。",
				"The chats are on the Agents page. Tap here to add or change an account."),
			Action: openHome}
	case len(o.accounts) == 0:
		step.Key = "next"
		step.Subtitle = l.t("点这里，在「添加账号」里选你常用的服务，照提示做，一两分钟就好。",
			"Tap here, pick the service you use under Add an account and follow the steps; it takes a minute or two.")
		step.Action = openHome
	case len(o.broken) == 0 && len(o.exhausted) == 0: // none handed to the assistant
		step.Subtitle = l.t("新账号默认不让它碰。点这里打开一个账号，在「助手指挥」里选「只读」或「可代发」。",
			"New accounts are off limits to it. Tap to open one and set Assistant control to Read or Confirm.")
	}
	return step
}

func (a *accounts) account(ctx context.Context, id string) (protocol.Account, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return protocol.Account{}, err
	}
	return space.Account(ctx, id)
}

func (a *accounts) accounts(ctx context.Context) ([]protocol.Account, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return nil, err
	}
	status, err := space.Status(ctx)
	if err != nil {
		return nil, err
	}
	return status.Accounts, nil
}

func (a *accounts) listView(ctx context.Context, r *ext.UIRequest, l lang) *wire.View {
	at := ui(r)
	v := &wire.View{Title: l.t(homeTitle, homeTitle+" (My AI assistants)"), RefreshAfter: 60}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	status, err := space.Status(ctx)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	o := overviewOf(status.Accounts)
	if at == AccountsUI {
		// the settings page's 账号: the management list, every account in full and one row to add another, not a
		// second copy of the home
		v.Title = l.t(accountsTitle, "Accounts")
		about := wire.Section{Items: []wire.Component{{Type: "text", Text: accountsAbout(l)}}}
		v.Sections = []wire.Section{about, pairedSection(r, l, a.clock(r), at, status.Accounts, false)}
		return v
	}
	about := wire.Section{Items: []wire.Component{{Type: "text",
		Text: l.t("这里把你在 Grok、Muse、Claude、Gemini、ChatGPT 等 AI 服务上的账号接进 Persona。接好后，它们的对话会出现在 Persona 的「Agents」页，你在 Persona 里就能看、能直接给它们发消息。你还可以允许 Persona 助手替你读这些对话、替你发消息，每次发之前都会先在手机上问你。",
			"This brings your accounts at AI services such as Grok, Muse, Claude, Gemini and ChatGPT into Persona. Their chats then show on Persona's Agents page, where you read them and write to them. You can also let the Persona assistant read them and write for you, asking you on your phone before each message.")}}}
	state := wire.Section{Title: l.t("现在的状态", "Where things stand"),
		Items: []wire.Component{{Type: "text", Text: o.stateLine(l)}}}
	if step, ok := o.nextStep(l, at); ok {
		state.Items = append(state.Items, wire.Component{Type: "text", Style: "secondary", Text: l.t("下一步", "Next")}, step)
	}
	v.Sections = []wire.Section{about, state}
	if len(status.Accounts) == 0 {
		// before the first account the choices are on the home itself: no tap before seeing them
		chooser := wire.Section{Title: l.t(addTitle, "Add an account"), Items: []wire.Component{{Type: "text", Text: chooseLine(l, providerSet(status.Providers))}}}
		v.Sections = append(append(v.Sections, chooser), a.addSections(r, l, at, status.Providers)...)
		return v
	}
	v.Sections = append(v.Sections, pairedSection(r, l, a.clock(r), at, status.Accounts, true))
	return v
}

// addView is the add-account page: how to choose in one line, then the ways to add one, grouped by how one connects.
func (a *accounts) addView(ctx context.Context, r *ext.UIRequest, l lang) *wire.View {
	v := &wire.View{Title: l.t(addTitle, "Add an account")}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	status, err := space.Status(ctx)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	v.Sections = append([]wire.Section{{Items: []wire.Component{{Type: "text", Text: chooseLine(l, providerSet(status.Providers))}}}},
		a.addSections(r, l, ui(r), status.Providers)...)
	return v
}

// chooseLine says above the ways to add an account which services go which way, so the choice needs no reading of
// each way's explanation (the page each row opens explains it in full). It names them from the lists addSections
// draws its rows from, the relay last and as a choice only for those who run one, and the demo when it is there.
func chooseLine(l lang, available map[string]bool) string {
	var web []string
	for _, s := range browserProviders {
		web = append(web, providerName(l, s.Provider))
	}
	var keys []string
	for _, p := range keyProviders {
		n := providerName(l, p)
		if p == cue.Name {
			n += l.t("（Manus）", " (Manus)")
		}
		keys = append(keys, n)
	}
	relay := providerName(l, muse2api.Name)
	line := l.t("怎么选："+providerName(l, grokBot)+" 在你自己的浏览器里登录；"+strings.Join(web, "、")+" 借用电脑上已登录的网页；"+
		strings.Join(keys, "、")+" 粘贴密钥；"+relay+" 只在你自己搭了 Muse 转接服务时才选",
		"How to choose: "+providerName(l, grokBot)+" signs in in your own browser; "+strings.Join(web, ", ")+
			" use a website signed in on a computer; "+strings.Join(keys, " and ")+" take a pasted key; "+relay+
			" is only for those who run their own Muse relay")
	if available[echo.Name] {
		line += l.t("；想先试试就选"+providerName(l, echo.Name), "; to try it first, pick "+providerName(l, echo.Name))
	}
	return line + l.t("。", ".")
}

// providerSet is which of the providers can be added now.
func providerSet(providers []protocol.Provider) map[string]bool {
	available := map[string]bool{}
	for _, p := range providers {
		available[p.Provider] = p.Available
	}
	return available
}

// accountsAbout is the one line atop 账号, which otherwise reads like the top of the home: what this page is for, and
// that the overview with what needs the user is the home.
func accountsAbout(l lang) string {
	return l.t("这里列出你接入的全部账号：点开一个可以管理它，也可以在下面再添加一个。要看总体情况和哪些账号要你处理，请打开导航里的「"+homeTitle+"」（导航放不下时在「更多」里）。",
		"Every account you connected: open one to manage it, or add another below. For the overview and what needs you, open "+homeTitle+" (My AI assistants) in the navigation (under More when it does not fit).")
}

// pairedSection lists the accounts, each opening its page, and ends with the row to add another. compact is the
// home's list: the accounts that need the user first, each in a few words; otherwise it is 账号's management list,
// in the order the space keeps, each with its last sync and what can be done on its page.
func pairedSection(r *ext.UIRequest, l lang, c clock, at string, accts []protocol.Account, compact bool) wire.Section {
	paired := wire.Section{Title: l.t("已接入的账号", "Your accounts")}
	if compact {
		paired.Title = l.t("账号一览", "Your accounts at a glance")
		accts = slices.Clone(accts)
		slices.SortStableFunc(accts, func(x, y protocol.Account) int { return attention(x) - attention(y) })
	}
	for _, acct := range accts {
		line := accountLine(l, acct, c)
		if compact {
			line = compactLine(l, acct, c)
		}
		paired.Items = append(paired.Items, withAvatar(r, l, &acct, acct.Provider, wire.Component{Type: "row", Key: "acct-" + acct.AccountID, Icon: "person",
			Title: accountTitle(l, acct), Subtitle: line, Badge: statusBadge(l, acct), Action: openAccount(l, at, acct)}))
	}
	switch {
	case len(paired.Items) == 0:
		paired.Items = []wire.Component{{Type: "empty", Icon: "person", Title: l.t("还没有接入任何账号", "No accounts yet"),
			Detail: l.t("点下面的「"+addTitle+"」，选一个你常用的服务。", "Tap Add an account below and pick a service you use.")}}
	case !compact:
		paired.Footer = l.t("点开一个账号可以改名、决定 Persona 助手能不能用它（助手指挥）、设为主助手，或者暂停、移除。",
			"Open an account to rename it, choose what the Persona assistant may do with it (Assistant control), make it the main assistant, pause or remove it.")
	}
	paired.Items = append(paired.Items, addRow(l, at))
	return paired
}

// addRow opens the add-account page.
func addRow(l lang, at string) wire.Component {
	return wire.Component{Type: "row", Key: "add", Icon: "plus", Title: l.t(addTitle, "Add an account"),
		Subtitle: l.t("Grok、Muse、Claude、Gemini、ChatGPT 等，选一个照提示做。", "Grok, Muse, Claude, Gemini, ChatGPT and more: pick one and follow the steps."),
		Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: l.t(addTitle, "Add an account"),
			State: ext.EncodeState(pageState{Page: pageAdd})}}}
}

// attention orders the home's list: accounts needing the user, then exhausted allowance, then recent hiccups, then low
// allowance, then the rest.
func attention(acct protocol.Account) int {
	switch {
	case acct.Status == protocol.AccountError:
		return 0
	case acct.Status == protocol.AccountActive && acct.Quota != nil && acct.Quota.State == protocol.QuotaExhausted:
		return 1
	case acct.LastError != "":
		return 2
	case acct.Status == protocol.AccountActive && acct.Quota != nil && acct.Quota.State == protocol.QuotaLow:
		return 3
	}
	return 4
}

// compactLine is an account's row subtitle on the home: the brand when the name does not carry it, then what is wrong
// in a few words, that it is paused, or its quota summary and what the Persona assistant may do with it.
func compactLine(l lang, acct protocol.Account, c clock) string {
	var parts []string
	if brand := providerName(l, acct.Provider); !strings.Contains(accountTitle(l, acct), brand) {
		parts = append(parts, brand)
	}
	switch {
	case acct.Status == protocol.AccountError || acct.LastError != "":
		short := l.t("出了问题", "something is wrong")
		if p, ok := problemOf(l, acct.LastError, basicPlace(acct)); ok && acct.LastError != "" {
			short = p.Short
		}
		parts = append(parts, short+l.t("，点开看看", "; open it"))
	case acct.Status == protocol.AccountPaused:
		parts = append(parts, l.t("已暂停同步", "paused"))
	default:
		if q := quotaSummaryCompact(l, acct, c); q != "" {
			parts = append(parts, q)
		}
		parts = append(parts, controlWords(l, core.Control(acct.AssistantControl)))
	}
	return strings.Join(parts, " · ")
}

// withAvatar gives a row for an account (acct) or for a way to add one (acct nil, provider) its avatar (avatarOf), so
// services tell apart at a glance. A row's avatar came with UI version 3; an older app is given the icon it drew
// before, since a row has at most one of icon and avatar.
func withAvatar(r *ext.UIRequest, l lang, acct *protocol.Account, provider string, c wire.Component) wire.Component {
	if r.UIVersion > 0 && r.UIVersion < 3 {
		return c
	}
	c.Icon = ""
	c.Avatar = avatarOf(r, l, acct, provider)
	return c
}

// avatarOf is the avatar of an account (acct) or of a way to add one (acct nil, provider), in the order names.Avatar
// keeps: the account's own picture, else its product's official icon (an https url the app loads itself), else the
// service's tile (names.TileRef, served by servePicture) for an app of UI version 6, which draws an avatar's ref. The
// letters (names.Letters) ride along always: what an app draws while a picture loads, when it fails, and, before UI
// version 6, in place of the tile. An own picture a roster gave as image data has no https address: an app of UI
// version 6 gets it by ref (ownRef) when it is a PNG, JPEG, WebP or GIF; otherwise the next candidate stands.
func avatarOf(r *ext.UIRequest, l lang, acct *protocol.Account, provider string) *wire.Avatar {
	own := ""
	if acct != nil {
		provider, own = acct.Provider, acct.AvatarURL
	}
	p := names.Avatar(string(l), provider, own)
	refs := r.UIVersion == 0 || r.UIVersion >= 6
	av := &wire.Avatar{Text: p.Text}
	switch {
	case names.HTTPS(p.Own):
		av.URL = p.Own
	case refs && acct != nil && servable(p.Own):
		av.Ref = ownRef(acct.AccountID)
	case p.Icon != "":
		av.URL = p.Icon
	case refs:
		av.Ref = p.Ref
	}
	return av
}

// accountLine is an account's row subtitle: the brand when the name does not carry it, the state in words and what
// the Persona assistant may do with it.
func accountLine(l lang, acct protocol.Account, c clock) string {
	var parts []string
	if brand := providerName(l, acct.Provider); !strings.Contains(accountTitle(l, acct), brand) {
		parts = append(parts, brand)
	}
	switch {
	case acct.Status == protocol.AccountError:
		parts = append(parts, problemWords(l, acct))
	case acct.Status == protocol.AccountPaused:
		parts = append(parts, l.t("已暂停同步", "paused"))
	case acct.LastError != "":
		parts = append(parts, problemWords(l, acct))
	case acct.LastSyncedAt <= 0:
		parts = append(parts, l.t("还没同步过", "not synced yet"))
	default:
		parts = append(parts, l.t("上次同步：", "synced ")+c.at(l, acct.LastSyncedAt))
	}
	if acct.Status == protocol.AccountActive {
		if q := quotaSummaryFull(l, acct, c); q != "" {
			parts = append(parts, q)
		}
	}
	parts = append(parts, controlWords(l, core.Control(acct.AssistantControl)))
	if acct.AssistantBackend {
		parts = append(parts, l.t("是主助手", "main assistant"))
	}
	return strings.Join(parts, " · ")
}

// controlWords is what the Persona assistant may do with an account, short.
func controlWords(l lang, c core.Control) string {
	switch c {
	case core.ControlRead:
		return l.t("Persona 助手：只读", "assistant: read")
	case core.ControlConfirm:
		return l.t("Persona 助手：可代发（每次先问你）", "assistant: may send, asking first")
	case core.ControlFollow:
		return l.t("Persona 助手：持续跟进（按对话租约）", "assistant: follow-up by lease")
	case core.ControlAuto:
		return l.t("Persona 助手：直接放行（自动护栏保护）", "assistant: auto with safety guards")
	}
	return l.t("Persona 助手：不能用", "assistant: off")
}

// browserConnection says whether a provider works through a computer's browser.
func browserConnection(p string) bool {
	_, ok := browserProviderLabel(p)
	return ok
}

// problemWords is what is wrong with an account and what to do, from its last error (problems.go says it), without
// looking its computer up (the account page names it).
func problemWords(l lang, acct protocol.Account) string {
	if acct.LastError == "" {
		if acct.Status == protocol.AccountError {
			return l.t("出了问题：点开它，点「重新检查」。", "Something is wrong: open it and check again.")
		}
		return ""
	}
	p, _ := problemOf(l, acct.LastError, basicPlace(acct))
	if p.NeedsUser {
		// The fix names the computer, which the account page looks up: the row only says where to find it.
		return p.Short + l.t("：点开它看怎么处理。", ": open it to see what to do.")
	}
	return p.Short + l.t("：", ": ") + p.Do
}

// addSections are the ways to add an account, grouped by how one connects, each row saying in one line what it takes
// and, when it cannot be added in this configuration, why and what to switch on.
func (a *accounts) addSections(r *ext.UIRequest, l lang, at string, providers []protocol.Provider) []wire.Section {
	available := providerSet(providers)
	row := func(p, subtitle string, st pageState) wire.Component {
		return withAvatar(r, l, nil, p, wire.Component{Type: "row", Key: "pair-" + p, Icon: "plus", Title: providerName(l, p), Subtitle: subtitle,
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: l.t("添加 ", "Add ") + providerName(l, p), State: ext.EncodeState(st)}}})
	}
	off := func(c wire.Component, why string) wire.Component {
		c.Subtitle = why
		c.Badge = &wire.Badge{Text: l.t("还没打开", "Not switched on"), Tone: wire.ToneCaution}
		return c
	}
	signIn := wire.Section{Title: l.t("在你自己的浏览器里登录", "Sign in in your own browser"),
		Items: []wire.Component{row(grokBot, l.t("在你自己的浏览器里点「同意」就行。", "Approve it in your own browser."), pageState{Page: pageGrok})}}
	if !available[grokBot] {
		signIn.Items[0] = off(signIn.Items[0], l.t("这个版本没有带上 Grok Bot。", "This version has no Grok Bot."))
	}

	// One line each: the page a row opens explains the way in full (browserPairView, pairView).
	web := wire.Section{Title: l.t("借用电脑上已登录的网页", "Use a website signed in on a computer"),
		Footer: l.t("要一台连着 Persona、开着的电脑，在它上面登录好这些网站；密码不会离开那台电脑。",
			"Needs a computer that is on and connected to Persona, signed in to these sites; passwords stay on it.")}
	for _, s := range browserProviders {
		c := row(s.Provider, l.t("用那台电脑上登录好的 ", "Through ")+siteHost(s.AppURL)+l.t("。", " signed in on that computer."),
			pageState{Page: pageBrowserPair, Provider: s.Provider})
		if !available[s.Provider] {
			c = off(c, l.t("要先在 ", "Switch on ")+settingsPath(l)+l.t(" 里打开「"+settingBrowser+"」。", " → \""+settingBrowser+"\"."))
		}
		web.Items = append(web.Items, c)
	}

	keys := wire.Section{Title: l.t("粘贴密钥", "Paste a key"),
		Footer: l.t("密钥是对方网站给你的一串字符，只在添加时提交一次，加密保存。",
			"A key is a string the service gives you; it is sent once when adding and kept encrypted.")}
	for _, p := range keyProviders {
		c := row(p, l.t("到 ", "Copy a key from ")+providerName(l, p)+l.t(" 网站复制一串密钥粘贴进来。", " and paste it."), pageState{Page: pagePair, Provider: p})
		if p == cue.Name {
			// "Cue" alone does not say which service it is: the agents of a Manus account (provider/cue).
			c.Subtitle = l.t("你在 Manus 上的智能体：到 Manus 网站复制你的 API Key 粘贴进来。", "Your agents on Manus: copy your API key from Manus and paste it.")
		}
		if !available[p] {
			c = off(c, l.t("这个版本没有带上它。", "This version does not include it."))
		}
		keys.Items = append(keys.Items, c)
	}
	// The relay is not the Muse app and looks like it by name: its line says so first, and where an ordinary Muse
	// account goes instead.
	notMuse := l.t("不是 Muse 官方的：你自己搭了 Muse 转接服务才用，", "Not Muse's own: only if you run your own Muse relay, ")
	ordinary := l.t("普通 Muse 账号选上面的「", " An ordinary Muse account goes under ") + providerName(l, providerMuse) + l.t("」。", " above.")
	m := row(muse2api.Name, notMuse+l.t("粘贴它的密钥。", "paste its key.")+ordinary, pageState{Page: pagePair, Provider: muse2api.Name})
	if !available[muse2api.Name] {
		// Not missing from the build: it waits for its server's address in the extension's settings.
		m = off(m, notMuse+l.t("先在 ", "first write its address in ")+settingsPath(l)+
			l.t(" 里填「"+settingMuse2API+"」。", " → \""+settingMuse2API+"\".")+ordinary)
	}
	keys.Items = append(keys.Items, m)
	sections := []wire.Section{signIn, web, keys}
	if available[echo.Name] {
		sections = append(sections, wire.Section{Title: l.t("先试试", "Try it first"), Items: []wire.Component{
			row(echo.Name, l.t("不用真账号：你发什么它就回什么，不连任何网站。", "No real account: it answers with what you send and reaches nothing."),
				pageState{Page: pagePair, Provider: echo.Name})}})
	}
	return sections
}

// siteHost is a site's address as people write it ("claude.ai").
func siteHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}

func (a *accounts) accountView(ctx context.Context, r *ext.UIRequest, l lang, st *pageState) *wire.View {
	at := ui(r)
	v := &wire.View{RefreshAfter: 60}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	acct, err := space.Account(ctx, st.Account)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "empty", Icon: "warning",
			Title: l.t("这个账号已不在", "This account is gone"), Detail: reason(l, err, "")}}}}
		return v
	}
	v.Title = accountTitle(l, acct)
	control, err := space.AssistantControl(ctx, acct.AccountID)
	if err != nil || control == "" {
		// A level this version does not know (written by a newer one) would be a segmented value none of its choices
		// has, and the hub refuses the page (wire.CheckView): it shows as off, the most careful level.
		control = core.ControlOff
	}

	// What it is and how it is doing, with the fix when it needs one. On the home the head carries the account's
	// avatar (avatarOf; a header came with UI version 4; an older app gets the 服务 row it had). Opened from the
	// settings page's 账号 it is always the 服务 row, with the avatar on it, as 0.6.3 drew it: the web app, drawing
	// this page inside its Settings screen, shifted the whole screen up with a header atop a pushed page (a host bug;
	// TestAccountPageHeadPerEntry pins this).
	info := wire.Section{Items: []wire.Component{
		withAvatar(r, l, &acct, acct.Provider, wire.Component{Type: "row", Title: l.t("服务", "Service"), Value: providerName(l, acct.Provider)}),
	}}
	if at == HomeUI && (r.UIVersion == 0 || r.UIVersion >= 4) {
		info.Items[0] = wire.Component{Type: "header", Key: "head", Title: accountTitle(l, acct),
			Subtitle: providerName(l, acct.Provider), Avatar: avatarOf(r, l, &acct, acct.Provider)}
	}
	if acct.Email != "" {
		// The name above shows only the part before @.
		info.Items = append(info.Items, wire.Component{Type: "row", Title: l.t("邮箱", "Email"), Value: acct.Email})
	}
	info.Items = append(info.Items, []wire.Component{
		{Type: "row", Title: l.t("现在", "Now"), Value: stateWords(l, acct), Badge: statusBadge(l, acct)},
		{Type: "row", Title: l.t("上次同步", "Last synced"), Value: a.clock(r).lastSynced(l, acct.LastSyncedAt)},
		{Type: "row", Title: l.t("对话", "Chats"), Value: fmt.Sprint(acct.SessionCount)},
	}...)
	if words := problemWords(l, acct); words != "" {
		if p, ok := problemOf(l, acct.LastError, placerFor(ctx, space, a.browser).of(ctx, acct, acct.LastError)); ok {
			words = p.Text(l)
		}
		info.Items = append(info.Items, wire.Component{Type: "text", Text: words},
			wire.Component{Type: "button", Title: l.t("重新检查", "Check again"), Action: &wire.Action{ID: actRecheck}})
	}
	// 重新登录 (only Grok Bot signs in again from here: club.logins.grok_bot.*) leads the page only while the account
	// needs it; otherwise it waits under 同步, saying when it is for.
	signInAgain, troubled := wire.Component{}, acct.Status == protocol.AccountError || acct.LastError != ""
	if acct.Provider == grokBot {
		signInAgain = wire.Component{Type: "button", Title: l.t("重新登录", "Sign in again"),
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: providerName(l, grokBot),
				State: ext.EncodeState(pageState{Page: pageGrok, Account: acct.AccountID})}}}
		if troubled {
			info.Items = append(info.Items, signInAgain)
		}
	}

	// Only a name the owner chose is prefilled: a composed title ("Grok Bot (2)", a brand in another language) is
	// the placeholder, so 「改名」 pressed unchanged does not freeze it as theirs. The field's label names the block,
	// which has no title of its own to say 名字 twice.
	name := acct.OwnerName
	rename := wire.Section{
		Footer: l.t("给它起个好认的名字，比如「工作用 Claude」。只在 Persona 里显示，不会改对方网站上的名字。",
			"A name you will recognize, like \"Work Claude\". Shown only in Persona; the service's own name is not changed."),
		Items: []wire.Component{
			{Type: "field", Name: fieldName, Label: l.t("名字", "Name"), Kind: wire.FieldKindText, Value: name,
				Placeholder: accountTitle(l, acct)},
			{Type: "button", Title: l.t("改名", "Rename"), Action: &wire.Action{ID: actRename, Submit: true}},
		}}

	sections := []wire.Section{
		info,
		rename,
		assistantSection(l, acct, control, st.ConfirmAuto),
		a.followLeaseSection(ctx, space, l, acct, a.clock(r)),
		a.inboxSection(l, acct),
		a.mainAssistantSection(l, acct),
	}

	pause := wire.Component{Type: "button", Title: l.t("暂停同步", "Pause"), Action: &wire.Action{ID: actPause,
		Confirm: l.t("暂停后不再拉取它的新对话，也不能给它发消息，随时可以恢复。", "While paused, no new chats are fetched and nothing can be sent; resume any time.")}}
	if acct.Status == protocol.AccountPaused {
		pause = wire.Component{Type: "button", Title: l.t("恢复同步", "Resume"), Action: &wire.Action{ID: actResume}}
	}
	syncing := wire.Section{Title: l.t("同步", "Syncing"),
		Footer: l.t("平时每隔几分钟会自动拉取它的新对话。", "New chats are fetched on their own every few minutes."),
		Items: []wire.Component{
			{Type: "button", Title: l.t("立即同步", "Sync now"), Action: &wire.Action{ID: actSync}},
			pause,
		}}
	if signInAgain.Type != "" && !troubled {
		syncing.Items = append(syncing.Items, signInAgain)
		syncing.Footer += l.t("「重新登录」只在 Grok 的登录失效时才需要（这页顶上会提示），重新同意一次就好，以前的对话都还在。",
			" Sign in again is only needed once Grok's sign-in stops working (the top of this page says so); the chats stay.")
	}
	sections = append(sections, syncing)
	sections = append(sections, quotaDisplaySection(l, acct, a.clock(r)))
	sections = append(sections, a.terminalEntrySection(ctx, l, at, acct))
	if space.MountsRepos(ctx, acct.AccountID) {
		sections = append(sections, reposEntry(l, at, acct))
	}
	if takeoverSupported(acct.Provider) && a.hub != nil {
		sec, pending := a.machineSection(ctx, l, acct)
		sections = append(sections, sec)
		if pending {
			v.RefreshAfter = 10
		}
	}
	sections = append(sections, wire.Section{Title: l.t("移除", "Remove"), Items: []wire.Component{
		{Type: "button", Title: l.t("移除这个账号（保留聊天记录）", "Remove (keep the chats)"), Role: "destructive", Action: &wire.Action{ID: actRemove,
			Confirm: l.t("移除后，保存的密钥或登录会被删除，以前的对话和消息还留着。", "Removing deletes the saved key or sign-in and keeps the chats and messages.")}},
		{Type: "button", Title: l.t("移除并删除所有聊天记录", "Remove and delete the chats"), Role: "destructive", Action: &wire.Action{ID: actPurge,
			Confirm: l.t("密钥、对话和消息都会被删除，不能恢复。", "The key, chats and messages are all deleted, for good.")}},
	}})
	v.Sections = sections
	return v
}

// stateWords is an account's state in a few words.
func stateWords(l lang, acct protocol.Account) string {
	switch {
	case acct.Status == protocol.AccountError:
		return l.t("要你处理", "Needs you")
	case acct.Status == protocol.AccountPaused:
		return l.t("已暂停同步", "Paused")
	case acct.LastError != "":
		return l.t("能用，最近有点小问题", "Working, with a recent hiccup")
	}
	return l.t("正常，在自动同步", "Working, syncing on its own")
}

// assistantSection is 助手指挥: how far the Persona assistant may use the account. The name is the one the assistant's
// guide (tools.go) sends the user to. Switching to ControlAuto requires an explicit confirmation step first.
func assistantSection(l lang, acct protocol.Account, control core.Control, confirmAuto bool) wire.Section {
	items := []wire.Component{{Type: "segmented", Name: fieldControl, Choices: controlChoices(l, control), Value: string(control),
		Action: &wire.Action{ID: actControl}}}
	if confirmAuto && control != core.ControlAuto {
		items = append(items, autoConfirmItems(l)...)
	}
	return wire.Section{Title: l.t("助手指挥：Persona 助手能不能用这个账号", "Assistant control: may the Persona assistant use this account"),
		Footer: l.t("关闭：Persona 助手完全看不到它（新账号默认这样）。只读：Persona 助手能读它的对话，帮你总结、提醒。可代发：还能替你给它发消息，每次发之前都先在手机上问你。持续跟进：按下面开启的对话租约，在指定对话、限定轮数和有效期内连续跟进，其余写操作仍先在手机上问你。直接放行：发消息不再逐次询问（需先二次确认，受额度、未确认发送、突发频率限制及仓库强制确认保护）。",
			"Off: the Persona assistant does not see it (new accounts start so). Read: it can read the chats to sum up and remind you. Confirm: it can also write for you, asking on your phone before each message. Follow: it continues turns in chats that have an active follow-up lease below, and asks first elsewhere. Auto: it writes without asking each time (requires a second confirmation and is guarded by allowance, unconfirmed-send, burst-limit and repository confirmation rules)."),
		Items: items}
}

// autoConfirmItems renders the explicit secondary confirmation card and buttons before saving ControlAuto.
func autoConfirmItems(l lang) []wire.Component {
	return []wire.Component{
		{
			Type:  "row",
			Key:   "confirm-auto-card",
			Icon:  "warning",
			Title: l.t("确认开启「直接放行」？", "Turn on Auto (direct bypass)?"),
			Subtitle: l.t("风险与安全护栏说明：开启后，Persona 助手在这个账号里发消息、新开对话或打断时不再每次先在手机上问你。以下护栏全程生效：额度用完或对方限流时自动拦截、上一条消息未确认送达时暂停连发、单对话高频突发自动限流，且挂载或授权仓库仍每次强制手机确认，账号配对与权限管理仅限你自己操作。",
				"Risk & safety guards: once on, the Persona assistant sends, starts chats and interrupts on this account without asking each time. Guards stay active: sends pause when out of allowance or when a previous send is unconfirmed, per-chat burst limits prevent loops, repository mounts always require phone confirmation, and account management stays owner-only."),
			Badge: &wire.Badge{Text: l.t("需二次确认", "Confirm required"), Tone: wire.ToneCaution},
		},
		{
			Type:  "button",
			Key:   "confirm-auto-yes",
			Title: l.t("确认开启直接放行", "Confirm turning on Auto"),
			Role:  "destructive",
			Action: &wire.Action{
				ID: actControlAutoConfirm,
				Confirm: l.t("确认将这个账号设为「直接放行」？开启后助手发消息不再逐次询问，仍受额度、未确认发送、突发频率限制和仓库强制确认保护。",
					"Confirm setting this account to Auto? The assistant will send without asking each time, still guarded by allowance, unconfirmed-send, burst-limit and repository confirmation rules."),
			},
		},
		{
			Type:   "button",
			Key:    "confirm-auto-no",
			Title:  l.t("取消，保持当前档位", "Cancel and keep current level"),
			Action: &wire.Action{ID: actControlAutoCancel},
		},
	}
}

func sessionDisplayTitle(l lang, prov, title string) string {
	title = strings.TrimSpace(title)
	if prov == echo.Name && strings.EqualFold(title, "Echo") {
		return l.t("演示对话", "Echo")
	}
	if title == "" {
		return l.t("默认对话", "Main chat")
	}
	return title
}

func ttlWords(l lang, ttl string) string {
	switch strings.TrimSpace(ttl) {
	case core.FollowTTL6h:
		return l.t("6 小时", "6 hours")
	case core.FollowTTL24h:
		return l.t("24 小时", "24 hours")
	default:
		return l.t("2 小时", "2 hours")
	}
}

// followLeaseSection renders the FollowLease management section on the account page: active leases with remaining turns,
// expiration time, background switch and revoke actions, plus the form to create a new lease for a chat.
func (a *accounts) followLeaseSection(ctx context.Context, space *core.Space, l lang, acct protocol.Account, c clock) wire.Section {
	var items []wire.Component
	activeAssistantKeys, _ := space.AssistantSessionKeys(ctx)
	leasesRes, leaseErr := space.ListFollowLeases(ctx, protocol.ListFollowLeasesParams{AccountID: acct.AccountID, ActiveOnly: true})
	if leaseErr != nil {
		items = append(items, wire.Component{
			Type:  "text",
			Style: "secondary",
			Text:  reason(l, leaseErr, ""),
		})
	} else if len(leasesRes.Leases) == 0 {
		items = append(items, wire.Component{
			Type:  "text",
			Style: "secondary",
			Text:  l.t("现在没有生效中的跟进租约。", "No active follow-up leases right now."),
		})
	} else {
		for _, fl := range leasesRes.Leases {
			if activeAssistantKeys[fl.SessionKey] {
				continue
			}
			chatTitle := sessionDisplayTitle(l, acct.Provider, fl.SessionTitle)
			bgWords := l.t("仅在线跟进", "attended only")
			if fl.AllowBackground {
				bgWords = l.t("允许后台跟进（同受轮数与时长封顶）", "background allowed (same turn & time caps)")
			}
			sub := l.t(fmt.Sprintf("剩余 %d / %d 轮（已用 %d 轮） · %s到期 · %s",
				fl.RemainingTurns, fl.MaxTurns, fl.UsedTurns, c.at(l, fl.ExpiresAt), bgWords),
				fmt.Sprintf("%d / %d turns left (%d used) · expires %s · %s",
					fl.RemainingTurns, fl.MaxTurns, fl.UsedTurns, c.at(l, fl.ExpiresAt), bgWords))
			if fl.Note != "" {
				sub += " · " + fl.Note
			}
			items = append(items,
				wire.Component{
					Type:     "row",
					Key:      "lease-" + fl.LeaseID,
					Icon:     "clock",
					Title:    chatTitle,
					Subtitle: sub,
					Badge: &wire.Badge{
						Text: l.t(fmt.Sprintf("剩 %d 轮", fl.RemainingTurns), fmt.Sprintf("%d left", fl.RemainingTurns)),
						Tone: wire.TonePositive,
					},
				},
				wire.Component{
					Type:  "button",
					Key:   "revoke-" + fl.LeaseID,
					Title: l.t("撤销「"+chatTitle+"」的跟进租约", "Revoke lease for \""+chatTitle+"\""),
					Role:  "destructive",
					Action: &wire.Action{
						ID:     actLeaseRevoke,
						Values: map[string]string{valLeaseID: fl.LeaseID},
						Confirm: l.t("撤销后，Persona 助手在这个对话里发消息会恢复为每次先在手机上问你。确认撤销吗？",
							"Once revoked, the Persona assistant will ask on your phone before each message in this chat again. Revoke?"),
					},
				},
			)
		}
		if len(leasesRes.Leases) > 1 {
			items = append(items, wire.Component{
				Type:  "button",
				Key:   "revoke-all-leases",
				Title: l.t("撤销全部跟进租约", "Revoke all follow-up leases"),
				Role:  "destructive",
				Action: &wire.Action{
					ID: actLeaseRevokeAll,
					Confirm: l.t("撤销这个账号下的全部跟进租约？",
						"Revoke all follow-up leases for this account?"),
				},
			})
		}
	}

	sessions, _ := space.Store().AccountSessions(ctx, acct.AccountID)
	var sessionChoices []wire.Choice
	for _, sess := range sessions {
		if !sess.CanSend || sess.IsHidden || activeAssistantKeys[sess.SessionKey] {
			continue
		}
		choiceVal := sess.ExternalID
		if choiceVal == "" {
			choiceVal = sess.SessionKey
		}
		sessionChoices = append(sessionChoices, wire.Choice{
			Value: choiceVal,
			Title: sessionDisplayTitle(l, acct.Provider, sess.Title),
		})
	}
	if len(sessionChoices) > 0 {
		items = append(items,
			wire.Component{
				Type:    "field",
				Name:    fieldLeaseSession,
				Label:   l.t("要跟进的对话", "Chat to follow"),
				Kind:    wire.FieldKindChoice,
				Choices: sessionChoices,
				Value:   sessionChoices[0].Value,
			},
			wire.Component{
				Type:  "field",
				Name:  fieldLeaseTTL,
				Label: l.t("有效时长", "Duration"),
				Kind:  wire.FieldKindChoice,
				Choices: []wire.Choice{
					{Value: core.FollowTTL2h, Title: l.t("2 小时", "2 hours")},
					{Value: core.FollowTTL6h, Title: l.t("6 小时", "6 hours")},
					{Value: core.FollowTTL24h, Title: l.t("24 小时", "24 hours")},
				},
				Value: core.FollowTTL2h,
			},
			wire.Component{
				Type:  "field",
				Name:  fieldLeaseTurns,
				Label: l.t("最多跟进轮数", "Max turns"),
				Kind:  wire.FieldKindChoice,
				Choices: []wire.Choice{
					{Value: "10", Title: l.t("10 轮", "10 turns")},
					{Value: "20", Title: l.t("20 轮", "20 turns")},
					{Value: "50", Title: l.t("50 轮", "50 turns")},
				},
				Value: "10",
			},
			wire.Component{
				Type:  "field",
				Name:  fieldLeaseBackground,
				Label: l.t("后台跟进", "Background follow-up"),
				Kind:  wire.FieldKindChoice,
				Choices: []wire.Choice{
					{Value: "off", Title: l.t("关闭（只在线跟进）", "Off (attended only)")},
					{Value: "on", Title: l.t("开启（同受轮数和时长封顶）", "On (same turn & time caps)")},
				},
				Value: "off",
			},
			wire.Component{
				Type:        "field",
				Name:        fieldLeaseNote,
				Label:       l.t("备注（可不填）", "Note (optional)"),
				Kind:        wire.FieldKindText,
				Placeholder: l.t("例如：跟进测试进展", "e.g. Follow up on test run"),
			},
			wire.Component{
				Type:   "button",
				Title:  l.t("新建跟进租约", "Create follow-up lease"),
				Action: &wire.Action{ID: actLeaseGrant, Submit: true},
			},
		)
	} else {
		items = append(items, wire.Component{
			Type:  "text",
			Style: "secondary",
			Text:  l.t("这个账号目前没有可发消息的对话；同步出可发消息的对话后，就可以在这里为指定对话新建跟进租约。", "This account has no writable chats yet; sync a writable chat first to create a follow-up lease here."),
		})
	}
	return wire.Section{
		Title: l.t("持续跟进租约：按对话授权助手跟进", "Follow-up leases: authorize the assistant per chat"),
		Footer: l.t("在「助手指挥」设为「持续跟进」时生效：拿到租约的那个对话在有效时长和轮数上限内可连续跟进（可选开启后台跟进，同样受轮数和时长双重封顶）；超限、到期或没设租约的对话自动退回逐次手机确认。把助手指挥降为关闭、只读或逐次确认，或移除账号时，所有租约会自动撤销。",
			"Active when Assistant control is set to Follow: the chosen chat may continue turns within the duration and turn cap (and in the background when switched on, under the same caps); expired, exhausted or other chats fall back to per-message confirmation. Stepping down to Off, Read or Confirm, or removing the account, revokes all leases automatically."),
		Items: items,
	}
}

// inboxSection is 对话进收件箱: whether this account's chats are rows in Persona's Inbox. The extension's own switch
// decides whether any are; while it is off the section only says where to turn it on.
func (a *accounts) inboxSection(l lang, acct protocol.Account) wire.Section {
	title := l.t("收件箱：这个账号的对话要不要出现在 Persona 收件箱", "Inbox: should this account's chats appear in Persona's Inbox")
	if a.inboxCfg == nil || !a.inboxCfg().On {
		return wire.Section{Title: title, Items: []wire.Component{{Type: "text", Text: l.t("现在是关着的：先到 ", "Off now: first switch on ") + settingsPath(l) +
			l.t(" 里打开「"+settingInbox+"」。", " → \""+settingInbox+"\".")}}}
	}
	val := "on"
	if acct.InboxOff {
		val = "off"
	}
	return wire.Section{Title: title,
		Footer: l.t("打开后，这个账号的每个对话在收件箱里占一行，有新回复就冒出来，内容会交给 Persona 的「消息分拣」模型排序。",
			"On: each chat of this account is a row in the Inbox, new replies show up, and Persona's message-triage model sorts them."),
		Items: []wire.Component{{Type: "segmented", Name: fieldInbox, Value: val, Choices: []wire.Choice{
			{Value: "on", Title: l.t("进收件箱", "In the Inbox")}, {Value: "off", Title: l.t("不进", "Not in Inbox")}},
			Action: &wire.Action{ID: actInbox}}}}
}

// controlChoices are the 5 助手指挥 levels to choose from: off, read, confirm, follow and auto.
func controlChoices(l lang, _ core.Control) []wire.Choice {
	return []wire.Choice{
		{Value: string(core.ControlOff), Title: l.t("关闭", "Off")},
		{Value: string(core.ControlRead), Title: l.t("只读", "Read")},
		{Value: string(core.ControlConfirm), Title: l.t("可代发（逐次确认）", "Confirm")},
		{Value: string(core.ControlFollow), Title: l.t("持续跟进", "Follow")},
		{Value: string(core.ControlAuto), Title: l.t("直接放行", "Auto")},
	}
}

// controlChoiceTitle is a level's name as the choices have it.
func controlChoiceTitle(l lang, control core.Control) string {
	for _, c := range controlChoices(l, control) {
		if c.Value == string(control) {
			return c.Title
		}
	}
	return l.t("关闭", "Off")
}

// mainAssistantSection is 主助手: the account itself answering as the main assistant. With the bridge switched off in
// the settings the switch would do nothing, so the page says what to switch on instead (and still lets an account
// that has it on turn it off).
func (a *accounts) mainAssistantSection(l lang, acct protocol.Account) wire.Section {
	sec := wire.Section{Title: l.t("主助手：让它直接当你的助手", "Main assistant: let it be your assistant"),
		Footer: l.t("和上面的「助手指挥」不一样：助手指挥是让 Persona 助手去用这个账号；主助手是反过来，让这个账号自己来当你的助手——开启后，在 Persona 对话里可以选它来聊天、督办任务，由它来回答你。",
			"Unlike Assistant control above (the Persona assistant using this account), this makes the account itself your assistant: once on, you can pick it in Persona's chats and it answers you and supervises tasks.")}
	if !a.runtimeEnabled() {
		sec.Items = []wire.Component{{Type: "text", Text: l.t("现在用不了：先到 ", "Not available now: first switch on ") + settingsPath(l) +
			l.t(" 里打开「"+settingRuntime+"」。", " → \""+settingRuntime+"\".")}}
		if !acct.AssistantBackend {
			return sec
		}
	}
	val := "off"
	if acct.AssistantBackend {
		val = "on"
	}
	sec.Items = append(sec.Items, wire.Component{Type: "segmented", Name: fieldAssistantBackend,
		Choices: []wire.Choice{{Value: "off", Title: l.t("关闭", "Off")}, {Value: "on", Title: l.t("开启", "On")}},
		Value:   val, Action: &wire.Action{ID: actAssistantBackend}})
	return sec
}

// quotaWords names an allowance the way people say it: "每天的次数 62%".
func quotaWords(l lang, kind, window string, pct *int) string {
	var per string
	switch window {
	case "minute":
		per = l.t("每分钟的", "per minute ")
	case "hour":
		per = l.t("每小时的", "per hour ")
	case "3h":
		per = l.t("每 3 小时的", "per 3h ")
	case "5h":
		per = l.t("每 5 小时的", "per 5h ")
	case "day":
		per = l.t("每天的", "per day ")
	case "week":
		per = l.t("每周的", "per week ")
	case "month":
		per = l.t("每月的", "per month ")
	case "":
	default:
		per = l.t("每 "+window+" 的", "per "+window+" ")
	}
	var what string
	switch kind {
	case protocol.QuotaKindRequests:
		what = l.t("次数", "requests")
	case protocol.QuotaKindTokens:
		what = l.t("用量（字数）", "usage (tokens)")
	case protocol.QuotaKindCredits:
		what = l.t("点数", "credits")
	case protocol.QuotaKindWindow:
		what = l.t("时段额度", "window")
	default:
		what = l.t("额度", "allowance")
	}
	s := per + what
	if pct != nil {
		s += fmt.Sprintf(" %d%%", *pct)
	}
	return s
}

func quotaBadgeTone(tone string) string {
	switch tone {
	case protocol.QuotaToneExhausted:
		return wire.ToneDanger
	case protocol.QuotaToneLow:
		return wire.ToneCaution
	case protocol.QuotaToneOK:
		return wire.TonePositive
	default:
		return wire.ToneNeutral
	}
}

// unavailableView is a pairing page for a provider that cannot be added in this configuration: why, and what to
// switch on.
func unavailableView(l lang, title, why string) *wire.View {
	return &wire.View{Title: title, Sections: []wire.Section{{Items: []wire.Component{
		{Type: "empty", Icon: "settings", Title: l.t("现在还加不了", "Cannot be added yet"), Detail: why},
	}}}}
}

func (a *accounts) available(p string) bool {
	_, ok := a.club.Registry().Get(p)
	return ok
}

func (a *accounts) pairView(l lang, st pageState) *wire.View {
	name := providerName(l, st.Provider)
	title := l.t("添加 ", "Add ") + name
	if !a.available(st.Provider) {
		why := l.t("这个版本没有带上 ", "This version does not include ") + name + l.t("。", ".")
		if st.Provider == muse2api.Name {
			why = l.t("这一项只给自己搭了 Muse 转接服务的人用。先到 ", "This is only for your own Muse relay. First write its address in ") +
				settingsPath(l) + l.t(" 里填「"+settingMuse2API+"」，保存后再回来。普通的 Muse 账号请用「借用电脑上已登录的网页」那一组里的 ",
				" → \""+settingMuse2API+"\" and save. For an ordinary Muse account use ") + providerName(l, providerMuse) +
				l.t("。", " instead.")
		}
		return unavailableView(l, title, why)
	}
	keyLabel := l.t("密钥（API Key）", "Key (API key)")
	keyHelp := l.t("到 ", "Copy the API key from ") + name + l.t(" 网站的设置里找到 API Key，复制整串粘贴到这里。", "'s settings and paste all of it.")
	sendsTo := name
	var intro []wire.Component
	switch st.Provider {
	case cue.Name:
		intro = []wire.Component{{Type: "text", Text: cueWhat(l)}}
		keyHelp = l.t("你 Manus 账号的 API Key：到 Manus 网站复制，整串粘贴到这里。", "Your Manus account's API key: copy it from Manus and paste all of it.")
		sendsTo = l.t("Manus（api.manus.ai）", "Manus (api.manus.ai)")
	case muse2api.Name:
		keyHelp = l.t("你自己搭的 Muse 转接服务启动时设的那串密钥。", "The key your own Muse relay was started with.")
	case echo.Name:
		keyLabel = l.t("随便填一串字", "Any text")
		keyHelp = l.t("演示账号不连任何网站：填一串以 echo- 开头的字就行，比如 echo-test-001。", "The demo reaches nothing: any text starting with echo-, like echo-test-001.")
	}
	footer := l.t("密钥（API Key）是对方网站给你的一串字符，凭它就能代表你使用服务。它只在添加时提交一次，加密保存在你的 Persona 上，只会发给 ",
		"A key (API key) is a string the service gives you that acts on your behalf. It is sent once, kept encrypted on your Persona and sent only to ") + sendsTo +
		l.t("，之后不会再显示出来。", "; it is never shown again.")
	if st.Provider == echo.Name {
		footer = l.t("演示账号不会把任何东西发到外面。", "The demo account sends nothing anywhere.")
	}
	return &wire.View{Title: title, Sections: []wire.Section{{
		Footer: footer,
		Items: append(intro, []wire.Component{
			{Type: "field", Name: fieldCredential, Label: keyLabel, Kind: wire.FieldKindSecret, Detail: keyHelp},
			{Type: "field", Name: fieldName, Label: l.t("名字（可不填）", "Name (optional)"), Kind: wire.FieldKindText,
				Detail: l.t("给它起个好认的名字，比如「工作用 ", "A name you will recognize, like \"Work ") + name + l.t("」，以后也能改。", "\"; you can change it later.")},
			{Type: "button", Title: l.t("添加", "Add"), Action: &wire.Action{ID: actPair, Submit: true}},
		}...)}}}
}

// cueWhat says on Cue's pairing page which service it is, for someone who has not heard the name: the agents of a
// Manus account, reached through the official Manus API (provider/cue).
func cueWhat(l lang) string {
	return l.t("Cue 就是你在 Manus 上的智能体，这里通过 Manus 的官方接口收发消息。", "Cue means your agents on Manus, reached through Manus's official API.")
}

// grokView is the Grok Bot sign-in (club.logins.grok_bot.*): start gives a login id and the https page the user
// approves on, each render polls it (pending, or done with the account). A sign-in lasts 10 minutes and at most two
// wait at once; one that expired, was canceled or failed is not found any more. With st.Account it signs that
// account in again (its credential is replaced) instead of pairing a new one.
func (a *accounts) grokView(ctx context.Context, l lang, st *pageState) *wire.View {
	v := &wire.View{Title: providerName(l, grokBot)}
	if st.Login == "" && st.Account == "" && !a.available(grokBot) {
		return unavailableView(l, l.t("添加 ", "Add ")+providerName(l, grokBot), l.t("这个版本没有带上 Grok Bot。", "This version has no Grok Bot."))
	}
	start := wire.Component{Type: "button", Title: l.t("开始登录", "Start signing in"), Action: &wire.Action{ID: actGrokStart}}
	again := st.Account
	if st.Login == "" {
		intro := l.t("在你自己的浏览器里登录 Grok Bot：点「开始登录」会打开 Grok 的授权页，在那里点同意，这里会自动看到结果。",
			"You sign in in your own browser: start, open the page and approve; this page checks for the result.")
		if again != "" {
			intro = l.t("重新登录这个 Grok Bot 账号：同意后换成新的登录，以前的对话和消息都还在。",
				"Signing this Grok Bot account in again: once you approve, its credential is replaced; its sessions and messages stay.")
		}
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: intro}, start}}}
		return v
	}
	var res protocol.GrokBotLoginPoll
	if err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotPoll, protocol.LoginParams{LoginID: st.Login}, &res); err != nil {
		why := protocol.ReasonOf(err)
		switch {
		case why == protocol.ReasonLoginNotFound:
			*st = pageState{Page: pageGrok, Account: again, NoteTone: wire.ToneCaution,
				Note: l.t("这次登录已过期（10 分钟）或已取消，请重新开始。", "This sign-in expired (after 10 minutes) or was canceled; start again.")}
		case slices.Contains(retryableLogin, why):
			st.Note, st.NoteTone = l.t("暂时查不到登录结果，稍后会自动再查。", "Could not check the sign-in just now; it is checked again shortly."), wire.ToneCaution
			v.Sections = []wire.Section{{Items: a.grokPending(l, *st)}}
			v.RefreshAfter = 30
			return v
		default:
			words := reasonWords(l, why)
			if words == "" && protocol.CodeOf(err) == protocol.CodeInvalid {
				words = l.t("Grok 没有同意这次登录（可能点了拒绝，或者这个 Grok 账号用不了），请重新开始。",
					"Grok did not approve this sign-in (refused, or the account cannot be used); start again.")
			}
			if words == "" {
				words = reason(l, err, "")
			}
			*st = pageState{Page: pageGrok, Account: again, Note: l.t("登录没有完成：", "Sign-in did not finish: ") + words,
				NoteTone: wire.ToneDanger}
		}
		v.Sections = []wire.Section{{Items: []wire.Component{start}}}
		return v
	}
	if res.Status == protocol.LoginDone {
		name := providerName(l, grokBot)
		if res.Account != nil {
			name = accountTitle(l, *res.Account)
		}
		if again == "" && res.Account != nil {
			// A new account: the done step, which asks about the assistant (and offers no second sign-in).
			*st = pageState{Page: pageDone, Account: res.Account.AccountID}
			return a.doneView(ctx, l, *st)
		}
		note := l.t("已登录并配对：", "Signed in and paired: ")
		if again != "" {
			note = l.t("已重新登录：", "Signed in again: ")
		}
		*st = pageState{Page: pageGrok, Account: again, Note: note + name, NoteTone: wire.TonePositive}
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "button", Title: l.t("好了", "Done"), Action: &wire.Action{ID: actFinish}}}}}
		return v
	}
	v.Sections = []wire.Section{{Items: a.grokPending(l, *st)}}
	v.RefreshAfter = 30
	return v
}

func (a *accounts) grokPending(l lang, st pageState) []wire.Component {
	items := []wire.Component{}
	if httpsURL(st.LoginURL) {
		items = append(items, wire.Component{Type: "link", Title: l.t("打开授权页", "Open the sign-in page"), URL: st.LoginURL})
	}
	return append(items,
		wire.Component{Type: "text", Style: "secondary", Text: l.t("批准后这页每 30 秒检查一次，也可以点下面立即检查。授权页 10 分钟内有效。",
			"After you approve, this page checks every 30 seconds, or check now below. The page is valid for 10 minutes.")},
		wire.Component{Type: "button", Title: l.t("我已批准，立即检查", "I approved; check now"), Action: &wire.Action{ID: actGrokCheck}},
		wire.Component{Type: "button", Title: l.t("取消", "Cancel"), Role: "destructive", Action: &wire.Action{ID: actGrokStop}})
}

func (a *accounts) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	l := lang(r.Locale)
	var st pageState
	_ = r.DecodeState(&st)
	st.Note, st.NoteTone, st.ConfirmAuto = "", "", false
	done := func(note string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, wire.TonePositive
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	// refuse is a refusal of the page's own, in words with the next step, before anything is asked of the core.
	refuse := func(note string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, wire.ToneDanger
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	failed := func(err error, secret string) (*wire.ActionResult, error) {
		return refuse(reason(l, err, secret))
	}
	update := func(patch map[string]any) error {
		patch["account_id"] = st.Account
		return call(ctx, a.club, protocol.MethodClubAccountsUpdate, patch, nil)
	}
	switch r.Action {
	case actRepoCheck, actRepoBind, actRepoAuthorize, actRepoResume, actRepoRefresh:
		return a.repoAction(ctx, r, l, st)
	case actSync:
		// club.accounts.sync answers ok whether or not the sync worked (a failure is kept on the account), so the
		// outcome is read back from the account.
		before, _ := a.account(ctx, st.Account)
		if err := call(ctx, a.club, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: st.Account}, nil); err != nil {
			return failed(err, "")
		}
		after, err := a.account(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		if after.Status == protocol.AccountError || after.LastError != "" {
			st.Note, st.NoteTone = l.t("检查过了，还没好。", "Checked; still not working. ")+problemWords(l, after), wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if before.Status == protocol.AccountError || before.LastError != "" {
			return done(l.t("检查过了，已经恢复正常，最新的对话也拉下来了", "Checked: working again, and the latest chats are in"))
		}
		return done(l.t("已同步，最新的对话已经拉下来了", "Synced; the latest chats are in"))
	case actRecheck:
		return a.recheck(ctx, r, l, st)
	case actPause, actResume:
		paused := r.Action == actPause
		if err := update(map[string]any{"paused": paused}); err != nil {
			return failed(err, "")
		}
		if paused {
			return done(l.t("已暂停同步：不再拉取它的新对话，也不能给它发消息", "Paused: no new chats are fetched and nothing can be sent"))
		}
		return done(l.t("已恢复同步：会继续自动拉取它的新对话", "Resumed: new chats are fetched again"))
	case actControl:
		level, err := core.ParseControl(r.Values[fieldControl])
		if err != nil || r.Values[fieldControl] == "" {
			return refuse(l.t("没认出选的是哪一项，请重新选一次", "That choice was not recognized; choose again"))
		}
		if level == core.ControlAuto {
			cur, _ := a.account(ctx, st.Account)
			if core.Control(cur.AssistantControl) != core.ControlAuto {
				st.ConfirmAuto = true
				st.Note = l.t("开启「直接放行」前，请先在下面的确认卡片里查看风险与安全护栏，再点「确认开启直接放行」",
					"Before turning on Auto, please review the risks and safety guards on the card below, then tap Confirm turning on Auto")
				st.NoteTone = wire.ToneCaution
				return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
			}
		}
		st.ConfirmAuto = false
		if err := update(map[string]any{"assistant_control": level}); err != nil {
			return failed(err, "")
		}
		switch level {
		case core.ControlRead:
			return done(l.t("已设为只读：Persona 助手现在能读这个账号的对话，但不会替你发消息", "Set to Read: the Persona assistant can now read this account's chats, not write"))
		case core.ControlConfirm:
			return done(l.t("已设为可代发：Persona 助手能读，也能替你发消息，每次发之前先在手机上问你", "Set to Confirm: the Persona assistant can read and write, asking you each time"))
		case core.ControlFollow:
			return done(l.t("已设为持续跟进：持有生效租约的对话在限定轮数和有效期内可连续跟进，其余写操作仍每次先在手机上问你", "Set to Follow: chats with an active lease can continue within their turn and time limits; other writes still ask you each time"))
		case core.ControlAuto:
			return done(l.t("已开启直接放行：Persona 助手发消息不再逐次询问，额度预检、未确认发送保护、单对话频率限制与仓库强制确认护栏已生效", "Set to Auto: the Persona assistant can write without asking each time; allowance, unconfirmed-send, burst-limit and repository confirmation guards are active"))
		}
		return done(l.t("已关闭：Persona 助手现在看不到这个账号", "Off: the Persona assistant no longer sees this account"))
	case actControlAutoConfirm:
		st.ConfirmAuto = false
		if err := update(map[string]any{"assistant_control": core.ControlAuto}); err != nil {
			return failed(err, "")
		}
		return done(l.t("已开启直接放行：Persona 助手发消息不再逐次询问，额度预检、未确认发送保护、单对话频率限制与仓库强制确认护栏已生效", "Set to Auto: the Persona assistant can write without asking each time; allowance, unconfirmed-send, burst-limit and repository confirmation guards are active"))
	case actControlAutoCancel:
		st.ConfirmAuto = false
		return done(l.t("已取消开启直接放行，保持当前档位", "Canceled turning on Auto; kept the current level"))
	case actLeaseGrant:
		sessKey := strings.TrimSpace(r.Values[fieldLeaseSession])
		if sessKey == "" {
			return refuse(l.t("还没选对话：请先选一个要跟进的对话", "No chat chosen: pick a chat to follow first"))
		}
		if space, err := a.club.Space(ctx, core.DefaultSpace); err == nil {
			if sessions, err := space.Store().AccountSessions(ctx, st.Account); err == nil {
				for _, sess := range sessions {
					if sess.SessionKey == sessKey || sess.ExternalID == sessKey {
						sessKey = sess.SessionKey
						break
					}
				}
			}
		}
		ttl := cmp.Or(strings.TrimSpace(r.Values[fieldLeaseTTL]), core.FollowTTL2h)
		maxTurns := core.DefaultFollowMaxTurns
		if rawTurns := strings.TrimSpace(r.Values[fieldLeaseTurns]); rawTurns != "" {
			n, err := strconv.Atoi(rawTurns)
			if err != nil || n < 1 || n > core.MaxFollowMaxTurns {
				return refuse(l.t("轮数上限填得不对，请选 10、20 或 50 轮", "Invalid turn limit; choose 10, 20 or 50 turns"))
			}
			maxTurns = n
		}
		allowBg := strings.TrimSpace(r.Values[fieldLeaseBackground]) == "on"
		note := strings.TrimSpace(r.Values[fieldLeaseNote])
		if len(utf16.Encode([]rune(note))) > 200 {
			return refuse(l.t("备注太长了：最多 200 个字，请删短一点", "The note is too long: at most 200 characters"))
		}
		var granted protocol.FollowLease
		if err := call(ctx, a.club, protocol.MethodClubLeasesGrant, protocol.GrantFollowLeaseParams{
			AccountID:       st.Account,
			SessionKey:      sessKey,
			TTL:             ttl,
			MaxTurns:        maxTurns,
			AllowBackground: allowBg,
			Note:            note,
		}, &granted); err != nil {
			return failed(err, "")
		}
		acct, _ := a.account(ctx, st.Account)
		chatTitle := sessionDisplayTitle(l, acct.Provider, granted.SessionTitle)
		msg := l.t(fmt.Sprintf("已为「%s」新建跟进租约（最多 %d 轮，有效期 %s）", chatTitle, granted.MaxTurns, ttlWords(l, ttl)),
			fmt.Sprintf("Created follow-up lease for %q (up to %d turns, valid for %s)", chatTitle, granted.MaxTurns, ttlWords(l, ttl)))
		if core.Control(acct.AssistantControl) != core.ControlFollow {
			msg += l.t("；将上面的「助手指挥」设为「持续跟进」后生效", "; set Assistant control above to Follow for it to take effect")
		}
		return done(msg)
	case actLeaseRevoke:
		leaseID := strings.TrimSpace(r.Values[valLeaseID])
		if leaseID == "" {
			return refuse(l.t("没找到要撤销的租约，请刷新页面再试", "No lease specified; refresh and try again"))
		}
		var revRes protocol.RevokeFollowLeaseResult
		if err := call(ctx, a.club, protocol.MethodClubLeasesRevoke, protocol.RevokeFollowLeaseParams{
			AccountID: st.Account,
			LeaseID:   leaseID,
			Reason:    "owner_ui",
		}, &revRes); err != nil {
			return failed(err, "")
		}
		return done(l.t("已撤销这条跟进租约：助手在这个对话发消息已恢复为每次先在手机上问你", "Follow-up lease revoked: the assistant will ask on your phone before each message in this chat again"))
	case actLeaseRevokeAll:
		var revRes protocol.RevokeFollowLeaseResult
		if err := call(ctx, a.club, protocol.MethodClubLeasesRevoke, protocol.RevokeFollowLeaseParams{
			AccountID: st.Account,
			Reason:    "owner_ui_all",
		}, &revRes); err != nil {
			return failed(err, "")
		}
		return done(l.t("已撤销这个账号下的全部跟进租约", "Revoked all follow-up leases for this account"))
	case actAssistantBackend:
		on := strings.TrimSpace(r.Values[fieldAssistantBackend]) == "on"
		if on && !a.runtimeEnabled() {
			return refuse(l.t("现在开不了：先到 ", "Cannot be switched on: first switch on ") + settingsPath(l) +
				l.t(" 里打开「"+settingRuntime+"」", " → \""+settingRuntime+"\""))
		}
		if err := update(map[string]any{"assistant_backend": on}); err != nil {
			return failed(err, "")
		}
		if on {
			return done(l.t("已设为主助手：在 Persona 对话里选它，就能直接跟它聊", "Now a main assistant: pick it in Persona's chats to talk to it"))
		}
		return done(l.t("已不再当主助手：Persona 对话里不会再列出它", "No longer a main assistant: Persona's chats stop offering it"))
	case actInbox:
		on := strings.TrimSpace(r.Values[fieldInbox]) == "on"
		if err := update(map[string]any{"inbox": on}); err != nil {
			return failed(err, "")
		}
		if on {
			return done(l.t("这个账号的对话会进收件箱", "This account's chats go to the Inbox"))
		}
		return done(l.t("这个账号的对话不再进收件箱", "This account's chats stay out of the Inbox"))
	case actQuotaDisplay:
		disp := strings.TrimSpace(r.Values[fieldQuotaDisplay])
		if err := update(map[string]any{"quota_display": disp}); err != nil {
			return failed(err, "")
		}
		if disp == protocol.QuotaDisplayOff {
			return done(l.t("头像上不再显示额度", "The avatar no longer shows an allowance"))
		}
		return done(l.t("已改好头像上显示的额度", "The avatar's allowance is changed"))
	case actRename:
		name := strings.TrimSpace(r.Values[fieldName])
		switch {
		case name == "":
			return refuse(l.t("名字不能是空的：输入一个名字再点「改名」", "The name is empty: type one, then Rename"))
		case len(utf16.Encode([]rune(name))) > maxDisplayName:
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点再点「改名」", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		if err := update(map[string]any{"display_name": name}); err != nil {
			return failed(err, "")
		}
		return done(l.t("已改名为「", "Renamed to \"") + name + l.t("」", "\""))
	case actRemove, actPurge:
		p := protocol.RemoveAccountParams{AccountID: st.Account, DeleteData: r.Action == actPurge}
		if err := call(ctx, a.club, protocol.MethodClubAccountsRemove, p, nil); err != nil {
			return failed(err, "")
		}
		a.term().deleteAccount(st.Account)
		if p.DeleteData {
			return &wire.ActionResult{Pop: true, Toast: l.t("已移除这个账号，并删除了它的聊天记录", "Removed, with its chats deleted")}, nil
		}
		return &wire.ActionResult{Pop: true, Toast: l.t("已移除这个账号，以前的聊天记录还留着", "Removed; its chats are kept")}, nil
	case actPair:
		cred := strings.TrimSpace(r.Values[fieldCredential])
		name := strings.TrimSpace(r.Values[fieldName])
		switch {
		case cred == "":
			return refuse(l.t("还没填密钥：把对方网站给你的那串密钥粘贴进来，再点「添加」", "No key yet: paste the key from the service, then Add"))
		case len(cred) < minCredential:
			return refuse(l.t("这串密钥太短了，可能没复制完整：回对方网站重新复制整串再试", "The key is too short; it may be cut off. Copy all of it again"))
		case len(cred) > maxCredential:
			return refuse(l.t("这串密钥太长了，可能多复制了别的内容：只复制密钥本身再试", "The key is too long; copy only the key"))
		case len(utf16.Encode([]rune(name))) > maxDisplayName:
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		var acct protocol.Account
		p := protocol.AddAccountParams{Provider: st.Provider, Credential: cred, DisplayName: name}
		if err := call(ctx, a.club, protocol.MethodClubAccountsAdd, p, &acct); err != nil {
			return failed(explain(l, err, place{Provider: st.Provider, Pairing: pairKey}), cred)
		}
		return a.added(ctx, r, acct), nil
	case actBrowserPair:
		if !a.available(st.Provider) {
			return refuse(l.t("这类账号还没打开：先到 ", "Not switched on: first switch on ") + settingsPath(l) +
				l.t(" 里打开「"+settingBrowser+"」", " → \""+settingBrowser+"\""))
		}
		if parseMachineChoice(r.Values[fieldMachine]).MachineID == "" {
			return refuse(l.t("还没选电脑：在「用哪台电脑」里选一台，再点「检查登录并添加」", "No computer chosen: choose one, then try again"))
		}
		if len(utf16.Encode([]rune(strings.TrimSpace(r.Values[fieldName])))) > maxDisplayName {
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		acct, err := a.pairBrowserAccount(ctx, st.Provider, r.Values[fieldMachine], r.Values[fieldName])
		if why := protocol.ReasonOf(err); why == provider.CodeAuthFailed || why == protocol.ReasonNotSignedIn {
			site, _ := browserSite(st.Provider)
			return refuse(l.t("那台电脑上 Persona 控制的浏览器里还没登录好 ", "The browser Persona controls on that computer is not signed in to ") + siteHost(site.AppURL) +
				l.t("：先在那个浏览器里登录，再回来点「检查登录并添加」", ": sign in in that browser first, then come back and try again"))
		}
		if err != nil {
			return failed(explain(l, err, a.pairPlace(ctx, st.Provider, r.Values[fieldMachine])), "")
		}
		return a.added(ctx, r, acct), nil
	case actFinish:
		return a.finish(ctx, l, st), nil
	case actOpenAccount:
		return &wire.ActionResult{View: a.view(ctx, r, pageState{Page: pageAccount, Account: st.Account})}, nil
	case actGrokStart:
		var res protocol.GrokBotLoginStart
		err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotStart, protocol.GrokBotLoginStartParams{AccountID: st.Account}, &res)
		if protocol.ReasonOf(err) == protocol.ReasonLoginLimit {
			st.Note, st.NoteTone = reasonWords(l, protocol.ReasonLoginLimit), wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if err != nil {
			return failed(err, "")
		}
		if res.LoginID == "" || !httpsURL(res.LoginURL) {
			return refuse(l.t("登录没有给出可以打开的授权页，请稍后重新开始", "The sign-in gave no page to open; start again later"))
		}
		st = pageState{Page: pageGrok, Account: st.Account, Login: res.LoginID, LoginURL: res.LoginURL}
		return &wire.ActionResult{View: a.view(ctx, r, st), OpenURL: res.LoginURL}, nil
	case actGrokCheck:
		return &wire.ActionResult{View: a.view(ctx, r, st)}, nil
	case actGrokStop:
		if st.Login != "" {
			if err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotCancel, protocol.LoginParams{LoginID: st.Login}, nil); err != nil {
				st.Login, st.LoginURL = "", ""
				return failed(err, "")
			}
		}
		st = pageState{Page: pageGrok, Account: st.Account}
		return done(l.t("已取消这次登录", "Sign-in canceled"))
	case actTakeover:
		mid, err := a.runTakeover(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已把接入说明发给它（电脑编号 "+mid+"）。连上后，请到 Persona 的「机器」页点「这是我的电脑」确认",
			"Instructions sent ("+mid+"); once it connects, confirm it on the Machines page"))
	case actWake:
		mid, err := a.runWake(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已让它把电脑重新连上（电脑编号 "+mid+"）", "Asked it to reconnect its computer ("+mid+")"))
	case actTermSaveSSH, actTermClearSSH, actTermProbe, actTermStage, actTermPreset, actTermExec, actTermCancel, actTermChatStage, actTermChatSend,
		actTermComputerRefresh, actTermComputerStage, actTermComputerPreset, actTermComputerRun,
		actTermMachineStage, actTermMachinePreset, actTermMachineExec:
		return a.handleTerminalAction(ctx, r, l, st, done, refuse, failed)
	}
	return nil, nil
}

// accountName is an account's name for the assistant's tools and the machine it pairs (Chinese, names.Default): the
// space's name for it, else its service's.
func accountName(a protocol.Account) string {
	return names.Fallback(names.Default, a.Provider, a.DisplayName)
}

// accountTitle is what an account is called on a page in l: the space's name for it in that language (owner-chosen
// names as typed), else its service's, never the raw provider id.
func accountTitle(l lang, a protocol.Account) string {
	if l.t("zh", "en") == "en" && a.DisplayNameEn != "" {
		return a.DisplayNameEn
	}
	return names.Fallback(string(l), a.Provider, a.DisplayName)
}

// providerName is a service's name as people know it, in l (names.Brand, the one label table).
func providerName(l lang, p string) string {
	return names.Brand(string(l), p)
}

func statusBadge(l lang, acct protocol.Account) *wire.Badge {
	switch acct.Status {
	case protocol.AccountActive:
		if acct.LastError != "" {
			return &wire.Badge{Text: l.t("有点小问题", "Hiccup"), Tone: wire.ToneCaution}
		}
		if acct.Quota != nil {
			switch acct.Quota.State {
			case protocol.QuotaExhausted:
				return &wire.Badge{Text: l.t("额度用完", "Used up"), Tone: wire.ToneDanger}
			case protocol.QuotaLow:
				return &wire.Badge{Text: l.t("额度快用完", "Low"), Tone: wire.ToneCaution}
			}
		}
		return &wire.Badge{Text: l.t("正常", "Working"), Tone: wire.TonePositive}
	case protocol.AccountPaused:
		return &wire.Badge{Text: l.t("已暂停", "Paused"), Tone: wire.ToneNeutral}
	case protocol.AccountError:
		return &wire.Badge{Text: l.t("要处理", "Needs you"), Tone: wire.ToneDanger}
	}
	return &wire.Badge{Text: l.t("状态不明", "Unknown"), Tone: wire.ToneNeutral}
}

// clock is what r's page writes times against: the club's now, in the viewer's time zone.
func (a *accounts) clock(r *ext.UIRequest) clock {
	return clock{now: a.club.Now(), loc: r.Location()}
}

// httpsURL is a link the hub draws: what wire.CheckHTTPS and the schema's lowercase "https://host" let through.
func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && strings.HasPrefix(s, "https://") &&
		wire.CheckHTTPS(s, "link") == nil
}
