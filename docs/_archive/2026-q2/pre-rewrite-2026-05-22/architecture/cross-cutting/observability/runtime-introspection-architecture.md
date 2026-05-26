---
doc: runtime-introspection-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Runtime Introspection Architecture

> **Tier 3 architecture doc.** Read when touching `packages/shared/core/diag/runtimeintrospect/` or per-service runtime / debug endpoints. Lets operators see what the running process **currently thinks** without redeploying.

---

## 1. Why introspection separate from logs / metrics

Logs are after-the-fact. Metrics are aggregates. **Introspection** answers "what's the current effective config in this process right now?" — the snapshot of in-memory state that hot-swapping has produced.

If a hook config update is mis-applied, logs may not surface it for hours; metrics show degraded enforcement after the fact. Introspection lets ops query the **live snapshot** and verify.

## 2. The endpoints

Two distinct surfaces serve introspection today:

**A. Shared `runtimeintrospect` registry — `GET /debug/runtime`.** A single bearer-gated GET that returns a JSON snapshot built from every `Source` the service has registered. Mounted by Hub at `e.GET("/debug/runtime", …)` (`packages/nexus-hub/cmd/nexus-hub/wiring/routes.go:95`) and by ai-gateway at `mux.Handle("GET /debug/runtime", …)` (`packages/ai-gateway/cmd/ai-gateway/wiring/runtimeapi.go:175`). Handler implementation: `packages/shared/core/diag/runtimeintrospect/handler.go`.

**B. Per-service `/runtime/*` runtime API (ai-gateway / compliance-proxy).** A wider surface for the same bearer-gated investigation flow but split into discrete routes that the service authors itself:

| Endpoint | Returns |
|---|---|
| `GET /runtime/config` | Full effective config snapshot (redacted). |
| `GET /runtime/config/{key}` | Single config key value (redacted if sensitive). |
| `GET /runtime/sync-status` | Per-key sync state — last-applied generation, drift, error if any. |
| `GET /runtime/health` | Readiness signal (DB / Hub WS / shadow reconciled). |

Wired in ai-gateway at `packages/ai-gateway/internal/runtimeapi/server.go:44-47` and in compliance-proxy at `packages/compliance-proxy/internal/runtime/server/server.go` (see `compliance-pipeline-architecture.md` §9 for the compliance-proxy variant which also exposes a break-glass PUT path).

There are no `/runtime/hooks`, `/runtime/routing/rules`, `/runtime/credentials/health`, or `/runtime/cert-cache/stats` endpoints. There is no `/runtime/cpu-profile`, `/runtime/heap-profile`, `/runtime/goroutine-dump`, or any other pprof handler — pprof is not wired into any production binary.

## 3. Auth

Bearer-token, with disabled-by-default semantics (`packages/shared/core/diag/runtimeintrospect/handler.go`):

- `HandlerOptions.Token` empty → endpoint returns `503 Service Unavailable` ("introspection disabled"). This is the production default until an operator explicitly provisions a token.
- `Authorization` header missing the `Bearer <token>` prefix → `401`.
- Bearer mismatch (constant-time compare) → `401`.
- Bearer match → JSON snapshot with `Cache-Control: no-store`.

There is no localhost-bind requirement. The endpoint is reachable from anywhere the service binds, gated entirely by the bearer token. For Hub-mediated introspection (admin clicks "show snapshot for this Thing" in CP UI), the request goes Hub → Thing over the existing mTLS WS; for on-host investigation, operators set the bearer in the service env and curl with the token.

## 4. Redaction (binding)

All introspection responses are redacted for sensitive fields:

- Provider API keys → `[redacted]`.
- Bearer tokens → `[redacted]`.
- Private keys → `[redacted]`.
- mTLS cert private key → `[redacted]`.

The redaction list is the same as `audit-pipeline-architecture.md` §6. Sensitive fields don't appear in introspection output by design.

## 5. Mode-of-use

Two typical workflows:

### a. Operator on-host

```bash
# SSH to the prod instance, set the introspection token in the service env, then:
curl -H "Authorization: Bearer $RUNTIME_INTROSPECT_TOKEN" \
  http://localhost:3050/runtime/config | jq
```

### b. Admin via CP UI

```
CP UI → Infrastructure → Nodes → select Thing → "Runtime Introspection" tab
```

The CP fetches via Hub mTLS → Thing's runtime API → returns to admin browser.

## 6. pprof — intentionally not wired

Go's `net/http/pprof` is not registered on any production binary today. There is no `enable_pprof_endpoint` bootstrap config field and no `/runtime/cpu-profile` etc. routes. When a profile is needed, operators take it via `dlv` / `pprof` in a dev environment that mirrors prod load, or by adding pprof under a build tag in a one-off branch — never via runtime config flip in production.

This is a deliberate "delete instead of add" choice: pprof endpoints in long-running production processes are a known footgun (large response payload + stop-the-world GC pauses on heap), and the runtime-introspection use case rarely overlaps with the performance-profiling use case. Re-introducing pprof endpoints requires explicit user approval and a Plan-Mode proposal in PR.

## 7. Performance impact

Introspection endpoints are O(1) reads from in-memory atomic-pointer snapshots — negligible cost. There are no expensive endpoints in the current surface.

## 8. Cross-references

- `compliance-proxy-details-architecture.md` §5 — runtime API pattern.
- `audit-pipeline-architecture.md` §6 — same redaction list.
- `diag-event-triage-architecture.md` — diag mode interaction.
- E31-S7 in archived sources (`_archive/`) — original requirements.
