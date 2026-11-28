# Installing the Persona extension from a local package

This guide installs Ensemble on a Persona hub from a package you build yourself, before it is published
in a market. Everything below runs on the hub's machine, as the user the hub runs as.

## What the package is

`make package` writes `dist/personal-agent-club-<version>.tar.gz` and its `.sha256`. The archive holds one directory,
`personal-agent-club/`, with:

- `persona-extension.json`, and the same manifest as `persona-plugin.json` for hubs from before extensions were
  renamed from plugins;
- `bin/personal-agent-club-<platform>` for `linux-amd64`, `linux-arm64`, `darwin-arm64` and `darwin-amd64`.

There is no source in it. The programs are built with `-trimpath` and without cgo; the packaging script refuses to
pack when a program contains a path of the machine that built it, and the archive's entries carry no owner name or
macOS metadata. The hub needs no Go toolchain or other runtime.

Before installing, check the archive against its hash:

```sh
shasum -a 256 -c personal-agent-club-<version>.tar.gz.sha256   # or: sha256sum -c …
```

To hold the program to the hub's own checks without a hub (manifest and package as at install, handshake, error
codes, every page drawable), run `scripts/persona-test.sh` with a `personad` on `PATH` (or `PERSONAD=/path/to/personad`).

## What 0.7.14 adds

Interactive WebShell Phase 2 integrates the three Persona core terminal capabilities (`nwverse/persona#16741`):
- **Direct native terminal deep link (`persona://terminal/<machine_id>`).** Confirmed paired Persona machines (Muse,
  Grok Bot, echo) show an **打开原生终端 / Open native terminal** button with an explicit Chinese/English `Confirm`
  dialog that navigates straight to `/machines/<id>/terminal`; zero terminal traffic or credentials pass through the
  extension, and overview cards automatically strip terminal navigation actions.
- **Owner-approved `machines.exec` for sandboxed environments without SSH.** `persona-extension.json` declares the
  optional named Hub scope `machines.exec` (granted by name only; every call triggers a Persona owner approval card).
  Paired machines expose a two-step command runner calling `ext.Hub.MachinesExec`, displaying cleaned stdout/stderr,
  truncation, timeout, cancellation, and explicit owner denial feedback.
- **Full interactive PTY WebShell (`contributes.terminal_sessions` + `github.com/creack/pty`).** Ensemble registers the
  `terminal` JSON-RPC namespace (`terminal.open`, `terminal.input`, `terminal.resize`, `terminal.close`) and streams
  ordered `terminal.frame` / `terminal.closed` events over real PTYs:
  - Configured SSH / Tailscale SSH targets (`ssh:<account_id>`) run validated `exec.Command` argv sessions supporting
    `vim`, `tmux`, `htop`, window resize (`SIGWINCH`), and `Ctrl-C`, with zero passwords or private keys written to disk.
  - Local `echo` demo accounts (`echo:<account_id>` / `echo-demo`) provide a reproducible restricted sandbox PTY shell
    for full end-to-end PTY I/O, resize, and teardown verification without external credentials.
  - Enforces strict target validation, a 4-session concurrency cap, 30-minute idle timeout reclamation, and full
    process-group teardown (`SIGTERM` -> `SIGKILL` to `-pgid`) to prevent orphan processes.

Gemini cloud computer Task-mode WebShell (`0.7.12`) and the 5-level assistant control / FollowLease model (`0.7.13`)
remain unchanged and fully compatible.

## What 0.7.12 adds

Gemini accounts have a remote computer status card and confirmed WebShell on the Remote terminal page:
- **Read-only remote computer status.** Gemini's Remote terminal page queries `/BardFrontendService.GetVmSettings`
  (`W8adn`) and `MaZiqc` task counts read-only inside the signed-in browser tab without navigating an active
  conversation or flipping any user setting. It shows whether task mode and remote computer internet access are enabled,
  notes the 2 vCPU / 8 GB RAM / Debian 12 environment and persistent working directory, and links directly to
  `https://gemini.google.com/gemini-spark` and the current cloud computer session. When internet access is off, it shows
  a clear hint and link so the owner can enable it themselves.
