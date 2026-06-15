# Nexus Operator Toolkit (`nexus`)

> Operate and observe your Nexus Gateway from the terminal — a **chat agent** and a
> **live console** in one screen. Ask it questions in plain English, watch it drive the
> views as it answers, and run the same triage you'd do in the web Control Plane —
> governed by exactly the same IAM, because everything goes through the same admin API.

One static Go binary, two faces over one core: an interactive **TUI** (the console)
and a scriptable **CLI**.

---

## Quickstart

```bash
# 1. Build it (from packages/nexus-cli)
go build -o bin/nexus ./cmd/nexus

# 2. Point it at a deployment (a built-in `local` target works out of the box)
./bin/nexus setup           # interactive: Control Plane URL, AI Gateway URL, prod flag
./bin/nexus login           # browser OAuth2 + PKCE  (or: login --admin-key for a bot)

# 3. Launch the console
./bin/nexus                 # no subcommand → the TUI
```

First launch runs a short wizard (pick environment → sign in → pick a model → pick or
create a Virtual Key). After that, `nexus` goes straight to the console.

---

## Your first five minutes

1. **Just ask.** The cursor starts in the chat at the bottom. Type
   *"what's my most expensive provider today?"* and press `enter`. The agent reads live
   gateway state, answers, and — when it helps — **opens the relevant view above** so you
   see it work. Its thinking streams in a dim style above the answer; the answer renders
   as markdown.
2. **Keep typing.** You don't have to wait — send another message mid-answer and it
   queues. `esc` interrupts a running turn.
3. **Jump to a view.** Press `/` for the command palette and pick `cost`, or just press a
   number key. `tab` moves focus up to the view (and back to the chat).
4. **Drill in.** In a list view, `↑/↓` to move, `enter` to open a row's detail, `←` or
   `esc` to go back.
5. **Lost? Press `/help`** for the full keys-and-commands cheat sheet, right in the chat.

---

## The console at a glance

The screen is split: an **interactive view on top**, an **always-on chat at the bottom**.
`tab` switches which one has the keyboard — the focused half grows (the chat takes ~80%
when you're chatting, since that's where your attention is; the view keeps a sliver you can
`tab` back into). A **prod** environment wears a persistent red banner and a reddened
location badge so you always know which deployment you're touching.

---

## Keyboard cheat sheet

| Anywhere | |
|---|---|
| `tab` | switch focus between the chat and the view above |
| `/` | open the command palette |
| `1`–`9` | jump to a view |
| `←` / `esc` | back one level (close a detail, then up the trail) |
| `q` | quit (when a view is focused) · `ctrl+c` quit anytime |

| In the chat | |
|---|---|
| `enter` | send (you can keep typing while it works — messages queue) |
| `↑` / `↓` | scroll the transcript (no Fn needed on a Mac) |
| `PgUp`/`PgDn` (`fn+↑`/`fn+↓` on a Mac) | scroll a half page |
| `esc` | interrupt the running turn |

| In a list view | |
|---|---|
| `↑` / `↓` | move the row cursor |
| `enter` | open the row's detail |
| `←` / `esc` | back |
| `f` | filter the view (where supported) |

Typing is IME-aware, so a non-English input method composes cleanly at the cursor without
misfiring a shortcut.

---

## Talk to the agent (the fun part)

The bottom chat is a real agent over your gateway. It can answer and it can *act*:

- *"what's failing right now?"* → it checks alerts/errors and opens the view.
- *"why is spend up this week?"* → it pulls cost by provider and explains.
- *"disable the provider with the worst p95"* → it proposes the change and **pauses for
  your authorization**. On a prod environment it shows an Allow/Deny prompt (defaulting to Deny)
  before anything is applied — the same guard the manual controls use.

It runs through your selected model and Virtual Key, so your gateway's own compliance
policy applies to what it sends. Switch its model any time with `/model` (a name, or
`/model` alone to pick from the catalog).

---

## The views

`Overview` (Mission Control cockpit) · `Radar` (live traffic) · `Event` (one request in
full, with replay) · `SLO` (latency/availability per provider) · `Cost` (spend, burn-rate,
cache ROI) · `Chat` (a raw model playground with A/B compare) · `Lab` (traffic generator +
request lab + routing dry-run) · `Kill` (kill switch + emergency passthrough) · `Alerts` ·
`Nodes` (fleet + config drift) · `Compliance` (block/TLS/hook KPIs) · `Jobs` · `Sync` ·
`Models` (catalog + pricing) · `Keys` (Virtual Keys: revoke/rotate) · `Rules` (routing
rules: inspect config, toggle).

Every list with per-record detail drills with `enter` and returns with `←`/`esc`. Views
poll on a throttle and keep the last good data with an inline note if a refresh blips, so a
transient error never blanks the screen.

---

## Slash commands

`/` on an empty chat prompt opens a fuzzy palette:

- `/<view>` — open any view (`/cost`, `/nodes`, `/alerts`, `/chat` the raw playground, …)
- `/resource` — browse **any** admin kind (pick a kind → list → drill a record), all local, no LLM
- `/model [name]` — switch the chat model
- `/event <id>` — open a traffic event by id · `/sessions` — resume a past conversation
- `/clear` — reset the conversation (and the agent's context) · `/compact` — condense older turns
- `/context` — context-usage breakdown · `/verbose` — expand tool I/O
- `/env` — switch environment · `/login` / `/logout`
- `/help` — the full keys-and-commands reference

---

## Cool things to try

- **Ask, then watch it navigate.** *"show me the slowest provider"* — the SLO view opens
  and drills to that provider.
- **Replay a bad request.** Open an event (`/radar` → `enter`), press `r` to re-fire the
  captured request through the pipeline under your own key.
- **Flush the cache, safely.** In `Cost`, press `f` — on prod you'll be asked to Allow/Deny it first.
- **A/B two models.** In the `Chat` view, `/compare <model>` fans each prompt to two models
  side by side and flags the faster reply.
- **Take a rule out of the path.** `/rules` → `enter` to read its config → `t` to disable —
  a one-keystroke incident action.

---

## Scripting

Every capability is also a CLI command (`nexus health`, `nexus cost --group provider`,
`nexus traffic ls`, …) with `--output json` for a stable shape and distinct exit codes for
branching.

---

## Learn more

- **Full reference:** [`docs/users/features/operator-toolkit.md`](../../docs/users/features/operator-toolkit.md)
- **Architecture:** [`docs/developers/architecture/nexus-operator-toolkit-architecture.md`](../../docs/developers/architecture/nexus-operator-toolkit-architecture.md)
