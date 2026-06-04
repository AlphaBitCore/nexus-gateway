# Control Plane UI — Chat with Nexus (web assistant)

"Chat with Nexus" is a floating assistant available on every Control Plane page. It
answers questions about the running system in plain language and, with your
approval, performs operational actions on your behalf. It is the web counterpart of
the `nexus` operator CLI/TUI: the same agent, reached from a chat popup instead of a
terminal. It is available to signed-in administrators only, and every action it takes
runs under your own identity and permissions — it can never do anything you could not
do yourself in the UI.

## The assistant widget

**Purpose.** Ask about system state and request actions without hunting through pages.

**What you see.** A round button in the bottom-right corner opens a chat popup (on a
phone-sized screen the popup takes the full screen). You type a question; the reply
streams in as it is written. A **maximize** control in the popup header expands it from
the default corner card to a near-fullscreen panel (and back) when you want more room for
a long answer or a wide table; the size is your choice and the default stays compact.
Closing the popup cancels any in-progress reply. Pressing `Esc` closes the popup; the
input is focused when it opens.

**How replies stream.** As the assistant works it streams the written answer along with
a row of **tool-activity chips**. Each chip names a read or action the assistant ran and
shows its status — running, succeeded (✓), or failed (✗) — so you can see exactly what it
consulted to produce an answer. Click a chip to expand it and see the exact input the
assistant ran (for example, the query it searched). The answer renders as **Markdown** — headings, lists,
tables, links, and code blocks are formatted — and appears with a smooth typing
animation as it streams in. An error is shown as its own message rather than silently
dropping the turn.

**Key concepts.** The assistant reads the gateway through the same admin APIs you use
(health, traffic, cost, alerts, nodes, kill-switch and passthrough state, and more) and
can run a small set of operational **actions** — engaging or disengaging the kill
switch, engaging or disengaging emergency passthrough, enabling or disabling a provider
or routing rule, revoking a virtual key, and flushing the gateway config cache. Reads
happen automatically; every action requires your explicit approval (see *Approving
actions*).

## Navigation

When a question is better answered by a page than by text, the assistant can take you
there — for example to the Traffic, Analytics, or Infrastructure pages. It navigates
only to known Control Plane pages; it can never send you to an arbitrary or external
address. After it navigates, the chat popup collapses so the page is unobstructed.

## Approving actions

**Purpose.** No action that changes system state runs without your confirmation, shown
with the exact operation laid out for you.

**What you see.** When the assistant wants to perform an action, the chat shows a
confirmation card with the exact action, its structured parameters, and the reason —
all rendered by the server from the resolved request, never free text the model could
craft. You choose **Allow** or **Deny**. Denying returns control to the conversation and
nothing is changed.

**Impact preview.** For the highest-impact actions — the kill switch, global emergency
passthrough, and revoking a virtual key — the card also shows an **impact preview**: the
current state and what the action would change (for example, "Halts TLS bumping on every
node, fleet-wide"), and a marker when the action is irreversible. The preview is read
only and is shown before you can Allow. If the current state cannot be read at that
moment, the card says so and still lets you proceed — an emergency control is never
blocked by a transient read failure.

**Production second confirm.** On a production deployment, a single Allow is not enough
for an action: after the first Allow the card asks you to **confirm once more** before the
action runs. This second step is enforced by the server, so an accidental or scripted
single click cannot change production. Denying at either step aborts. If the
confirmation has expired (for example, the request sat too long), the chat tells you the
action did **not** run and to re-issue it, rather than leaving you unsure.

## Conversation history

**Purpose.** Return to an earlier conversation or start a clean one.

**What you see.** A history panel lists your own past conversations, newest first. You
can open one to reload its transcript, delete one, or start a new chat. History is
private to you — you only ever see your own conversations.

## Model selection

A searchable model selector in the popup header — grouped by provider — lets you choose
which model answers; the best available model is pre-selected and you can switch it
mid-conversation. By default the
dropdown offers **every active chat model the assistant's backend key can route** (the
reachable models filtered to the catalog's enabled chat models) — no operator list to
maintain — and the pre-selected default automatically falls back to the best available
model if the configured one is not routable. An operator can still pin a narrower list if
they want to constrain the choice. The dropdown is hidden when only one model is
available.

## Files

Some answers produce a file (for example an exported summary the assistant wrote to its
sandbox). When that happens, a download button appears inline in the reply. Files are
private to you and are downloaded through the Control Plane; expired files report that
they are no longer available.

## Where the data comes from

The widget (`ChatWithNexus.tsx`) talks to the Control Plane assistant endpoints through
`streamChat.ts`: `streamChat` (the streamed `POST /assistant/chat` turn), `confirmDecision`
(`POST /assistant/confirm` for Allow/Deny and the production second confirm),
`listSessions` / `getSession` / `deleteSession` (the conversation history,
`GET`/`DELETE /assistant/sessions`), `downloadFile` (`GET /assistant/files/:id`), and
`listModels` (`GET /assistant/models`). All endpoints require a signed-in admin session;
no separate permission is added — each action the assistant runs is permission-checked
at the admin API it calls, exactly as if you had performed it in the UI.

## Key concepts

- **Acts as you.** Every read and action the assistant performs runs under your
  identity and is subject to your permissions; a permission you lack produces the same
  denial it would in the UI.
- **Approval gate.** Reads are automatic; state-changing actions always require explicit
  approval, with the exact operation and (for high-impact actions) an impact preview
  shown first. Production adds a server-enforced second confirmation.
- **Private to you.** Conversations, memory, and files are isolated per administrator;
  you never see another user's assistant data.
- **Audit trail.** Actions the assistant performs on your behalf are recorded
  distinguishably from actions you take directly in the UI, so the audit log shows which
  changes were AI-assisted.

## References

- Operator runbook: `docs/operators/ops/runbooks/e90-web-assistant.md`.