- **Dedicated Task-mode WebShell isolated from the main chat.** Confirmed commands run in a dedicated Task-mode
  (`chat-spark-toggle` `data-value="40"`) conversation kept under `localStorage["ensemble.gemini.computer"]`, leaving
  `main` (`data-value="0"`, `ensemble.gemini.main`) untouched. Terminal stdout/stderr is extracted from the task's
  code blocks, stripped of ANSI control characters, capped at 16 KB, and retained in the last 5 runs on the page.
- **Two-step confirmation and allowance guards.** Commands and read-only preset probes (`uname -srm && nproc && free -h
  && df -h`, `ls -la`, `python3 --version && node -v`) go through `validateTerminalCommand`, staging with a one-shot
  SHA-256 digest, the native `Confirm` dialog, quota-exhaustion blocking before consuming the staged command, and
  `Unconfirmed` protection against blind retries.

No new permissions or configuration fields are added.

## What 0.7.11 adds

Mounting repositories on Claude Duet works again:
- **`club_agent_repo_bind` writes where Claude keeps a channel's repositories.** It sent the new list to the channel
  itself (`PATCH /v1/code/channels/<chan>`, which answers 200 and drops `default_source_urls`), so every bind ended
  `unconfirmed` with `mounted: []` and nothing changed. It now writes the channel's config
  (`PATCH /v1/code/channels/<chan>/config` with `default_source_urls` and the added ones as `chosen_source_urls`), as
  Claude's own channel settings do, keeping every repository the channel had, and reads it back.
- **A refusal is a refusal.** Claude refuses a repository the GitHub App was not granted with 403 and the reason in
  `error.resource` (`github_repo_access_denied`); that is `mount_refused` with its reason now, not "signed out".
- **One write per channel at a time.** Two binds on one channel wait for each other even when they run in two
  computers' browsers; each merges from what the channel has when it runs, so neither drops the other's repositories.
- **`club_agent_repos_list` without `channel`** says a repository is mounted when any channel (main chat included)
  has it, and names those channels in `mounted_in`; with `channel` it still checks only that channel.

No new permissions or configuration fields are added.

## What 0.7.10 adds

The account page no longer fails with HTTP 502 「扩展返回的界面无法显示」:
- **The avatar ring selector fits what the hub draws.** Persona's hub holds every page to `wire.CheckView`: a
  `segmented` control has 2–5 choices of at most 12 characters each. The selector titled its choices with the long
  wording (「每 5 小时的时段额度 91%」 on ChatGPT Dots, 「Auto (the lowest)」 in English, Claude Duet's and Gemini's 5-hour
  windows, Groq's tokens), so the page was refused once real readings came in. Its choices are short now (「自动」
  「5小时 91%」「每周点数 40%」 / "Auto" "5h 91%" "Credit 40%" "Req/day 80%"), cut to 12 characters if a service ever
  reports something longer; the bars above keep the full wording. With more than three buckets the selector keeps
  Auto, Off, the one chosen and the ones with the least left, so it never has more than five choices.
- **English Inbox choice.** "Not in the Inbox" (16 characters) is "Not in Inbox", which the hub refused the same way
  once 「把 AI 对话放进收件箱」 was on.
- **Other pages that some data would have refused.** An assistant level this version does not know (written by a
  newer one) showed as a selector value with no choice; it shows as off now. A link is drawn only when it is what the
  hub accepts (lowercase `https://`, a host, no user name). The list of repositories granted to the Claude GitHub App
  is cut to a view's 2000 characters, counting the rest, for an installation with many repositories.
- Every page (home, 账号, add, card, account, terminal, repositories, pairing, Grok Bot sign-in) is now tested against
  `wire.CheckView` in both languages, with every service's real readings, seven buckets, and stale readings.

No new permissions or configuration fields are added.

