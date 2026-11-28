package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/nwverse/ensemble/internal/store"
	"github.com/nwverse/ensemble/internal/vault"
)

// inboxProvider is a fake service whose chats and replies the test scripts, counting every call that reaches it.
// Nothing here is a real account or a real conversation.
type inboxProvider struct {
	*echo.Provider
	mu       sync.Mutex
	script   map[string][]provider.RemoteMessage // by external session id
	sends    []provider.Outgoing
	calls    atomic.Int64 // every adapter call
	failSend bool
	status   provider.SendStatus
}

func newInboxProvider() *inboxProvider {
	return &inboxProvider{Provider: echo.New(), script: map[string][]provider.RemoteMessage{}, status: provider.Sent}
}

func (p *inboxProvider) Name() string { return "echo" }

func (p *inboxProvider) say(session, role, id, text string, at int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script[session] = append(p.script[session], provider.RemoteMessage{ExternalID: id, Role: role,
		Blocks: []protocol.Block{protocol.TextBlock(text)}, CreateTimeMs: at, Position: at})
}

func (p *inboxProvider) ListSessions(ctx context.Context, cred string) ([]provider.RemoteSession, error) {
	p.calls.Add(1)
	return p.Provider.ListSessions(ctx, cred)
}

func (p *inboxProvider) ListMessages(ctx context.Context, cred string, ref provider.SessionRef, o provider.ListOptions) (provider.RemotePage, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.script[ref.ExternalID]; ok {
		return provider.RemotePage{Messages: append([]provider.RemoteMessage(nil), m...)}, nil
	}
	return p.Provider.ListMessages(ctx, cred, ref, o)
}

func (p *inboxProvider) Send(ctx context.Context, cred string, ref provider.SessionRef, m provider.Outgoing, sc provider.SendContext) (provider.SendResult, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failSend {
		return provider.SendResult{}, &provider.Error{Code: "REFUSED", HTTPStatus: 400}
	}
	p.sends = append(p.sends, m)
	if p.status == provider.Unconfirmed {
		return provider.SendResult{Status: provider.Unconfirmed, Error: "UNCONFIRMED"}, nil
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: "u-" + m.ClientID}, nil
}

func (p *inboxProvider) sent() []provider.Outgoing {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Outgoing(nil), p.sends...)
}

type inboxEnv struct {
	t     *testing.T
	h     *exttest.Host
	club  *core.Club
	space *core.Space
	prov  *inboxProvider
	now   *atomic.Int64
	ids   []string // account ids

	pmu      sync.Mutex
	pushes   []notification
	pushFail int // the next this many alerts fail
	rounds   atomic.Int64
	mark     int64 // rounds at the last sync
}

// push is the alert sender of the test program: it records what the hub would have been asked to send.
func (e *inboxEnv) push(_ context.Context, n notification) error {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.pushFail > 0 {
		e.pushFail--
		return errors.New("hub unreachable")
	}
	e.pushes = append(e.pushes, n)
	return nil
}

func (e *inboxEnv) alerts() []notification {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return append([]notification(nil), e.pushes...)
}

func (e *inboxEnv) round() { e.rounds.Add(1) }

// settle waits until the program's rounds of alerts have stopped coming: at least one after the last sync when one
// is expected (the Inbox and its alerts are on), then none for a while. The alerts are sent by the program's own
// goroutine, so a test can only wait for it; the guards themselves are tested directly, without waiting.
func (e *inboxEnv) settle(expectRound bool) {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for expectRound && e.rounds.Load() <= e.mark && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for last, quiet := e.rounds.Load(), 0; quiet < 12 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		if now := e.rounds.Load(); now != last {
			last, quiet = now, 0
		} else {
			quiet++
		}
	}
}

// waitAlerts waits until n alerts have been sent, or fails.
func (e *inboxEnv) waitAlerts(n int) []notification {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := e.alerts(); len(got) >= n {
			e.settle(false)
			if got = e.alerts(); len(got) != n {
				e.t.Fatalf("want %d alerts, got %+v", n, got)
			}
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("want %d alerts, got %+v", n, e.alerts())
	return nil
}

// noAlerts checks that none was sent once the program's rounds after the last sync are over.
func (e *inboxEnv) noAlerts(expectRound bool) {
	e.t.Helper()
	e.settle(expectRound)
	if got := e.alerts(); len(got) != 0 {
		e.t.Fatalf("unexpected alerts: %+v", got)
	}
}

func (e *inboxEnv) clock() time.Time { return time.UnixMilli(e.now.Load()) }

// newInbox starts the extension with the fake service, two accounts (one chat each, "main" and the read-only "notes").
func newInboxEnv(t *testing.T, cfg string) *inboxEnv {
	t.Helper()
	e := &inboxEnv{t: t, prov: newInboxProvider(), now: &atomic.Int64{}}
	e.now.Store(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).UnixMilli())
	e.prov.Provider.Now = e.clock
	e.club = core.New(core.Options{Registry: provider.NewRegistry(e.prov), FollowUps: []time.Duration{}, Now: e.clock})
	e.h = startHost(t, pac.Manifest, exttest.Serve(t, program(e.club, pac.Version(), e.push, e.round)), json.RawMessage(cfg))
	ctx := context.Background()
	var err error
	if e.space, err = e.club.Space(ctx, core.DefaultSpace); err != nil {
		t.Fatal(err)
	}
	for _, cred := range []string{"echo-fixture-one", "echo-fixture-two"} {
		a, err := e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: cred})
		if err != nil {
			t.Fatal(err)
		}
		e.ids = append(e.ids, a.AccountID)
	}
	e.sync() // the chats exist before anything is said in them: what comes later is news, not a backfill
	e.now.Add(10 * 60_000)
	return e
}

func (e *inboxEnv) collect() []wire.ConnectorInboxThing {
	e.t.Helper()
	var res wire.ConnectorCollectResult
	if err := e.h.Call("connector.inbox.collect", wire.ConnectorParams{Contribution: InboxConnector}, &res); err != nil {
		e.t.Fatalf("inbox.collect: %v", err)
	}
	return res.Things
}

func (e *inboxEnv) reply(p wire.ConnectorInboxReply) error {
	p.Contribution = InboxConnector
	return e.h.Call("connector.inbox.reply", p, nil)
}

