# Runbook — "Chat with Nexus" web assistant (E90)

Operational guide for deploying and troubleshooting the Control Plane web assistant
(the browser face of the CLI operator agent). Covers the SSE transport, session
affinity, required configuration, graceful-degradation modes, and a deploy self-check.

## What it is
A floating chat widget in the Control Plane UI that runs an agent turn server-side:
the agent self-calls admin APIs **as the calling user** (no privilege escalation) and
streams its work back over Server-Sent Events. Inference runs through the AI Gateway
under a backend **system Virtual Key** that is never sent to the browser.

All users' assistant inference flows through this one system VK, so its inference cost
is a **platform cost on `NEXUS_ASSISTANT_SYSTEM_VK`, not attributed per-user** (an
accepted design decision). Per-user inference rate-limit / token / cost ceilings are
**not** implemented; assistant inference is bounded by the system VK's own gateway-side
rate-limit/quota, per-session turn serialization (HTTP 409 `turn_in_progress`), the
wall-clock turn deadline, and the agent step cap. Per-user admin **actions** are still
IAM-scoped and audit-stamped (`via=assistant`); only the inference *cost* is pooled.

Those admin self-calls are dispatched **in-process** (no loopback HTTP hop): they run
through the same `AdminAuth → IAM → audit` middleware as any admin request, the
originating user's IP is preserved for the audit actor, and the AI-initiated audit
marker (`via=assistant`) rides as an unforgeable in-process signal — a client cannot
forge it, and any inbound `X-Nexus-Initiated-By` header is stripped at the ingress
edge. Inference calls to the AI Gateway (a different host) still use the network.

Endpoints (all under `/api/admin/assistant/`, behind `AdminAuth`, login-only — no new
IAM action; every tool is IAM-checked at the admin API it self-calls). A turn is
**started** by a command POST and **observed** over a separate long-lived SSE stream
(the command/data-stream split), so a dropped connection can reconnect without losing
the turn:
- `POST /sessions/:id/chat` — start a turn (runs detached; returns 202). Body:
  `message`, optional `model`. The `:id` is a client-chosen session id (a fresh id
  starts a new conversation; an owned id continues it). A second chat while a turn is in
  flight for the same session is **409** (no concurrent turns per session).
- `GET /sessions/:id/stream?lastSeq=N` — the SSE stream (long-lived). Reconnect with
  `lastSeq` to replay only missed events; each frame carries `id:` (the seq).
- `POST /sessions/:id/interrupt` — stop the in-flight turn (the Stop button); 204/409.
- `POST /confirm` — deliver an Allow/Deny for a parked confirm-tier write.
- `GET /sessions`, `GET /sessions/:id`, `DELETE /sessions/:id` — session history.
- `GET /files/:id` — download a sandbox file (CP-proxied, owner-checked).
- `GET /models` — the default + allow-listed selectable models.

A detached turn survives a brief stream disconnect (a ~30s grace window) so a reconnect
resumes it; with no reconnect in the window the turn is cancelled to bound system-VK
billing. A deliberate Stop / closing the widget interrupts immediately. The SSE stream
(`GET .../stream`) is long-lived and unbuffered — the same Nginx/ALB no-buffering +
raised idle/read-timeout guidance below applies to it.

## Configuration (env — secrets never in yaml)
| Variable | Required | Purpose |
|---|---|---|
| `NEXUS_ASSISTANT_SYSTEM_VK` | **yes** (for inference) | backend system Virtual Key used only for the assistant's LLM calls; never exposed to the browser. Absent → `/chat` returns 503 "assistant inference is not configured". |
| `NEXUS_ASSISTANT_MODEL` | no (default `claude-sonnet-4-6`) | default inference model. **Robust fallback:** if the configured default is not routable by the system VK, the picker resolves the default to the best available routable chat model instead of breaking. |
| `NEXUS_ASSISTANT_MODELS` | no | comma-separated allow-list of client-selectable models. **Empty (recommended) → the picker AUTO-DERIVES every chat-type model the system VK can route, grouped by provider (no list to maintain).** Set it only to pin a narrower allow-list; in that explicit mode a client-requested model not on the list silently falls back to the default. |
| `NEXUS_ASSISTANT_DISABLE_BODY_READS` | no | `1` withholds the raw-body read tools (`observe_traffic_event` / `observe_traffic_list` / `resource_read` / `resource_invoke`) entirely — the §8 governance posture for deployments that do not want raw traffic bodies reachable by the assistant. Aggregate analysis tools stay. |
| `NEXUS_ASSISTANT_TURN_DEADLINE` | no (default `10m`) | wall-clock backstop on a single turn (Go duration, e.g. `10m`). A hung upstream past this is stopped with a `turn_deadline` SSE error rather than wedging forever. **Keep it below the ingress `proxy_read_timeout` / LB idle timeout** so the clean error fires before the proxy severs the stream. |