## What 0.7.9 adds

- **A ChatGPT Dot's own picture.** A Dot whose owner gave it a picture (ChatGPT's `/backend-api/tbo` lists it as
  `avatar_url`, with the file in `avatar_manifest`) now shows that picture instead of the dots cloud, on the account's
  row and page and on Persona's Agents page. Its address is a signed `oaiusercontent.com` link that expires within
  minutes, which the hub could not load later, so the signed-in ChatGPT page itself fetches it once, fits it into
  128×128 and hands it over as a WebP data URL (about 9 KB, at most 16 KB, never past the 64 KB an own picture may
  weigh); the next syncs reuse it while the Dot keeps the same file. A picture at a public https address would be
  passed as it is. A Dot without its own picture, or one that cannot be made, keeps the dots icon.

No new permissions or configuration fields are added.

## What 0.7.8 adds

Persona's Messages page can list the agents' conversations among the others, newest message first:
- **A sync with nothing new moves nothing.** Muse's main chat was ranked at the time of every sync (its row has no
  date) and so rose to the top each time, and its messages, which carry no time, were re-dated with it; a site
  without scripts of its own did the same to every chat. A conversation is now ranked by its newest message (its own
  time, or the time it was first seen) or by the time its service's list really gives; a coarse relative date (Muse's
  side chats: "2h", "今天") only places a conversation seen for the first time. A message already stored keeps its
  time, so no reply is counted unread again. A rank an earlier version raised this way is not lowered (Muse gives no
  message times to correct it from): it stops rising, and the conversation sinks as others get new messages.
- **Muse's picture is a PNG.** It was muse.ai's `favicon.ico` (an ICO, which iOS and some browsers draw unevenly); it
  is now the 180×180 icon Muse's own web app manifest lists (`muse.ai/images/pwa_icons/maskable/180.png`), drawn
  within the manifest's safe zone so the round avatar shows all of it. Every official icon is now a PNG.

No new permissions or configuration fields are added.

## What 0.7.7 adds

Live real-time quota readings for **Muse**, **Gemini**, and **Claude Duet credit**:
- **Muse (`muse.ai`)**: reads real weekly usage (`percentUsed`, exact `resetsAt`, `quotaStatus`) and non-expiring extra credits (`topupBalance` / `topupTotal`) from Settings › General › Usage (intercepting the Next.js server action payload with a DOM fallback, then cleanly closing the settings dialog).
- **Gemini (`gemini.google.com/usage?hl=en`)**: reads live **Current usage** and **Weekly limit** percentages and exact reset times inside an isolated temporary page (`task.newPage()`), leaving the active Gemini chat tab untouched.
- **Claude Duet (`claude.ai/api/organizations/<org>/usage`)**: also reads Claude Duet's dedicated weekly credit (`maple_sundial`, e.g., `$190.87 / $500`) alongside 5-hour and 7-day plan limits, falling back cleanly to plan limits when promotional credit reaches `$0`.

No new permissions or configuration fields are added.

## What 0.7.6 adds

Repositories for Claude Duet, from Persona:
- A Claude Duet account's page has **仓库**: every channel's mounted repositories (the main chat's too; a channel can
  have several), the repositories the Claude GitHub App was granted, and a form to mount repositories on a channel.
  **检查** only reads; **挂载（只挂已授权的）** adds the granted ones to the channel, keeping what it has, and reads it back.
- **去 GitHub 授权并挂载** adds repositories the app was not granted on github.com, in the computer's own browser: only
  the ones named (the others stay selected, a grant is never switched to "All repositories"), then reads the page
  again and mounts them. When GitHub asks you to sign in, confirm access (password, 2FA, passkey), get an organization
  owner's approval or pass a captcha, it stops, changes nothing, and shows the page's direct link, the repositories to
  tick and **重新检测并挂载** to go on afterwards. It never answers those prompts or asks for a password.
- The Persona assistant gets `club_agent_repos_list` (read) and `club_agent_repo_bind` / `club_agent_repo_authorize`
  (writes, each confirmed on the phone with the account, channel, repositories and what changes).

