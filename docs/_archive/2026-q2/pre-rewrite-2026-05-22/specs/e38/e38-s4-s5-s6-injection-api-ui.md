# E38-S4/S5/S6 — Marker Injection + Dry-run API + Admin UI

> Stories: e38-s4, e38-s5, e38-s6
> Epic: 38 (Prompt Cache Friendliness)
> Status: Approved

## User Story

As a Platform Admin, I want the Gateway to auto-inject Anthropic
`cache_control` markers at semantic boundaries (L4), preview what the
normaliser would change for any captured request (dry-run API), and
configure all cache settings through the Admin UI.

---

## S4: cache_control Marker Injection (L3+L4 extension)

### Tasks

- T1: Add `rule_cache_inject.go` to `normaliser` package
  - Reads `system[*]`, `tools[*]`, `messages[*]` from Anthropic JSON
  - Appends `"cache_control": {"type":"ephemeral"}` to last content block at each boundary
  - Respects existing client-set markers (counts them; never removes)
  - Enforces total ≤ 4 markers (Anthropic limit)
  - Boundary 3 (messages history) only if `cache_marker_boundary3_enabled=true`
  - Returns `MarkersInjected` count in `Result`
- T2: Extended TTL: when `extended_ttl_enabled=true` (global), inject
  `"cache_control": {"type":"persistent"}` instead of `"ephemeral"` at
  boundaries. Only when Anthropic account has beta access.
- T3: Unit tests: single boundary, multi-boundary, existing-marker respect,
  4-marker limit enforcement, boundary-3 sub-toggle

### Acceptance Criteria

- AC1: When `cache_marker_inject_enabled=true` for a Provider and the
  request has a system prompt ≥ 1024 tokens (estimated), Boundary 1
  marker is injected and `CacheMarkerInjected=1` on the traffic event.
- AC2: Existing client `cache_control` markers are preserved; injected
  count reflects only the Gateway-added markers.
- AC3: Total markers never exceeds 4; if client set 3, Gateway injects 1.
- AC4: Boundary 3 not injected when `cache_marker_boundary3_enabled=false`.

---

## S5: Dry-run Preview API

### API

```
POST /api/admin/cache/preview
Authorization: Bearer <admin-token>
Content-Type: application/json

{
  "traffic_event_id": "uuid"
}

Response 200:
{
  "diff": "--- original\n+++ normalized\n@@...",
  "strip_count": 1,
  "strip_bytes": 32,
  "markers_injected": 2,
  "estimated_savings_usd": 0.00284,
  "rules_applied": ["anthropic/claude-code-cch-strip"],
  "rules_would_apply_if_enabled": ["anthropic/anthropic-cache-marker-inject"]
}
```

### Tasks

- T1: Add `POST /api/admin/cache/preview` to Control Plane admin router
- T2: Handler: load traffic_event_payload.inline_request_body; run normaliser in
  preview mode (always apply enabled rules + dry-run disabled rules); compute diff
- T3: Return unified diff + result fields
- T4: IAM: `admin:WriteSettings` (reuse existing settings write permission)

### Acceptance Criteria

- AC1: Given a captured traffic_event_id with an Anthropic request containing `cch=`,
  the API returns a diff showing the `cch=` removal and correct `strip_bytes`.
- AC2: Returns 404 when `traffic_event_id` not found.
- AC3: Returns 422 when the payload body is absent (non-LLM traffic).

---

## S6: Admin UI — Cache Settings Pages

### Pages

**1. Global Settings → Cache** (new tab under existing Global Settings)
- Toggle: "Body Normaliser" (global L3 on/off)
- Toggle: "Extended Cache TTL (1h)" (global L5)

**2. Providers → [Provider] → Cache tab** (new tab on Provider detail page)
- Toggle: "Auto-inject cache markers (L4)"
- Toggle: "Include conversation history boundary (Boundary 3)" — visible only when L4 is on
- Link to Cache Rules for this provider's adapter type

**3. Cache Rules page** (new top-level page under Settings)
- Table: adapter_type | rule_id | risk | status | 7d match rate | estimated savings | toggle
- Click rule row → rule detail drawer: description, risk_note, dry-run stats, enable/disable

### i18n keys (add to all 3 locale files)

```
settings:cache.title
settings:cache.normaliser_enabled
settings:cache.extended_ttl
providers:cache_tab.title
providers:cache_tab.inject_markers
providers:cache_tab.boundary3
cacheRules:page.title
cacheRules:table.adapter_type
cacheRules:table.rule_id
cacheRules:table.risk
cacheRules:table.status
cacheRules:table.match_rate
cacheRules:table.savings
cacheRules:drawer.risk_note
cacheRules:drawer.enable
cacheRules:drawer.disable
```

### Acceptance Criteria

- AC1: Toggling the global normaliser in UI writes to `system_metadata` via
  `PATCH /api/admin/system-metadata`; AI Gateway hot-swaps within 5 s.
- AC2: Cache Rules page lists all bundled rules with correct adapter_type labels.
- AC3: Enabling `claude-code-cch-strip` shows a confirmation dialog with the
  risk warning before saving.
- AC4: Provider Cache tab toggle for L4 persists to `system_metadata.prompt_cache.providers.<id>`.
