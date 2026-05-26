# E55-S2 — Streaming Compliance trinity (cp + agent share `shared/streaming/policy`)

**Epic:** E55 (`docs/developers/specs/e55/e55-tls-bump-trinity.md`)
**Depends on:** E55-S1 (shared/tlsbump exists)

## User Story

> As an admin tuning streaming compliance for SSE LLM responses, I want the
> macOS agent to honor the same Streaming Compliance UI knobs (mode,
> chunk_bytes, hook_timeout_ms, max_buffer_bytes, on_hook_failure,
> capture_request_body, capture_response_body, raw_spill_enabled) that
> compliance-proxy already honors — including per-host overrides on
> Interception Domains — so a single admin policy applies regardless of
> ingress.

## Tasks

### S2.T1 — Agent thingclient registers `streaming_compliance.config`
- In `packages/agent/core/sync/configsync/manager.go` (or equivalent) register the Cat B Thing config key `streaming_compliance.config` with `needsPull: true` per the [thing pull-only model](../../../../docs/developers/architecture/cross-cutting/foundation/thing-model.md).
- On change-signal, fetch the latest blob via thingclient HTTP, decode using `streampolicy.LoadGlobalPolicy`, and atomically swap the pointer.

### S2.T2 — Agent stamps `tlsbump.Deps.StreamPolicy` from the swappable policy
- The agent ingress wrapper (`bridge.go`) reads the current global StreamPolicy from the swap pointer when constructing `Deps` per inbound bridge connection.
- Per-host override resolution stays in `tlsbump`'s existing SSE pipeline (`shared/streaming/policy.Resolve(global, perHost)`), which already merges `interception_domain.streaming_*` columns over the global default.

### S2.T3 — Verify three-mode parity
- Manual test matrix per mode (admin sets in UI; verify in admin UI Traffic detail):

  | Mode | Hook decision: approve | Hook decision: block | Hook timeout |
  |---|---|---|---|
  | `passthrough` | relay only; no hook called; no body captured | (n/a — no hook) | (n/a) |
  | `chunked_async` | bytes flow real-time to client; hook runs audit-only on chunk boundaries; pipeline result on traffic_event | hook records `block` decision; client still gets bytes (audit-only) | configured `fail_open` → continue; `fail_close` → audit-flag and continue |
  | `buffer_full_block` | response held until hook returns; client gets full body or 451 | client gets HTTP 451 with reason; no body | `fail_open` → release body; `fail_close` → 451 |

- Acceptance: agent rows in `traffic_event` show identical `request_hooks_pipeline` / `response_hooks_pipeline` / `action` values to cp rows for the same hook + mode combinations.

## Acceptance Criteria

- [ ] Switching `streaming_compliance.mode` in admin UI changes agent behavior live within one Hub config-sync round-trip (≤ 5 s).
- [ ] Per-host override on an `interception_domain` row (e.g. `api.openai.com` set to `chunked_async` while global is `buffer_full_block`) is honored by both ingresses.
- [ ] Streaming SSE responses through the agent stream byte-for-byte to the client in real time when mode is `chunked_async` or `passthrough` (no client-perceived buffering > 100 ms).
- [ ] `buffer_full_block` mode + a hook that returns `block` causes the client to receive HTTP 451 (no body bytes).
- [ ] `fail_close` + a deliberately-timing-out hook causes HTTP 451 even on a passing input (verified by setting `hook_timeout_ms = 1`).

## Risks / open items

- Per-host override columns currently live on `interception_domain` (via `shared/configtypes.InterceptionDomain`) but `shared/streaming/policy.Resolve` expects fields named on the cp-specific `domainpolicy.InterceptionDomain` row. After S1.T3 unifies these, the resolver may need a small adapter.