func (e *inboxEnv) sync() {
	e.t.Helper()
	e.mark = e.rounds.Load()
	for _, id := range e.ids {
		if err := e.space.SyncAccount(context.Background(), id); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *inboxEnv) sessionKey(accountIdx int, ext string) string {
	e.t.Helper()
	return core.SessionKey(echo.Name, e.ids[accountIdx], ext)
}

func thingOfThread(things []wire.ConnectorInboxThing, thread string) *wire.ConnectorInboxThing {
	for i := range things {
		if things[i].Thread == thread {
			return &things[i]
		}
	}
	return nil
}

const inboxOn = `{"demo_echo":true,"inbox_conversations":true}`

func TestInboxOffByDefault(t *testing.T) {
	e := newInboxEnv(t, `{"demo_echo":true}`)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hello", e.now.Load())
	e.sync()
	if got := e.collect(); len(got) != 0 {
		t.Fatalf("inbox.collect with the switch off: %+v", got)
	}
	var st wire.ConnectorStatus
	if err := e.h.Call("connector.status", wire.ConnectorParams{Contribution: InboxConnector}, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != wire.ConnectorStateNotConfigured || len(st.Guide) == 0 {
		t.Fatalf("status with the switch off: %+v", st)
	}
	var cr wire.ConnectorCallResult
	if err := e.h.Call("connector.catalog.call", wire.ConnectorCatalogCallParams{Contribution: InboxConnector, Source: InboxConnector,
		Tool: toolConversations}, &cr); err != nil || strings.Contains(cr.Text, "agent") {
		t.Fatalf("catalog.call with the switch off: %q %v", cr.Text, err)
	}
	for _, ev := range e.h.Events() {
		if strings.HasSuffix(ev.Name, "changed") && strings.Contains(string(ev.Params), InboxConnector) {
			t.Fatalf("told the hub of a change with the switch off: %+v", ev)
		}
	}
}

func TestInboxRowPerChat(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleUser, "u1", "run the report", now-60_000)
	e.prov.say("main", protocol.RoleAssistant, "m2", "The report is ready.\nIt has 3 sections.", now-30_000)
	e.sync()
	things := e.collect()
	// the demo provider lists "main" and the read-only "notes" for each of the two accounts
	if len(things) != 4 {
		t.Fatalf("want a row for each of 4 chats, got %d: %+v", len(things), things)
	}
	key := e.sessionKey(0, "main")
	row := thingOfThread(things, key)
	if row == nil {
		t.Fatalf("no row for %s", key)
	}
	if row.Kind != kindAgentReply || row.Last != "m:m2" || !strings.HasSuffix(row.Title, " · Echo") || !strings.HasPrefix(row.Title, row.Sender+" · ") {
		t.Fatalf("row: %+v", row)
	}
	if row.Preview != "The report is ready. It has 3 sections." || row.Extra == nil || row.Extra.RuleTier != wire.ConnectorTierFyi {
		t.Fatalf("preview or fallback tier: %+v", row)
	}
	if !row.CanReply || row.ReplyTo == "" || row.Done != "" {
		t.Fatalf("reply: %+v", row)
	}
	if !strings.Contains(row.Context, "我：run the report") || !strings.Contains(row.Context, "外部数据") {
		t.Fatalf("context: %q", row.Context)
	}
	var tags []string
	for _, tg := range row.Tags {
		tags = append(tags, tg.Key+"="+tg.Value)
	}
	if strings.Join(tags, ",") != "chat="+key+",sender="+e.ids[0] {
		t.Fatalf("tags: %v", tags)
	}
	if notes := thingOfThread(things, e.sessionKey(0, "notes")); notes == nil || notes.CanReply {
		t.Fatalf("a read-only chat must not offer a reply: %+v", notes)
	}
}

func TestInboxReadAndRepliedState(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "first", now-120_000)
	e.sync()
	key := e.sessionKey(0, "main")
	if row := thingOfThread(e.collect(), key); row == nil || row.Read || row.Last != "m:m1" {
		t.Fatalf("a first reply in an existing chat is unread: %+v", row)
	}
	e.now.Add(60_000)
	e.prov.say("main", protocol.RoleAssistant, "m2", "second", e.now.Load())
	e.sync()
	row := thingOfThread(e.collect(), key)
	if row == nil || row.Read || row.Last != "m:m2" {
		t.Fatalf("a new reply should be unread with a new last: %+v", row)
	}
	last := row.Last
	// the same messages again change nothing
	e.sync()
	if again := thingOfThread(e.collect(), key); again.Last != last || again.Read {
		t.Fatalf("a repeated sync moved the row: %+v", again)
	}
	// seen in the Agents page: read
	if err := e.space.MarkRead(context.Background(), protocol.MarkReadParams{SessionKey: key}); err != nil {
		t.Fatal(err)
	}
	if row := thingOfThread(e.collect(), key); !row.Read {
		t.Fatalf("a chat marked read: %+v", row)
	}
}

func TestInboxDoesNotFloodWhenSwitchedOn(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	ctx := context.Background()
	now := e.now.Load()
	// an unread reply from two days ago, in a chat that is still within the window
	e.prov.say("main", protocol.RoleAssistant, "old", "old news", now-48*3600_000)
	e.sync()
	key := e.sessionKey(0, "main")
	e.collect() // the switch is seen on for the first time
	if e.space.InboxSince(ctx) == 0 {
		t.Fatal("the first collect with the switch on should remember when")
	}
	if row := thingOfThread(e.collect(), key); row == nil || !row.Read {
		t.Fatalf("a chat that was quiet when the switch went on must come in read: %+v", row)
	}
	// news after the switch is unread
	e.now.Add(3600_000)
	e.prov.say("main", protocol.RoleAssistant, "new", "fresh", e.now.Load())
	e.sync()
	if row := thingOfThread(e.collect(), key); row == nil || row.Read || row.Last != "m:new" {
		t.Fatalf("a reply after the switch is unread: %+v", row)
	}
}

func TestInboxWindowAndCap(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	ctx := context.Background()
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "recent", now-1000)
	e.sync()
	// 130 chats in the store, newest first by rank: only the newest inboxMaxRows are rows
	st := e.space.Store()
	var rows []store.Session
	for i := range 130 {
		rows = append(rows, store.Session{SessionKey: fmt.Sprintf("agent:echo:%s:bulk%03d", e.ids[0], i), AccountID: e.ids[0], Provider: echo.Name,
			ExternalID: fmt.Sprintf("bulk%03d", i), Title: fmt.Sprintf("bulk %d", i), Kind: protocol.KindMain, CanSend: true,
			RankTimeMs: now - int64(i)*1000 - 5000, Ref: map[string]string{}})
	}
	if err := st.Import(ctx, store.Import{Sessions: rows}); err != nil {
		t.Fatal(err)
	}
	got := e.collect()
	if len(got) != inboxMaxRows {
		t.Fatalf("rows: %d, want the cap %d", len(got), inboxMaxRows)
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.After(got[i-1].At) {
			t.Fatalf("rows are not newest first at %d", i)
		}
	}
	e.now.Add(9 * 24 * 3600_000) // everything is now 9 days old
	if got := e.collect(); len(got) != 0 {
		t.Fatalf("chats older than the window should not be rows: %d", len(got))
	}
}