The GitHub step drives github.com through `browser.run`, which Persona does not yet limit by site:
[nwverse/persona#16943](https://github.com/nwverse/persona/issues/16943) asks for declared sites and phone-confirmed
access to settings pages. No new permission, network entry or setting.

## What 0.7.5 adds

Remote terminal & shell entry for each account:
- Every account detail page adds a **远端终端与命令行** entry opening its environment and terminal view.
- SSH-reachable environments (such as **Muse** cloud VMs and **Grok Bot** sandboxes) support saving a non-secret SSH / Tailscale SSH target (no passwords or private keys ever stored), running a fixed read-only status probe (`uname -srm && uptime`), and running commands only after explicit two-step staging + confirmation (`20 s` timeout, `16 KB` output cap, ANSI/control stripping).
- **Grok Bot** reads cloud sandbox run state read-only via `SandBoxService/GetSandBoxRunState` without waking a sleeping sandbox, and both **Muse** and **Grok Bot** also offer a two-step confirmed chat-assisted sandbox runner (respecting the quota exhaustion guard) when SSH is not configured.
- Hosted web/API services without a shell (**Claude Duet**, **ChatGPT Dots**, **Gemini**, **Manus**, **Groq**, **muse2api**) clearly report that no direct terminal is available and document the Persona core interfaces needed for native/embedded webshell support (`nwverse/persona#16741`).

No new permissions or configuration fields are added.

## What 0.7.4 adds

Claude Duet and ChatGPT dots show their own pictures, no longer Claude's and ChatGPT's icons: Claude Duet the icon of
its own app (Claude Duet by Anthropic in the App Store), whole on a transparent margin so the round avatar shows all of
it; ChatGPT dots the dot chatgpt.com's dots page shows, on a gray ground so it reads on light and dark pages alike. Both
are https addresses the apps load without credentials; nothing is added to this repository, and no new permission,
network entry or setting.

## What 0.7.3 adds

Quota & usage across every account:
- Real usage readings are fetched read-only on sync (throttled to every 5 minutes, immediate on manual Sync) for Grok Bot (`DashboardService/GetSandUsageStatus` / `GetCurrentPeriodUsage`), Claude Duet (`claude.ai/api/organizations/<id>/usage`), ChatGPT Dots (`chatgpt.com/backend-api/wham/usage`), alongside Groq's live `x-ratelimit-*` headers and 429 cooldowns.
- Services that do not expose real-time quota numbers (Muse, Gemini, Cue, muse2api) show local 5-hour and 24-hour send-count estimates with their exact data source and estimation method.
- The home page, Overview card, Accounts list and each account's detail page show remaining quota, reset times, per-window progress bars, and low/exhausted badges.
- Sending to an account known to be exhausted stops before hitting the provider (`QUOTA_EXHAUSTED`) with a clear prompt and reset time across Agents, Inbox, Takeover, and assistant tools, instead of failing silently.

No new permissions or configuration fields are added.

## What 0.7.2 adds

Gemini and Claude Duet each have one main chat again, and sending to it works.
Gemini: the main chat is a conversation Ensemble starts in Gemini's 对话 (chat) mode on its first message and continues after;
your other Gemini conversations are its side chats (tasks stay tasks). A send now waits for Gemini's answer before leaving the
page (the new Gemini drops a message whose page is left before the answer is written), and is refused, not lost, while Gemini is
still answering in that conversation.
Claude Duet: the main chat is Duet's own (claude.ai/duet, "Everything"), no longer the most recently active channel; every
channel is a side chat of it. No new permission or setting.

## What 0.7.1 adds

Alerts: a reply that waits for you (choices, or a request to sign in or approve a link) is also pushed to the phone, once.
Replies already waiting at the upgrade are not alerted, only later ones.
This needs the hub scope `notify`, so the hub asks again at upgrade; without it everything else works and no alert is sent.
The setting `inbox_push` (on) turns the alerts off. The manifest also says `engines.persona` `>=202611160608`: the first
stable hub (v0.6.0) with the connector `catalog` capability, `fallback`, `inbox_item` pages and `push_level`, found from the
hub's release index and git history. An older hub will refuse the install, and a development build (number 0) is not checked.

## What 0.7.0 adds

A second connector, `ensemble` (chats as Inbox rows), a page for those rows, and four settings: `inbox_conversations` (off),
`inbox_reply` (on), `inbox_via_subscription` (off) and `triage_rules`. Nothing changes until `inbox_conversations` is turned on.
The hub may ask again about the new connector at upgrade. The connector declares `fallback` (every row carries a tier), so the
hub's "Message triage" model is not required: without one the rows come in as "worth knowing". The chats' text goes to that model
when there is one. See the README, "Chats in Persona's Inbox".

## Back up first

An install or upgrade touches two directories under the hub's state directory (`~/.persona` unless `PERSONA_HOME`
says otherwise):

