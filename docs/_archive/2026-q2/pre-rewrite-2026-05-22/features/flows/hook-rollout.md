# Flow — Hook rollout

## What this flow accomplishes

An admin safely turns on a new compliance hook (PII / keyword / content-safety / custom) across the right traffic paths, validates behaviour, and avoids blast radius.

## Actors

Admin · CP · Hub · AI Gateway · Compliance Proxy · Agent.

## Sequence

1. **Admin → CP UI → Compliance → Hooks → New Hook** → configure: match conditions, `onMatch.action` (`block-hard` / `block-soft` / `redact` / `flag` / `log-only`), `applicableIngress` subset.
2. **Initial rollout (recommended)** — start with `applicableIngress: [aiGateway]` and `action: flag` or `log-only`. Stage on a single project via `scope`.
3. **CP → Hub** → persist HookConfig with E46-S4 canonical onMatch schema → bump shadow `hooks/v=N+1` → change-signal the Things whose `applicableIngress` matches.
4. **AI Gateway / Compliance Proxy / Agent** receive signal → pull → atomic.Pointer swap.
5. **App → `/v1/...`** or proxied traffic → request-stage hook runs → records decision in `traffic_event`.
6. **Admin** validates via CP UI Traffic page → filter by hook id → see counts / rejection rate / sample bodies → confirm no false-positive surge.
7. **Promote to `block-soft`** — observe; promote to `block-hard` if safe.
8. **Expand `applicableIngress`** to compliance proxy and / or agent (note agent reality §7 of `hook-architecture.md` — local exemption / rule-pack enforcement gap).

## Failure modes

- **`onMatch.action` mis-set** — the PII scanner shipping `block-hard` instead of `redact` (2026-05-13) is the canonical example. Always verify against expected semantics.
- **Match condition too broad** — flag stage catches this; UI shows the % of traffic the rule matches.
- **Streaming compliance mode mismatch** — a hook expecting to "block" on `chunked_async` can't stop already-sent bytes; UI shows the mode and warns.
- **Drift after change-signal** — Config Sync page shows drift if a Thing failed to apply.

## Verification

```bash
# Issue a request that should trip the hook:
curl ... /v1/chat/completions -d '{"messages":[{"role":"user","content":"my SSN is 123-45-6789"}]}'

# Inspect the audit row:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT request_hook_decision, request_hooks_pipeline FROM traffic_event WHERE request_id='...'"

# Confirm the hook id appears in the pipeline trace with the expected verdict.
```

## References

- `docs/developers/architecture/services/ai-gateway/hook-architecture.md` — Hook framework + onMatch saga.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §3 — flow diagram.
- `docs/users/features/cp-ui/compliance.md` — admin surface.
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` §7 — agent enforcement reality.
- `project_hookconfig_e46s4_migration` (memory) — PII scanner saga retrospective.