// A send that failed, or that no echo has confirmed, is not the owner having answered: it must not close the row
// (a thing with done set never opens an Inbox item, so every later reply would be lost).
func TestInboxFailedSendDoesNotMuteTheChat(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "ok?", now-1000)
	e.sync()
	key := e.sessionKey(0, "main")
	e.prov.mu.Lock()
	e.prov.failSend = true
	e.prov.mu.Unlock()
	if err := e.reply(wire.ConnectorInboxReply{Thread: key, Text: "this one fails"}); err == nil {
		t.Fatal("the send should have failed")
	}
	e.now.Add(3 * 24 * 3600_000)
	e.prov.say("main", protocol.RoleAssistant, "m2", "a later reply", e.now.Load())
	e.sync()
	row := thingOfThread(e.collect(), key)
	if row == nil || row.Last != "m:m2" || row.Done != "" || row.Read {
		t.Fatalf("a failed send must leave the chat open to the next reply: %+v", row)
	}
}

func TestInboxSkipsAssistantBackingChatsAndOffAccounts(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	ctx := context.Background()
	backing := e.sessionKey(0, "main")
	if err := e.space.SetAssistantSessionKeys(ctx, []string{backing}); err != nil {
		t.Fatal(err)
	}
	things := e.collect()
	if thingOfThread(things, backing) != nil {
		t.Fatal("a chat that backs the Persona assistant must stay out of the Inbox")
	}
	// the per-account switch, through the page's action
	res := act(t, e.h, pageState{Page: pageAccount, Account: e.ids[1]}, actInbox, map[string]string{fieldInbox: "off"})
	if res == nil {
		t.Fatal("no result")
	}
	things = e.collect()
	for _, th := range things {
		if strings.Contains(th.Thread, e.ids[1]) {
			t.Fatalf("an account switched off still has a row: %s", th.Thread)
		}
	}
	if thingOfThread(things, e.sessionKey(0, "notes")) == nil {
		t.Fatal("the other account's chats should stay")
	}
	act(t, e.h, pageState{Page: pageAccount, Account: e.ids[1]}, actInbox, map[string]string{fieldInbox: "on"})
	if got := e.collect(); len(got) != 3 {
		t.Fatalf("switched on again: %d rows", len(got))
	}
}

// The inbox switch and the assistant's control level are separate: neither reads or changes the other.
func TestInboxDoesNotTouchAssistantControl(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	ctx := context.Background()
	before, _ := e.space.AssistantControl(ctx, e.ids[0])
	act(t, e.h, pageState{Page: pageAccount, Account: e.ids[0]}, actInbox, map[string]string{fieldInbox: "off"})
	act(t, e.h, pageState{Page: pageAccount, Account: e.ids[0]}, actInbox, map[string]string{fieldInbox: "on"})
	after, _ := e.space.AssistantControl(ctx, e.ids[0])
	if before != core.ControlOff || after != before {
		t.Fatalf("assistant control moved: %q -> %q", before, after)
	}
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	if thingOfThread(e.collect(), e.sessionKey(0, "main")) == nil {
		t.Fatal("an account the assistant may not see should still be in the Inbox")
	}
}

func TestInboxRoutesAreExclusive(t *testing.T) {
	collect := func(e *inboxEnv) int { return len(e.collect()) }
	catalog := func(e *inboxEnv) (n int, kinds bool) {
		var cr wire.ConnectorCallResult
		if err := e.h.Call("connector.catalog.call", wire.ConnectorCatalogCallParams{Contribution: InboxConnector, Source: InboxConnector,
			Tool: toolConversations}, &cr); err != nil {
			e.t.Fatal(err)
		}
		var out struct {
			Things []wire.ConnectorInboxThing `json:"things"`
		}
		if err := json.Unmarshal([]byte(cr.Text), &out); err != nil {
			e.t.Fatal(err)
		}
		for _, th := range out.Things {
			if th.Thread == "" || th.Last == "" {
				e.t.Fatalf("a subscription row needs a thread and a last: %+v", th)
			}
			kinds = kinds || th.Kind != ""
		}
		return len(out.Things), kinds
	}
	direct := newInboxEnv(t, inboxOn)
	direct.prov.say("main", protocol.RoleAssistant, "m1", "hi", direct.now.Load())
	direct.sync()
	if n, _ := catalog(direct); n != 0 || collect(direct) != 4 {
		t.Fatalf("direct: catalog %d, collect %d", n, collect(direct))
	}
	via := newInboxEnv(t, `{"demo_echo":true,"inbox_conversations":true,"inbox_via_subscription":true}`)
	via.prov.say("main", protocol.RoleAssistant, "m1", "hi", via.now.Load())
	via.sync()
	n, kinds := catalog(via)
	if n != 4 || kinds || collect(via) != 0 {
		t.Fatalf("via subscription: catalog %d (kinds %v), collect %d", n, kinds, collect(via))
	}
	var bad wire.ConnectorCallResult
	err := via.h.Call("connector.catalog.call", wire.ConnectorCatalogCallParams{Contribution: InboxConnector, Source: InboxConnector, Tool: "inbox_things"}, &bad)
	var ee *ext.Error
	if !errors.As(err, &ee) || ee.Code != wire.ConnectorErrorCodeNotReadOnly {
		t.Fatalf("a tool that is not in the catalog must be refused as not_read_only: %v", err)
	}
}

// Reading the Inbox rows is reading what club has stored: nothing reaches the provider, so the browser accounts are
// not loaded by it.
func TestInboxCollectNeverCallsTheProvider(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	before := e.prov.calls.Load()
	for range 50 {
		e.collect()
		var cr wire.ConnectorCallResult
		_ = e.h.Call("connector.catalog.call", wire.ConnectorCatalogCallParams{Contribution: InboxConnector, Source: InboxConnector, Tool: toolConversations}, &cr)
		var st wire.ConnectorStatus
		_ = e.h.Call("connector.status", wire.ConnectorParams{Contribution: InboxConnector}, &st)
	}
	if after := e.prov.calls.Load(); after != before {
		t.Fatalf("the Inbox's reads called the provider %d times", after-before)
	}
}

func TestInboxReplySendsOnceThroughTheOutbox(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "ok?", e.now.Load())
	e.sync()
	key := e.sessionKey(0, "main")
	if err := e.reply(wire.ConnectorInboxReply{Thread: key, Text: "yes please", ClientID: "app-send-1"}); err != nil {
		t.Fatal(err)
	}
	sent := e.prov.sent()
	if len(sent) != 1 || sent[0].Text != "yes please" {
		t.Fatalf("sent: %+v", sent)
	}
	// the hub retrying the same send, with or without its own id, reaches the service once
	_ = e.reply(wire.ConnectorInboxReply{Thread: key, Text: "yes please", ClientID: "app-send-1"})
	_ = e.reply(wire.ConnectorInboxReply{Thread: key, Text: "again", ItemID: "i1", Last: "m:m1"})
	_ = e.reply(wire.ConnectorInboxReply{Thread: key, Text: "again", ItemID: "i1", Last: "m:m1"})
	if sent := e.prov.sent(); len(sent) != 2 {
		t.Fatalf("a retry must not send twice: %+v", sent)
	}
}