Persistence reuses the shared **spill** backend (`spill:` yaml — `localfs` locally,
`s3` in prod) and the Postgres database. The CP and the other services **must point at
the same spill backend root** so the retention sweep is consistent (see Degradation).

## Session affinity (REQUIRED for multi-replica) — current limitation
The confirm registry that pairs a parked write (inside the long-lived `/chat` SSE
request) with its `POST /confirm` decision is **in-process, per-pod**. Therefore, until
the P2b owner-registry + reconnect lands, a multi-replica deployment **must pin a
user's `/chat` and `/confirm` requests to the same pod**:
- **Nginx**: `hash $sessionId consistent;` on the upstream (consistent-hash on the
  session id — stable when replicas are added/removed; do NOT use `ip_hash`, which
  collapses behind a shared egress/NAT), plus `proxy_http_version 1.1;`,
  `proxy_buffering off;`, and a `proxy_read_timeout` **longer than the turn deadline**
  (default 10m — see below), e.g. `proxy_read_timeout 660s;`. The proxy must outlive
  the backstop so the user gets the clean `turn_deadline` SSE event instead of a raw
  proxy 504.
- **ALB / cloud LB**: enable target-group stickiness and raise the idle timeout above
  the turn deadline (default 10m); disable response buffering.
- OR run a single CP replica for the assistant surface.

**Safety net (automatic).** With a shared Redis, the assistant records which CP
instance owns each session (`assistant:owner:<user>:<session>`, TTL > the turn
deadline). A `/confirm` that the LB misroutes to the wrong instance — the parked
confirm channel lives only on the owner, in memory — is answered **`421
Misdirected Request`** (`wrong_owner`) instead of a confusing `409 expired`, so a
421-aware LB / client retries at the owner. This catches the transient misroutes
during pod scale events; it is a backstop, not a replacement for the stickiness
above. No Redis (single replica) → the registry is disabled and no 421 is issued.

Without affinity, a `POST /confirm` that lands on a different pod returns **409**
("no pending confirmation for that call") and the write fails safe (never executes).
This is a safety property, not data loss — but it breaks the confirm UX. Likewise,
multi-turn `sessionId` continuation and session history are DB/spill-backed (so they
work across pods), but an in-flight turn's confirm is pod-local.

## SSE transport
`/chat` IS the SSE stream. The handler sets `Content-Type: text/event-stream`,
`Cache-Control: no-cache`, `Connection: keep-alive`, and `X-Accel-Buffering: no`.
Any reverse proxy in front of the CP must **not buffer** the response:
- nginx: `proxy_buffering off;` for the assistant location (the `X-Accel-Buffering: no`
  header already requests this), and a read timeout long enough for a full turn.
- Load balancers: disable response buffering / raise idle timeouts on `/chat`.
A client disconnect cancels the turn server-side (stops the agent + system-VK billing).

## Graceful degradation (no hard failures)
- **No database** (`d.DB == nil`): the assistant runs with in-memory stores — chat
  works, but sessions/memory/files do not persist. No startup panic.
- **No spill backend wired**: session transcripts + files do not persist (memory
  facts still persist if DB is present). Chat still works.
- **Content expired under spill retention**: the shared spill sweep reaps blobs by
  age (default ~30d). If a transcript/file blob is reaped while its DB row survives,
  reads degrade — `GET /sessions/:id` / multi-turn `Load` return an **empty** session;
  `GET /files/:id` returns **410 Gone**. Conversation/file data is therefore subject to
  the shared retention horizon; size a dedicated retention/prefix if longer-lived
  history is required (tracked).

## Deploy self-check
1. `NEXUS_ASSISTANT_SYSTEM_VK` is set and the VK is valid on the AI Gateway
   (`POST /chat` with a trivial message streams a reply, not a 503).
2. The spill backend is **shared** with the other services (same `spill:` root /
   bucket+prefix) — confirm a written sandbox file downloads back via `GET /files/:id`.
3. For multi-replica: ingress session affinity is configured (a confirm-tier action —
   e.g. an explain/dry-run — completes its Allow round-trip without a 409).