| Directory | What it holds |
| --- | --- |
| `extensions/installed/personal-agent-club/<version>/` | The unpacked package; an upgrade keeps the current and the previous version |
| `extensions/data/personal-agent-club/` | The extension's own data (below): `club.key`, `club.lock` and `spaces/default/` with `club.db` and `credentials.enc` |

Copy both before an upgrade, keeping modes, for example from the state directory:

```sh
tar -C ~/.persona/extensions -cpzf personal-agent-club-backup.tgz \
  installed/personal-agent-club data/personal-agent-club
```

Treat the copy of the data directory like a
password store, since it holds the key next to what it protects. The hub also keeps the extension's settings and its
record in its database; back that up as you usually back up the hub.

Accounts, sessions, messages, read state and the outbox are kept on disk in the data directory and survive restarts,
upgrades and settings changes; the credentials are kept there encrypted. Quota readings are kept in memory only: after
a restart nothing is shown until a provider's next answer.

## Install

```sh
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

The command first shows what the extension contributes and asks for, then asks to continue (`--yes` skips the
question). It contributes:

- the agent source `pac`, shown as **Ensemble 的账号和对话** ("Ensemble's accounts and chats") among the extension's
  switches (its sessions appear on the Agents page as `pac:<sessionKey>`);
- the home page **我的 AI 助手** ("My AI assistants"): an entry in the navigation (icon `robot`) and a card on
  Overview, **我的 AI 助手 · 概况** (on the phones, above the list in Settings › Extensions) that opens it. The web's sidebar lists only the
  first few extension pages (three) and puts the rest under **更多** ("More"), so with other extensions installed the
  entry may be there; the card always opens it;
- the link **账号** ("Accounts") in **Settings › Extensions › Ensemble**: the same accounts and ways to add one,
  without the home's introduction and state; one line on top says so and that the overview, with what needs you, is
  **我的 AI 助手** in the navigation;
- the connector `club`, shown as **让 Persona 助手使用 Ensemble** ("Let the Persona assistant use Ensemble"), which
  carries the assistant's `club_agent_*` tools;

and asks for network access to `api.manus.ai` (Cue, the agents of a Manus account), `api2.cursor.sh` (Grok Bot) and
`api.groq.com` (Groq), and to the address you write in the setting `muse2api_url` (muse2api; nothing until you write
one). The settings page draws these permissions in the hub's own words: the network entries as the bare host names
(the manifest's `permissions.network` holds host names only, with no label) and `agent_runtimes` (the main
assistant bridge) in the hub's description of that permission (which names the node's `runtime.sock`); the manifest has only
`"agent_runtimes": true`, so none of that text comes from it.

**No hub restart is needed.** The hub starts the extension right after the install (it runs from startup on, as
agent sources must) and the source appears on the Agents page within seconds. `personad extension list` should show
it as `running`. Changing a setting restarts only the extension, not the hub.

To try it without any account, switch on the demo provider, which answers every message with its own text and
reaches nothing:

```sh
personad extension config personal-agent-club demo_echo=true
```

Then add a "演示账号" (demo account) on the home page with any text starting with `echo-`.

To use Muse through your own [muse2api](https://github.com/czg86389-hub/muse2api) server, write its address (http or
https; a server on the hub machine itself is `http://host.persona.internal:<port>`) and, if you like, the model:

```sh
personad extension config personal-agent-club muse2api_url=http://<host>:18610
personad extension config personal-agent-club muse2api_model=muse-spark
```

Then add "Muse 自建转接" (Muse, own relay) on the home page with the server's API key (`MUSE2API_KEY`).

The accounts through a computer's browser (Muse on the web, Claude Duet, Gemini, ChatGPT Dots) are on by default
since 0.6.0 (`browser_providers`; nothing is reached until one is added on a computer). To switch them off:

```sh
personad extension config personal-agent-club browser_providers=false
```

## Home page

The extension's pages are written for someone who has never read this guide; their copy is Chinese (English for an
English-language app) and avoids the program's own terms. The manifest itself (name, description, settings) is
written in Chinese, since the hub shows a manifest's own language and the package carries no translations.

The Overview card (**我的 AI 助手 · 概况**, "My AI assistants at a glance", so that the extension's settings page
does not list two contributions named alike) shows where things stand at a glance, without scrolling: how many accounts are
connected, how many need you (naming the first), how many the Persona assistant may use, and the one next step
(connect the first account, fix the broken one, let the assistant use them, or, when nothing needs you, where the
chats are). With no account it says so and offers to connect the first one. Tapping it opens the home.

The home (**我的 AI 助手**) shows, top to bottom:

- what the extension is for, in two sentences;
- **现在的状态** (where things stand): how many accounts are connected, how many need you, how many the Persona
  assistant may use, and the one next step: connect the first account, fix a broken one (it opens that account), or
  let the assistant use them through **助手指挥**;
- **已接入的账号** (your accounts): each with its service, its state in words and what the assistant may do with it;
- the ways to add an account, under one line (**怎么选**) naming every way the page offers, drawn from the same lists
  as the rows (the self-hosted Muse relay last, as a choice only for those who run one), grouped by how one connects:
  - **在浏览器里登录** (sign in in your browser): **Grok Bot**. Start the sign-in, approve on the page that opens in
    your browser, and the page checks the result every 30 seconds (or at once with *check now*).
  - **借用电脑上已登录的网页** (a website signed in on your computer): **Muse 网页版**, **Claude Duet**,
    **Gemini** and **ChatGPT Dots**, through the browser Persona drives on a computer connected to it (ego lite;
    pairing and every read run in a task space of its own inside that browser, with its sign-ins). The page lists
    your computers and says to sign in to the site in that browser first. With the setting `browser_providers` off, the rows and pages say
    so and where to switch it on.
  - **粘贴密钥** (paste a key): **Cue**, **Groq** and **Muse 自建转接** (your own muse2api server, which needs
    `muse2api_url` first; the row says so until it is written). The key field is a secret field: the apps never show
    it, the extension cannot fill it in, and the extension never shows, returns or logs a saved key.
  - **先试试** (try it first): the demo account, with `demo_echo` on.

An account's page starts with its state (and, when it needs you, what to do, *重新检查* / check again and, for a Grok
Bot account, *重新登录* / sign in again), then:

- the name field (**名字**): rename it (shown only in Persona);
- **助手指挥** (assistant control): *关闭* (off, the default), *只读* (read), *可代发（逐次确认）* (confirm), *持续跟进* (follow: chats with an active follow-up lease below can continue turns within their turn and time caps) or *直接放行* (auto: requires an explicit secondary confirmation card and is guarded by allowance pre-check, unconfirmed-send protection, per-chat burst limits, and mandatory phone confirmation for repository tools);
- **主助手** (main assistant): the account itself answering as a Persona main assistant. With
  `assistant_runtime` off the page says to switch that on instead of offering a switch that does nothing;