func TestInboxReplyRefusals(t *testing.T) {
	e := newInboxEnv(t, `{"demo_echo":true,"inbox_conversations":true,"inbox_reply":false}`)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	key := e.sessionKey(0, "main")
	for _, row := range e.collect() {
		if row.CanReply || row.ReplyTo != "" {
			t.Fatalf("inbox_reply is off but the row offers a reply: %+v", row)
		}
	}
	const secret = "reply-body-that-must-not-leak"
	if err := e.reply(wire.ConnectorInboxReply{Thread: key, Text: secret}); err == nil {
		t.Fatal("reply with inbox_reply off")
	}
	on := newInboxEnv(t, inboxOn)
	on.prov.say("main", protocol.RoleAssistant, "m1", "hi", on.now.Load())
	on.sync()
	good := on.sessionKey(0, "main")
	for name, p := range map[string]wire.ConnectorInboxReply{
		"no such chat":  {Thread: "agent:echo:a000000000000:nope", Text: secret},
		"read-only":     {Thread: on.sessionKey(0, "notes"), Text: secret},
		"empty":         {Thread: good, Text: "  \n"},
		"status change": {Thread: good, Text: secret, Changes: []wire.ConnectorStatusChange{{Ref: "r", Field: "f", Value: "v"}}},
	} {
		err := on.reply(p)
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats the reply: %v", name, err)
		}
	}
	if err := on.space.SetAssistantSessionKeys(context.Background(), []string{good}); err != nil {
		t.Fatal(err)
	}
	if err := on.reply(wire.ConnectorInboxReply{Thread: good, Text: secret}); err == nil {
		t.Error("replied into a chat that backs the Persona assistant")
	}
	if sent := append(e.prov.sent(), on.prov.sent()...); len(sent) != 0 {
		t.Fatalf("a refused reply reached the service: %+v", sent)
	}
}

func TestInboxReplyUnconfirmedIsNotAnError(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	e.prov.mu.Lock()
	e.prov.status = provider.Unconfirmed
	e.prov.mu.Unlock()
	if err := e.reply(wire.ConnectorInboxReply{Thread: e.sessionKey(0, "main"), Text: "maybe"}); err != nil {
		t.Fatalf("an unconfirmed send must not be reported as a failure (the owner would send it again): %v", err)
	}
}

func TestInboxReplyFailureIsAnError(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "hi", e.now.Load())
	e.sync()
	e.prov.mu.Lock()
	e.prov.failSend = true
	e.prov.mu.Unlock()
	if err := e.reply(wire.ConnectorInboxReply{Thread: e.sessionKey(0, "main"), Text: "will fail"}); err == nil {
		t.Fatal("a send the service refused should fail")
	}
}