4. Reverse proxy does not buffer `/chat` (tokens stream incrementally, not in one
   burst at the end).
5. The model picker lists the auto-derived chat models (or the pinned `NEXUS_ASSISTANT_MODELS` allow-list).

## Metrics (Prometheus)
The Control Plane emits these assistant instruments on `/metrics` (prefix
`nexus_`). Use them to watch health and to derive the §7 North Star / guardrails.

| Metric | Labels | Meaning |
|---|---|---|
| `assistant_turns_total` | `result` = ok \| error \| unavailable \| unsupported_auth | per-turn outcome |
| `assistant_tool_invocations_total` | `tool`, `result` = ok \| error | internal tool calls + misfires |
| `assistant_confirms_total` | `decision` = allow \| deny \| timeout \| cancelled | dangerous-write gate decisions |
| `assistant_navigations_total` | — | cross-page navigation directives emitted (≥1 per turn) |
| `assistant_pii_to_prompt_total` | — | tool results in which PII was redacted before prompt entry (guardrail target 0) |

Per-user attribution is **not** a metric label (cardinality) — use the admin
audit trail (`AdminAuditLog`, `via='assistant'`) for per-user/per-action queries.

Derived signals (PromQL):
- **Turn error / raw-error-exposure rate:** `sum(rate(nexus_assistant_turns_total{result="error"}[5m])) / sum(rate(nexus_assistant_turns_total[5m]))` (each `result="error"` turn also emits an SSE `error` event to the user — this is the raw-error-exposure guardrail).
- **Tool-misfire rate:** `sum(rate(nexus_assistant_tool_invocations_total{result="error"}[5m])) / sum(rate(nexus_assistant_tool_invocations_total[5m]))`.
- **Dangerous-write approve rate:** `sum(rate(nexus_assistant_confirms_total{decision="allow"}[1h])) / sum(rate(nexus_assistant_confirms_total[1h]))`.
- **Cross-page task volume (North-Star numerator candidate):** `sum(increase(nexus_assistant_navigations_total[7d]))` plus confirmed mitigations `sum(increase(nexus_assistant_confirms_total{decision="allow"}[7d]))`.

Derived offline: **dangerous-write reversal rate** — correlate
`confirms_total{decision="allow"}` with later `via='assistant'` undo actions in
`AdminAuditLog` (a join, not a single series).

Not yet emitted (honest gaps, see e90-s8 §5): the **North-Star success refinement**
("user did not redo within ~60s" / "issue eased"), **hallucination/correction rate**,
and **first-question abandonment** all need a frontend redo/correction/session-
continuation signal that does not exist yet (`turns_total` counts turn outcomes, not
whether a one-question session was abandoned). There is intentionally **no confirm-wait
/ turn-duration histogram** — the shared registry's fixed sub-second ms buckets cannot
represent human/LLM-scale latency; turn latency is observable via the system VK's own
gateway `traffic_event`.

## Data governance (PII + provider posture)
- **PII redaction before prompt.** Tool output from the body-relaying read tools
  (`observe_traffic_event`, `observe_traffic_list`, `resource_read`, `resource_invoke`)
  is scrubbed of PII (email, credit card, SSN, phone) before it enters the prompt,
  using the product's own PII detection engine. Watch `assistant_pii_to_prompt_total` —
  a sustained non-zero rate means real PII is flowing through the assistant's reads and
  being redacted at the boundary (working as intended, but worth knowing). The pattern
  set is the canonical default; an admin's custom PII rules are a planned enhancement.
- **Provider posture.** The assistant's inference runs on the system virtual key like
  any other gateway traffic — so the assistant's own conversation is governed by the
  same compliance pipeline (intentional dogfooding). To keep all assistant inference
  on-premise, point the system VK at a self-hosted / local model. To remove the
  raw-body read surface entirely, set `NEXUS_ASSISTANT_DISABLE_BODY_READS=1` — the
  assistant then has no `observe_traffic_event` / `observe_traffic_list` /
  `resource_read` / `resource_invoke` tools at all (the aggregate analysis tools stay).

## Related
- Architecture / invariants: `docs/developers/specs/e90-nexus-web-assistant.md`.
- Persistence as-built: `docs/developers/specs/e90-s6-persistence.md` §5.
- Skills + file sandbox as-built: `docs/developers/specs/e90-s7-builtin-skills.md` §5.
- Backend robustness (command/data-stream split, owner registry, reconnect): P2b
  (planned) — removes the single-pod affinity requirement above.
