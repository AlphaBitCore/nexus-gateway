# Flow — Kill switch & emergency passthrough (E48)

## What this flow accomplishes

The compliance pipeline cannot run (hook outage / safety incident / mass mis-configuration). The admin activates **emergency passthrough**: traffic flows without enforcement, the audit trail is still emitted, and the system auto-reverts at expiry.

## Actors

Admin · CP · Hub · AI Gateway · Compliance Proxy · Agent · Hub reconcile job.

## Sequence

1. **Admin → CP UI → Infrastructure → Kill Switch** OR `/ai-gateway/passthrough` → choose tier (`global` / `org` / `provider` / `route`) → free-text reason → expiry (max 8h, default 1h) → confirm.
2. **CP → Hub** → update Cat A inline shadow keys (`killswitch.*` + `passthrough.expiresAt`).
3. **Hub → change-signal** all affected Things.
4. **AI Gateway** → routing engine sees the shadow → stamps `ResolvedRequest.BypassHooks=true` and `BypassReason="killswitch.<tier>"`.
5. **AI Gateway / Compliance Proxy / Agent** → forward requests **without invoking hooks**. Quota gates also bypassed (cross-ref `quota-architecture.md` §8).
6. **Each request still emits `traffic_event`** with `passthrough=true`, `quota_bypassed=true`, and the bypass reason. **Audit trail non-optional.**
7. **Hub `passthrough.expiry` job** (`packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go:27`) runs every minute → if `now > passthrough.expiresAt` → clear killswitch → change-signal Things → next request enforces again.
8. **Audit row** `system:kill_switch.auto_reverted` is emitted (cross-ref `admin-audit-log-coverage.md` §5).

## Cold-start invariants

- **Fail-closed cold-start** — a Thing booting and finding no shadow does NOT default to passthrough. It defaults to enforced, waits for shadow, then applies. (Prevents a Hub-down boot from silently disabling enforcement.)
- **Mandatory expiry** — UI rejects manual entry > 8h.
- **Audit trail is non-optional** — every bypassed request emits.
- **Auto-revert** — admin forgetting to revert is fine; Hub does it.

## Failure modes

- **Hub down during activation** — CP cannot update shadow; UI surfaces. Local kill switch (compliance-proxy runtime API) can be used as a per-instance escape.
- **Things slow to apply** — change-signal fans out within a second but apply can take a few seconds; UI surfaces "expected propagation N seconds".
- **Expiry job stuck** — `job.stalled` alert on `passthrough.expiry`. Manual intervention to clear stale state.
- **Drift** — a Thing reporting `bypassHooks=true` after admin reverted = drift alert.

## Verification

```bash
# 1) Activate.
cp_curl -X POST /api/admin/passthrough -d '{"tier":"provider","scope":{"provider":"openai"},"expiresAt":"...","reason":"hook outage"}'

# 2) Send a request; observe bypassHooks=true on the audit row.
curl ... /v1/chat/completions

docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT passthrough_flags, passthrough_reason FROM traffic_event WHERE request_id='...'"

# 3) Wait for expiry; observe auto-revert audit event.
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT event_type FROM admin_audit WHERE event_type LIKE 'system:kill_switch%' ORDER BY emitted_at DESC LIMIT 3"
```

## References

- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §4 — full flow diagram.
- `docs/users/features/cp-ui/infrastructure.md` — Kill Switch admin surface.
- `docs/users/features/cp-ui/ai-gateway.md` — Passthrough admin surface.
- `docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md` §7 — reconcile job.
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` — what error class clients see during passthrough.
- `project_e48_emergency_passthrough` (memory) — end-to-end verification retrospective.