- syncing (sync now, pause, resume; for a Grok Bot account that works, *重新登录* waits here with a line saying it is
  only for when its sign-in stops working), which allowance the avatar shows (only a sentence while the service has
  reported none), and for Grok Bot and Muse an advanced part that connects the agent's own cloud computer to Persona;
- removing it, keeping or deleting its chats.

Times read the same on every page, in the viewer's time zone: within 7 days relative (*刚刚*, *5 分钟前*, *3 小时前*,
*3 天前*; an allowance that comes back *3 小时后*), older as month, day and time (*11月14日 12:04*), with the year only
when it is not this year's.

Every change says what it changed (*重新检查* reads the account back and says whether it works now), and a refusal says what to do next, in words rather than error codes.

## The assistant's tools

The connector `club` (**让 Persona 助手使用 Ensemble** in Settings › Connectors) gives the assistant `club_agents_list`, `club_agent_read`,
`club_agent_send`, `club_agent_start` and `club_agent_interrupt`, and for an agent with repositories (Claude Duet)
`club_agent_repos_list`, `club_agent_repo_bind` and `club_agent_repo_authorize`:

- An account the assistant may use is chosen on its page under **助手指挥** (assistant control): *关闭* (off, the
  default for every account: the assistant does not see it), *只读* (read: list and read), *可代发（逐次确认）*
  (confirm: writes ask on your phone first), *持续跟进* (follow: continues turns in a chat holding an active `FollowLease` within its turn and TTL caps, degrading to confirm elsewhere), or *直接放行* (auto: writes without asking each time, subject to safety guards; see `docs/permission-model.md`).
- Repository authorization and binding (`club_agent_repo_authorize`, `club_agent_repo_bind`) are listed in the manifest's
  `confirm_always`, so they are always confirmed on your phone whatever the control level or gate rules say.
- Starting a new session and interrupting a turn depend on the provider; where they are not available the tool says
  so before anything is asked.
- The repository writes need *可代发* like a message. `club_agent_repo_bind` only mounts repositories the Claude GitHub
  App was granted; `club_agent_repo_authorize` also adds the missing ones on GitHub (only those). Either answers
  `needs_github` with the direct link and the repositories to tick when GitHub needs the user.

## Sandbox and network

A package installed from a file runs **in the hub's sandbox** by default: it reads and writes only its own package and
data directory, and reaches the network only through the hub's egress proxy, which lets through only the hosts in
`permissions.network`. The hub hands the proxy to the program in `HTTPS_PROXY` / `HTTP_PROXY`, and the program's HTTP
client takes its proxy from there. Check the counters after the first sync:

```sh
personad extension show personal-agent-club   # "sandbox": {"active": true, "egress": {"allowed": …, "denied": …}}
```

- `denied` growing with `last_denied` naming a host means a provider reached somewhere the manifest does not list:
  report it rather than widening anything by hand.
- Keep the sandbox on. Turning it off (`personad extension sandbox personal-agent-club off`) lets the program read and
  write whatever the hub's user can, and the network list is then only shown, not enforced.
- On Linux the sandbox needs `bwrap` (bubblewrap) and, for memory and CPU limits, user systemd. Without `bwrap` the
  extension still runs, pointed at the proxy but not held to it, and its details say "No sandbox".
- If the hub itself must go through a proxy to reach the internet, set `HTTPS_PROXY` for the hub; the egress proxy
  goes out through it and the extension needs nothing more.
