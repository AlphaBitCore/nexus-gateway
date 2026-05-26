# Flow — Routing rule lifecycle

## What this flow accomplishes

An admin defines a routing rule; the AI Gateway picks it up; the next `/v1/*` request gets routed accordingly; the routing trace surfaces in the UI for debug.

## Actors

Admin · CP · Hub · AI Gateway · App.

## Sequence

1. **Admin → CP UI → Routing → New Rule** at `/ai-gateway/routing/new` → choose strategy (Single / Fallback / LoadBalance / Conditional / A/B / PolicyNarrowing) → set match conditions → fallback chain `onClass` entries → submit.
2. **CP** → call **admin guard** (E47-S8) → reject if the rule can never match any cataloged (provider, model). On pass, forward to Hub.
3. **Hub** → persist `routing_rule` row → bump shadow Cat B `routing/v=N+1` → change-signal AI Gateway.
4. **AI Gateway** → pull new rules → atomic.Pointer swap.
5. **App → `/v1/...`** → routing engine evaluates rules in priority order against canonical payload (E47-S2) → strategy tree resolves → `ResolvedRequest` carries (provider, model, credential, routing_trace).
6. **Executor** → denormalize → upstream → response → record `routing_trace` JSONB on `traffic_event`.
7. **CP analytics UI** → "Routing trace" panel renders the strategy path for a given `request_id`.

## Failure modes

- **Admin guard reject** — UI surfaces the reason (empty effective set / mis-cataloged provider).
- **No matching rule** — gateway falls back to admin-configured default route; if no default, returns 503.
- **Smart routing chosen something outside policy** — re-evaluated post-LLM; PolicyNarrowing kicks the choice back.
- **Fallback chain exhausted** — 503 with `reason=fallback_exhausted`. Investigate per-credential health.
- **Kill switch active** — routing still computes; `ResolvedRequest.BypassHooks=true`; executor skips hooks (cross-ref `kill-switch-and-passthrough.md`).

## Verification

```bash
# Admin creates a Fallback rule (OpenAI → Anthropic).
cp_curl -X POST /api/admin/routing-rules -d '{...}'

# Send a request that matches; in dev, force the primary to fail by tampering creds; observe fallback.
curl ... /v1/chat/completions

# Inspect routing_trace:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT routing_trace FROM traffic_event WHERE request_id='...'"
```

## References

- `docs/developers/architecture/services/ai-gateway/routing-architecture.md` — strategy tree + canonical payload + admin guard.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §2 — flow diagram.
- `docs/users/features/cp-ui/ai-gateway.md` — admin surface.
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` — fallback `onClass`.