func TestInboxChangedEvent(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	count := func() (n int) {
		for _, ev := range e.h.Events() {
			if strings.HasSuffix(ev.Name, ".changed") && strings.Contains(string(ev.Params), `"inbox"`) {
				n++
			}
		}
		return
	}
	e.prov.say("main", protocol.RoleAssistant, "m1", "one", e.now.Load())
	e.sync()
	deadline := time.Now().Add(3 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let the events of this change settle
	first := count()
	if first == 0 {
		t.Fatalf("a new reply should tell the hub: events %+v", e.h.Events())
	}
	e.sync() // nothing changed
	time.Sleep(200 * time.Millisecond)
	if count() != first {
		t.Fatalf("a sync that changed no row told the hub: %d -> %d", first, count())
	}
}

func TestInboxManifestRules(t *testing.T) {
	var m struct {
		Contributes struct {
			Connectors []wire.ConnectorItem `json:"connectors"`
			UI         []wire.UIItem        `json:"ui"`
		} `json:"contributes"`
		Configuration struct {
			Properties map[string]struct {
				Type    string `json:"type"`
				Default any    `json:"default"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	var club, inbox *wire.ConnectorItem
	for i := range m.Contributes.Connectors {
		switch m.Contributes.Connectors[i].ID {
		case Connector:
			club = &m.Contributes.Connectors[i]
		case InboxConnector:
			inbox = &m.Contributes.Connectors[i]
		}
	}
	if club == nil || inbox == nil {
		t.Fatalf("connectors: %+v", m.Contributes.Connectors)
	}
	// the host refuses an Inbox connector without these
	if p := m.Configuration.Properties["triage_rules"]; p.Type != "string" {
		t.Errorf("an Inbox connector needs a top-level triage_rules string setting: %+v", p)
	}
	for _, c := range []string{wire.ConnectorCapabilityInbox, wire.ConnectorCapabilityFallback, wire.ConnectorCapabilityEvents} {
		found := false
		for _, have := range inbox.Capabilities {
			found = found || have == c
		}
		if !found {
			t.Errorf("the Inbox connector lacks %q", c)
		}
		if c == wire.ConnectorCapabilityInbox {
			continue
		}
	}
	for _, k := range inbox.Kinds {
		if strings.TrimSpace(inbox.KindDescriptions[k]) == "" {
			t.Errorf("kind %q has no description", k)
		}
	}
	for _, c := range inbox.Capabilities {
		if c == wire.ConnectorCapabilityTools || c == wire.ConnectorCapabilityActions {
			t.Errorf("the Inbox connector must give the assistant no tools of its own: %q", c)
		}
	}
	if len(inbox.ConfirmAlways) != 0 || !slices.Equal(club.ConfirmAlways, []string{"club_agent_repo_authorize", "club_agent_repo_bind"}) {
		t.Errorf("confirm_always: club %v, inbox %v", club.ConfirmAlways, inbox.ConfirmAlways)
	}
	if d := m.Configuration.Properties["inbox_conversations"].Default; d != false {
		t.Errorf("inbox_conversations must default to off: %v", d)
	}
	var page *wire.UIItem
	for i := range m.Contributes.UI {
		if m.Contributes.UI[i].Placement == wire.PlacementInboxItem {
			page = &m.Contributes.UI[i]
		}
	}
	if page == nil || page.ID != InboxItemUI || page.Connector != InboxConnector {
		t.Errorf("the Inbox item page: %+v", page)
	}
}

func TestInboxCatalogPassesTheHostCheck(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	var cat wire.ConnectorCatalog
	if err := e.h.Call("connector.catalog.list", wire.ConnectorParams{Contribution: InboxConnector}, &cat); err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckConnectorCatalog(cat); err != nil {
		t.Fatal(err)
	}
	for _, tool := range cat.Sources[0].Tools {
		if strings.Contains(strings.ToLower(tool.Name), "send") || tool.Name == "inbox_things" {
			t.Errorf("the host refuses a subscription tool named like this: %q", tool.Name)
		}
	}
	recipe, _ := json.Marshal(map[string]any{"mode": "things", "tool": toolConversations, "args": map[string]any{}, "items_path": "$.things",
		"kind": "sub_message", "group_by": "thread"})
	if err := wire.ValidateJSON("ConnectorSubscriptionRecipe", recipe); err != nil {
		t.Fatalf("the documented subscription recipe is not valid: %v", err)
	}
}

// The extension's Inbox page draws the choices a reply ends with as buttons; a tap asks first and then sends.
func TestInboxChoicesAreButtonsThatReplyAfterConfirming(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleUser, "u1", "what now", now-60_000)
	e.prov.say("main", protocol.RoleAssistant, "m1", "Two ways forward, pick:\nA：run it now\nB：fix the cause\nleave it for now", now-1000)
	e.sync()
	key := e.sessionKey(0, "main")
	row := thingOfThread(e.collect(), key)
	if row == nil || row.Tier != wire.ConnectorTierAction {
		t.Fatalf("a reply that waits for a choice is for the owner: %+v", row)
	}
	item := &wire.UIInboxItem{ID: "i1", Extra: map[string]json.RawMessage{"connector": json.RawMessage(`{"thread":"` + key + `"}`)}}
	call := func(action string, values map[string]string, out any) error {
		return e.h.Call("ui.render", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
			Locale: "zh-Hans", Item: item}, out)
	}
	var v wire.View
	if err := call("", nil, &v); err != nil {
		t.Fatal(err)
	}
	var buttons []wire.Component
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "button" {
				buttons = append(buttons, c)
			}
		}
	}
	if len(buttons) != 3 || buttons[0].Title != "A：run it now" || buttons[2].Title != "leave it for now" {
		t.Fatalf("buttons: %+v", buttons)
	}
	for _, b := range buttons {
		if b.Action == nil || b.Action.Confirm == "" || b.Action.ID != actPick {
			t.Fatalf("a choice must ask before it sends: %+v", b.Action)
		}
	}
	if len(e.prov.sent()) != 0 {
		t.Fatal("drawing the page sent something")
	}
	var res wire.ActionResult
	pick := func(vals map[string]string) error {
		return e.h.Call("ui.action", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
			Locale: "zh-Hans", Item: item, Action: actPick, Values: vals}, &res)
	}
	if err := pick(buttons[1].Action.Values); err != nil {
		t.Fatal(err)
	}
	if sent := e.prov.sent(); len(sent) != 1 || sent[0].Text != "B：fix the cause" {
		t.Fatalf("sent: %+v", sent)
	}
	// a number that is not one of the choices, or text instead of a number, sends nothing
	_ = pick(map[string]string{valOption: "9", valLast: buttons[1].Action.Values[valLast], valSum: buttons[1].Action.Values[valSum]})
	_ = pick(map[string]string{valOption: "1"}) // no last or sum: not what was shown
	_ = pick(map[string]string{valOption: "-1"})
	_ = pick(map[string]string{valOption: "B：fix the cause; delete everything"})
	if sent := e.prov.sent(); len(sent) != 1 {
		t.Fatalf("a bad pick sent something: %+v", sent)
	}
}

func TestInboxChoicesNeedAReplyThatEndsInThem(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "Pick:\nA：one\nB：two", now-5000)
	e.prov.say("main", protocol.RoleUser, "u1", "A：one", now-1000) // already answered
	e.sync()
	row := thingOfThread(e.collect(), e.sessionKey(0, "main"))
	if row == nil || row.Tier != "" || row.Done != wire.ConnectorDoneReplied {
		t.Fatalf("a chat the owner has answered is not waiting on them: %+v", row)
	}
	// and no reply for a chat that cannot be replied to
	if row := thingOfThread(e.collect(), e.sessionKey(0, "notes")); row != nil && row.Tier != "" {
		t.Fatalf("%+v", row)
	}
}

// The owner confirms the text on the button they saw. If a newer reply changes the choices before the tap, the same
// position holds other words: nothing is sent.
func TestInboxTapAfterTheChoicesChangedSendsNothing(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "Next step, pick:\nA：run the tests\nB：skip them", now-2000)
	e.sync()
	key := e.sessionKey(0, "main")
	item := &wire.UIInboxItem{ID: "i1", Extra: map[string]json.RawMessage{"connector": json.RawMessage(`{"thread":"` + key + `"}`)}}
	var v wire.View
	if err := e.h.Call("ui.render", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
		Locale: "zh-Hans", Item: item}, &v); err != nil {
		t.Fatal(err)
	}
	var shown *wire.Action
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "button" && shown == nil {
				shown = c.Action
			}
		}
	}
	if shown == nil || shown.Values[valLast] != "m:m1" || shown.Values[valSum] == "" {
		t.Fatalf("the button must carry what it was drawn for: %+v", shown)
	}
	// a newer reply with different choices arrives before the tap
	e.now.Add(60_000)
	e.prov.say("main", protocol.RoleAssistant, "m2", "Changed my mind, pick:\nA：delete the branch\nB：keep it", e.now.Load())
	e.sync()
	var res wire.ActionResult
	if err := e.h.Call("ui.action", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
		Locale: "zh-Hans", Item: item, Action: actPick, Values: shown.Values}, &res); err != nil {
		t.Fatal(err)
	}
	if sent := e.prov.sent(); len(sent) != 0 {
		t.Fatalf("a tap confirmed for old text sent new text: %+v", sent)
	}
	if res.Toast == "" {
		t.Fatal("the owner should be told the choices changed")
	}
}

// With no choices the page has nothing to add, so the app draws the conversation itself.
func TestInboxItemPageWithoutChoicesLeavesTheBodyToTheApp(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "All done, nothing needed.", e.now.Load())
	e.sync()
	key := e.sessionKey(0, "main")
	item := &wire.UIInboxItem{ID: "i1", Extra: map[string]json.RawMessage{"connector": json.RawMessage(`{"thread":"` + key + `"}`)}}
	var v wire.View
	err := e.h.Call("ui.render", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
		Locale: "zh-Hans", Item: item}, &v)
	var ee *ext.Error
	if !errors.As(err, &ee) || ee.Code != wire.ErrorCodeUnsupported {
		t.Fatalf("want unsupported so the app draws its own body, got %v", err)
	}
}

// Two different texts at the same position of the same reply (a reply edited in place) are two different sends: the
// second must not be taken for the first by the outbox.
func TestInboxPickSendIDFollowsTheText(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	key := e.sessionKey(0, "main")
	item := &wire.UIInboxItem{ID: "i1", Extra: map[string]json.RawMessage{"connector": json.RawMessage(`{"thread":"` + key + `"}`)}}
	pickFirst := func() {
		var v wire.View
		if err := e.h.Call("ui.render", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
			Locale: "zh-Hans", Item: item}, &v); err != nil {
			t.Fatal(err)
		}
		a := v.Sections[0].Items[0].Action
		var res wire.ActionResult
		if err := e.h.Call("ui.action", wire.UIParams{Contribution: InboxItemUI, Placement: wire.PlacementInboxItem, Size: "full", UIVersion: 6,
			Locale: "zh-Hans", Item: item, Action: actPick, Values: a.Values}, &res); err != nil {
			t.Fatal(err)
		}
	}
	e.prov.say("main", protocol.RoleAssistant, "m1", "Next step, pick:\nA：run the tests\nB：skip them", now-2000)
	e.sync()
	pickFirst()
	// the same message id, edited in place, now offers other words at the same position
	e.prov.mu.Lock()
	e.prov.script["main"][0].Blocks = []protocol.Block{protocol.TextBlock("Next step, pick:\nA：delete the branch\nB：keep it")}
	e.prov.mu.Unlock()
	e.now.Add(1000)
	e.sync()
	// the row may be cached by the same last; the confirmed text is what is sent
	pickFirst()
	sent := e.prov.sent()
	if len(sent) != 2 || sent[0].Text != "A：run the tests" || sent[1].Text != "A：delete the branch" {
		t.Fatalf("two different confirmed texts must be two sends: %+v", sent)
	}
}

const tailscaleAsk = "这台机器要先加入网络。请打开 https://login.tailscale.com/a/0a1b2c3d4e5f 批准一下。"

// direct is a notifier of the test's own over given rows, with a log that already exists (so nothing is seeded): the
// guards and the bookkeeping are tested on it without waiting for the program's goroutine.
func (e *inboxEnv) direct(rows *[]cachedRow) *notifier {
	e.t.Helper()
	if err := e.space.Store().PutSetting(context.Background(), pushLogKey, "{}"); err != nil {
		e.t.Fatal(err)
	}
	n := newNotifier(newInbox(e.club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil }, e.push)
	n.rows = func(context.Context) ([]cachedRow, error) { return *rows, nil }
	return n
}

func waitingRow(i int, attn string, at time.Time) cachedRow {
	return cachedRow{attn: attn, thing: wire.ConnectorInboxThing{Thread: fmt.Sprintf("agent:echo:x:row%d", i), Last: "m:1", Sender: "账号",
		Label: "演示账号", At: at, Tags: []wire.ConnectorTag{{Key: "chat", Name: "对话标题"}}}}
}

const inboxOff = `{"demo_echo":true}` // the program's own alerts stay out of a test that drives a notifier itself

func TestInboxAuthRequestIsNeedsYouAndPushedOnce(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", tailscaleAsk, now-1000)
	e.sync()
	key := e.sessionKey(0, "main")
	row := thingOfThread(e.collect(), key)
	if row == nil || row.Tier != wire.ConnectorTierAction || row.Why == "" || row.Read {
		t.Fatalf("a sign-in request is for the owner: %+v", row)
	}
	got := e.waitAlerts(2) // the fake service gives both accounts the same scripted chat
	var n notification
	for _, g := range got {
		if g.ID == pushID(key, "m:m1") {
			n = g
		}
	}
	if n.PushLevel != "P0" || n.Level != "time-sensitive" || n.ID == "" || len(n.ID) > 56 {
		t.Fatalf("alert: %+v", got)
	}
	if !strings.Contains(n.Title, row.Sender) || !strings.Contains(n.Body, row.Label) {
		t.Fatalf("alert should say whose it is: %+v", n)
	}
	for _, leak := range []string{"tailscale", "批准一下", "0a1b2c3d", "Echo"} { // the reply, its link, the chat's title
		if strings.Contains(n.Title+n.Body, leak) {
			t.Fatalf("the alert carries %q of the conversation: %+v", leak, n)
		}
	}
	// more rounds do not alert the same reply again
	e.now.Add(1000)
	e.sync()
	_ = e.space.SetInboxAccount(context.Background(), e.ids[1], true) // any club event runs a round
	e.settle(true)
	if got := e.alerts(); len(got) != 2 {
		t.Fatalf("alerted again: %+v", got)
	}
	// nor does a program started again on the same data
	again := newNotifier(newInbox(e.club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil }, e.push)
	if more := again.pushAttention(context.Background()); more {
		t.Fatal("nothing should be left to alert")
	}
	if got := e.alerts(); len(got) != 2 {
		t.Fatalf("a restarted program alerted again: %+v", got)
	}
}

func TestInboxChoicesAreAlertedToo(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("main", protocol.RoleAssistant, "m1", "接下来怎么办？\nA：现在就重跑\nB：先查原因\n先放着", e.now.Load()-1000)
	e.sync()
	n := e.waitAlerts(2)[0]
	if n.PushLevel != "P0" || n.Level != "active" || !strings.Contains(n.Title, "在等你选") {
		t.Fatalf("alert: %+v", n)
	}
	if strings.Contains(n.Title+n.Body, "重跑") {
		t.Fatalf("the alert repeats the reply: %+v", n)
	}
}

func TestInboxAuthInAChatYouCannotReplyTo(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	e.prov.say("notes", protocol.RoleAssistant, "n1", "Please log in again to continue.", e.now.Load()-1000)
	e.sync()
	row := thingOfThread(e.collect(), e.sessionKey(0, "notes"))
	if row == nil || row.CanReply || row.Tier != wire.ConnectorTierAction {
		t.Fatalf("signing in is the owner's to do even where they cannot reply: %+v", row)
	}
	e.waitAlerts(2)
}

func TestInboxOrdinaryRepliesAreNotAlerted(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	e.prov.say("main", protocol.RoleAssistant, "m1", "已经处理好了，不需要你做什么。", now-3000)
	e.prov.say("notes", protocol.RoleAssistant, "n1", "我已经登录好了，可以继续。", now-2000)
	e.sync()
	for _, th := range e.collect() {
		if th.Tier != "" {
			t.Fatalf("an ordinary reply was marked for the owner: %+v", th)
		}
	}
	e.noAlerts(true)
}

func TestInboxNoAlertForOldOrAnsweredReplies(t *testing.T) {
	e := newInboxEnv(t, inboxOn)
	now := e.now.Load()
	// quiet when the switch went on: comes in read, so no alert
	e.prov.say("main", protocol.RoleAssistant, "old", tailscaleAsk, now-48*3600_000)
	// within a day but past the alert window
	e.prov.say("notes", protocol.RoleAssistant, "stale", tailscaleAsk, now-7*3600_000)
	e.sync()
	e.noAlerts(true)
	// an answered request: the owner's own message is the newest
	e.now.Add(60_000)
	e.prov.say("main", protocol.RoleAssistant, "ask", tailscaleAsk, e.now.Load())
	e.prov.say("main", protocol.RoleUser, "done", "批准了", e.now.Load()+1)
	e.sync()
	e.noAlerts(true)
}

func TestInboxAlertsCanBeSwitchedOff(t *testing.T) {
	e := newInboxEnv(t, `{"demo_echo":true,"inbox_conversations":true,"inbox_push":false}`)
	e.prov.say("main", protocol.RoleAssistant, "m1", tailscaleAsk, e.now.Load()-1000)
	e.sync()
	row := thingOfThread(e.collect(), e.sessionKey(0, "main"))
	if row == nil || row.Tier != wire.ConnectorTierAction {
		t.Fatalf("the row is still for the owner: %+v", row)
	}
	e.noAlerts(false)
	off := newInboxEnv(t, inboxOff)
	off.prov.say("main", protocol.RoleAssistant, "m1", tailscaleAsk, off.now.Load()-1000)
	off.sync()
	off.noAlerts(false)
}

// What already waits when alerts start (an upgrade, the switch turned on) was in the Inbox before: it is remembered,
// not alerted, and only what comes later is.
func TestInboxNoBurstOfAlertsWhenAlertsStart(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	now := e.club.Now()
	rows := []cachedRow{waitingRow(0, attnAuth, now), waitingRow(1, attnChoice, now)}
	n := newNotifier(newInbox(e.club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil }, e.push)
	n.rows = func(context.Context) ([]cachedRow, error) { return rows, nil }
	n.seed(context.Background()) // as the program does when it starts
	if more := n.pushAttention(context.Background()); more || len(e.alerts()) != 0 {
		t.Fatalf("rows waiting at the start were alerted: %+v", e.alerts())
	}
	rows = append(rows, waitingRow(2, attnAuth, now))
	n.pushAttention(context.Background())
	if got := e.alerts(); len(got) != 1 || got[0].ID != pushID(rows[2].thing.Thread, "m:1") {
		t.Fatalf("only the new one should be alerted: %+v", got)
	}
	// a round is the seed too, when the start's own could not be done
	e2 := newInboxEnv(t, inboxOff)
	n2 := newNotifier(newInbox(e2.club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil }, e2.push)
	n2.rows = func(context.Context) ([]cachedRow, error) { return rows, nil }
	n2.pushAttention(context.Background())
	if got := e2.alerts(); len(got) != 0 {
		t.Fatalf("an unseeded first round alerted: %+v", got)
	}
}

func TestInboxFailedAlertIsTriedAgain(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	rows := []cachedRow{waitingRow(0, attnAuth, e.club.Now())}
	n := e.direct(&rows)
	e.pmu.Lock()
	e.pushFail = 1
	e.pmu.Unlock()
	if more := n.pushAttention(context.Background()); !more || len(e.alerts()) != 0 {
		t.Fatalf("a failed alert leaves work for a later round: more=%v %+v", more, e.alerts())
	}
	if more := n.pushAttention(context.Background()); more || len(e.alerts()) != 1 {
		t.Fatalf("the next round sends it: more=%v %+v", more, e.alerts())
	}
	n.pushAttention(context.Background())
	if got := e.alerts(); len(got) != 1 {
		t.Fatalf("and it is sent once: %+v", got)
	}
}

// run comes back by itself after a failure, with no new event from the club.
func TestInboxAlertRetriesWithoutANewEvent(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	rows := []cachedRow{waitingRow(0, attnAuth, e.club.Now())}
	n := e.direct(&rows)
	n.retry = 20 * time.Millisecond
	e.pmu.Lock()
	e.pushFail = 2
	e.pmu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	go n.run(ctx)
	n.kick <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for len(e.alerts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-n.done
	if got := e.alerts(); len(got) != 1 {
		t.Fatalf("want the alert after two failed tries, got %+v", got)
	}
}

func TestInboxAlertsAreCappedPerRound(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	var rows []cachedRow
	for i := range 9 {
		rows = append(rows, waitingRow(i, attnAuth, e.club.Now()))
	}
	n := e.direct(&rows)
	if more := n.pushAttention(context.Background()); !more || len(e.alerts()) != pushMaxEach {
		t.Fatalf("%d alerts in one round (more=%v), want %d and more to come", len(e.alerts()), more, pushMaxEach)
	}
	if more := n.pushAttention(context.Background()); more || len(e.alerts()) != 9 {
		t.Fatalf("%d alerts after two rounds (more=%v), want 9", len(e.alerts()), more)
	}
}

// A row the owner read or answered while an earlier alert of the round was on its way is not alerted: the rows are
// read again before every send.
func TestInboxRowAnsweredDuringARoundIsNotAlerted(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	rows := []cachedRow{waitingRow(0, attnAuth, e.club.Now()), waitingRow(1, attnAuth, e.club.Now())}
	n := e.direct(&rows)
	first := true
	n.push = func(ctx context.Context, a notification) error {
		if first { // while the first alert goes out, the owner answers the second chat
			first = false
			rows[1].thing.Done = wire.ConnectorDoneReplied
		}
		return e.push(ctx, a)
	}
	n.pushAttention(context.Background())
	if got := e.alerts(); len(got) != 1 || got[0].ID != pushID(rows[0].thing.Thread, "m:1") {
		t.Fatalf("the answered row was alerted: %+v", got)
	}
}

// However many events come while the hub is slow, each waiting reply is alerted once and the kicks do not pile up
// (they are one slot; there is one sending goroutine by construction).
func TestInboxManyEventsAlertEachReplyOnce(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	var rows []cachedRow
	for i := range 4 {
		rows = append(rows, waitingRow(i, attnChoice, e.club.Now()))
	}
	n := e.direct(&rows)
	var inFlight, worst atomic.Int32
	n.push = func(ctx context.Context, a notification) error {
		if c := inFlight.Add(1); c > worst.Load() {
			worst.Store(c)
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return e.push(ctx, a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go n.run(ctx)
	for range 50 {
		n.poke(ctx)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(e.alerts()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-n.done
	if got := e.alerts(); len(got) != 4 || worst.Load() != 1 {
		t.Fatalf("%d alerts, %d at once; want 4, one at a time", len(got), worst.Load())
	}
}

// failingStore fails reads of the alert log (or writes), as a database in trouble would.
type failingStore struct {
	store.Store
	failRead, failWrite bool
}

func (f *failingStore) Setting(ctx context.Context, key string) (string, bool, error) {
	if f.failRead && key == pushLogKey {
		return "", false, errors.New("database is locked")
	}
	return f.Store.Setting(ctx, key)
}

func (f *failingStore) PutSetting(ctx context.Context, key, value string) error {
	if f.failWrite && key == pushLogKey {
		return errors.New("database is locked")
	}
	return f.Store.PutSetting(ctx, key, value)
}

// While the log of what was alerted cannot be read, nothing is alerted (it could be a repeat); it is tried again.
// A log that cannot be written still keeps this run from repeating itself.
func TestInboxAlertLogInTrouble(t *testing.T) {
	ctx := context.Background()
	fs := &failingStore{Store: store.NewMemory(), failRead: true}
	club := core.New(core.Options{Registry: provider.NewRegistry(echo.New()), FollowUps: []time.Duration{},
		OpenSpace: func(string) (store.Store, vault.Vault, error) { return fs, vault.NewMemory(), nil }})
	t.Cleanup(club.Close)
	var sent []notification
	rows := []cachedRow{waitingRow(0, attnAuth, club.Now())}
	n := newNotifier(newInbox(club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil },
		func(_ context.Context, a notification) error { sent = append(sent, a); return nil })
	n.rows = func(context.Context) ([]cachedRow, error) { return rows, nil }
	if more := n.pushAttention(ctx); !more || len(sent) != 0 {
		t.Fatalf("alerted without knowing what was alerted before: more=%v %+v", more, sent)
	}
	fs.failRead = false
	_ = fs.Store.PutSetting(ctx, pushLogKey, "{}")
	fs.failWrite = true
	n.pushAttention(ctx)
	n.pushAttention(ctx)
	if len(sent) != 1 {
		t.Fatalf("want one alert, not repeated while the log cannot be written: %+v", sent)
	}
}

// When the program ends in the middle of a round, the alerts not yet started are not sent.
func TestInboxNoAlertAfterTheProgramEnds(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	rows := []cachedRow{waitingRow(0, attnAuth, e.club.Now()), waitingRow(1, attnAuth, e.club.Now())}
	n := e.direct(&rows)
	ctx, cancel := context.WithCancel(context.Background())
	n.push = func(c context.Context, a notification) error {
		cancel() // the program ends while the first alert is on its way
		return e.push(c, a)
	}
	if more := n.pushAttention(ctx); more || len(e.alerts()) != 1 {
		t.Fatalf("want the one alert that was under way and no more: more=%v %+v", more, e.alerts())
	}
}

// Without the hub's notify scope nothing can be sent in this run: the round stops and asks for no retry.
func TestInboxNoRetryWithoutTheNotifyScope(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	rows := []cachedRow{waitingRow(0, attnAuth, e.club.Now()), waitingRow(1, attnAuth, e.club.Now())}
	n := e.direct(&rows)
	tries := 0
	real := hubPush(HTTPClient(), func() hubAPI { return hubAPI{} }) // what the program has when the scope was not granted
	n.push = func(c context.Context, a notification) error { tries++; return real(c, a) }
	if more := n.pushAttention(context.Background()); more || tries != 1 {
		t.Fatalf("more=%v after %d tries; want one try and no retry", more, tries)
	}
}

// A hub that fails every time costs a round a bounded number of tries.
func TestInboxFailuresAreCappedPerRound(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	var rows []cachedRow
	for i := range 12 {
		rows = append(rows, waitingRow(i, attnAuth, e.club.Now()))
	}
	n := e.direct(&rows)
	tries := 0
	n.push = func(context.Context, notification) error { tries++; return errors.New("hub unreachable") }
	if more := n.pushAttention(context.Background()); !more || tries != pushMaxEach {
		t.Fatalf("more=%v after %d tries; want %d and a retry", more, tries, pushMaxEach)
	}
}

// If the seed at the start could not be done, the round that makes up for it remembers only what waited at the
// start: a reply that came since is alerted.
func TestInboxLateSeedDoesNotSwallowNews(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	start := e.club.Now()
	rows := []cachedRow{waitingRow(0, attnAuth, start.Add(-time.Minute))}
	n := newNotifier(newInbox(e.club, nil, func() inboxConfig { return Config{InboxConversations: true}.inbox() }), func() *ext.Hub { return nil }, e.push)
	fail := true
	n.rows = func(context.Context) ([]cachedRow, error) {
		if fail {
			return nil, errors.New("store busy")
		}
		return rows, nil
	}
	n.seed(context.Background()) // fails: the log stays unseeded
	fail = false
	e.now.Add(120_000)
	rows = append(rows, waitingRow(1, attnAuth, e.club.Now())) // came after the start
	n.pushAttention(context.Background())
	if got := e.alerts(); len(got) != 1 || got[0].ID != pushID(rows[1].thing.Thread, "m:1") {
		t.Fatalf("want only the reply that came after the start alerted: %+v", got)
	}
}

// hubPush asks the hub's notify route, with the program's own token, and never repeats it in an error.
func TestHubPushCallsTheNotifyRoute(t *testing.T) {
	const token = "pext_fixture_token_value"
	var gotAuth, gotPath string
	var gotBody notification
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer srv.Close()
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	send := hubPush(HTTPClient(), func() hubAPI { return hubAPI{URL: srv.URL + "/", Token: token} }) // the program's own client
	n := notification{ID: "ensemble-abc", Title: "T", Body: "B", Level: "active", PushLevel: "P0"}
	if err := send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/notify" || gotAuth != "Bearer "+token || gotBody != n {
		t.Fatalf("request: %s %q %+v", gotPath, gotAuth, gotBody)
	}
	status = http.StatusForbidden
	err := send(context.Background(), n)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("a refusal is an error that does not carry the token or the address: %v", err)
	}
	if err := hubPush(HTTPClient(), func() hubAPI { return hubAPI{} })(context.Background(), n); err == nil {
		t.Fatal("with no hub address or token nothing can be sent")
	}
	srv.Close()
	if err := send(context.Background(), n); err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a failed request: %v", err)
	}
}

func TestManifestAsksForNotifyAndTheHubBuild(t *testing.T) {
	var m struct {
		Version string `json:"version"`
		Engines struct {
			Persona  string `json:"persona"`
			Protocol int    `json:"extension_protocol"`
		} `json:"engines"`
		Permissions struct {
			Hub []string `json:"hub"`
		} `json:"permissions"`
		Configuration struct {
			Properties map[string]struct {
				Default any `json:"default"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Permissions.Hub, "notify") {
		t.Errorf("alerts need the notify scope: %v", m.Permissions.Hub)
	}
	for _, keep := range []string{"machines.read", "machines.exec", "browser.run"} {
		if !slices.Contains(m.Permissions.Hub, keep) {
			t.Errorf("lost scope %q: %v", keep, m.Permissions.Hub)
		}
	}
	// v0.6.0 (build 202611160608) is the first stable hub with the catalog capability, fallback, inbox_item pages
	// and push_level; checked against the hub's release index and git history when this was written.
	if m.Engines.Persona != ">=202611160608" || m.Engines.Protocol != 1 {
		t.Errorf("engines: %+v", m.Engines)
	}
	if d := m.Configuration.Properties["inbox_push"].Default; d != true {
		t.Errorf("inbox_push default: %v", d)
	}
	if pac.Version() != "0.7.14" {
		t.Errorf("version %s", pac.Version())
	}
}

// pushAttention keeps its own guards, whatever built the rows: nothing is alerted for a row already read, answered,
// without anything waiting, or too old.
func TestPushAttentionGuards(t *testing.T) {
	e := newInboxEnv(t, inboxOff)
	now := e.club.Now()
	rows := []cachedRow{waitingRow(0, attnAuth, now), waitingRow(1, attnAuth, now), waitingRow(2, "", now),
		waitingRow(3, attnAuth, now.Add(-pushWindow-time.Minute))}
	rows[0].thing.Read = true
	rows[1].thing.Done = wire.ConnectorDoneReplied
	n := e.direct(&rows)
	if more := n.pushAttention(context.Background()); more || len(e.alerts()) != 0 {
		t.Fatalf("alerted for a row that needs nothing: %+v", e.alerts())
	}
	rows = append(rows, waitingRow(4, attnChoice, now))
	n.pushAttention(context.Background())
	if got := e.alerts(); len(got) != 1 {
		t.Fatalf("the one that waits should be alerted: %+v", got)
	}
}