- Pictures need no network entry: an account's avatar is its own picture when the service gives one (a Grok Bot's
  bot avatar; a ChatGPT Dot's picture, which the signed-in ChatGPT page in your browser fetches and scales itself),
  else its product's official icon, else its letters (internal/names/avatar.go). The icons are https
  addresses of PNGs the products publish (muse.ai's web app manifest icon, manus.im, groq.com, Google's branding path on www.gstatic.com for Gemini;
  for Grok Bot and Claude Duet the App Store artwork of each one's own app, whole on a transparent margin; for
  ChatGPT dots the dot chatgpt.com's dots page shows, from its image CDN images.ctfassets.net, padded on a gray ground)
  that the Persona app loads itself on the pages, and the hub fetches as a conversation's `avatarUrl` on the Agents
  page; the program never fetches one, and no address carries anything of your accounts. The self-hosted Muse relay
  is not the Muse app and shows its letters.

## Stopping just this extension

```sh
personad extension disable personal-agent-club   # its program stops; the hub and other extensions keep running
personad extension enable personal-agent-club    # started again with the same settings and data
```

`disable` keeps the settings and the data directory; the `pac` source leaves the Agents page until `enable`. Work
on the data directory while the program is stopped (an offline import, a restore) goes between the two. A hub
restart starts the extension again by itself.

## The data directory

The hub gives the program `PERSONA_EXTENSION_DATA`, which is `<state>/extensions/data/personal-agent-club/`, where
`<state>` is the hub's state directory (`~/.persona` of the user the hub runs as, or `PERSONA_HOME`). The program keeps
there:

| Path | What |
| --- | --- |
| `club.key` | the key the credential files are sealed with (AES-256-GCM), 0600; created at the first start and never rewritten |
| `club.lock` | held while the program runs: one process owns the directory at a time |
| `spaces/default/club.db` | the store of the one space this shell serves: SQLite (with its `-wal` and `-shm` files while it runs) |
| `spaces/default/credentials.enc` | that space's credentials, sealed with `club.key` |

This is the data directory layout `personal-agent-club serve` uses, with the key under the name `club.key`, so the
command's tools work on it while the extension is disabled: `personal-agent-club import -data <dir> -key-file
<dir>/club.key BUNDLE.json` imports a space exported from the Lark gateway ([import-bundle.md](import-bundle.md)), and
`personal-agent-club accounts -data <dir>` lists the accounts read-only (that one also works while the extension
runs). An import while the extension runs is refused (the lock), as is a second copy of the program. A key file that
does not open `credentials.enc` stops the program at start, and nothing is overwritten: keep `club.key` with the rest.

To see the path a running program actually has, on Linux:

```sh
pid=$(pgrep -f 'extensions/installed/personal-agent-club/.*/bin/personal-agent-club-' | head -1)
tr '\0' '\n' </proc/$pid/environ | grep '^PERSONA_EXTENSION_DATA='
```

Print only that line: the same environment holds the program's proxy credential.

Files put there by anything else (a restore, an import tool) must be owned by the user the hub runs as and be
readable and writable by that user; `0600` files in `0700` directories are what the extension writes itself. The
sandbox lets the program read and write everything under its data directory and nothing above it, and it does not
change owners or modes: a file left owned by root, or another user, cannot be opened by the program. Run such tools
as the hub's user while the extension is disabled, then enable it.

## Upgrade

```sh
personad extension upgrade personal-agent-club file:/path/to/personal-agent-club-<newer version>.tar.gz
```

The version must be higher than the installed one. The hub stops the old program and starts the new one, keeping the
settings and the data directory; when the new one does not come up within 30 seconds it goes back to the old one by
itself. An upgrade that asks for more (another host, another contribution place) shows what grew and asks again. No
hub restart.

A package with the **same version** as the installed one cannot be upgraded to. To run its programs anyway, remove
the installed one keeping its data, then install again; this drops the extension's settings (set `demo_echo` again
if you used it), not its data directory:

```sh
personad extension uninstall personal-agent-club --keep-data
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

## Roll back or remove

```sh
personad extension rollback personal-agent-club                 # back to the version before the last upgrade (one step)
personad extension disable personal-agent-club                  # stop it; settings and data stay
personad extension uninstall personal-agent-club --keep-data    # remove it but keep the data directory
```

`uninstall` without `--keep-data` deletes the data directory, key file included. To go back to a state you backed up,
uninstall with `--keep-data`, put the saved data directory back in place, and install the matching package again.
