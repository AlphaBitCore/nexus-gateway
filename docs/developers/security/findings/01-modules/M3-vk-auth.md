# M3 — Virtual key authentication + cross-VK enforcement

> Phase 1 module audit. Base `77e86466f`. Confirmed findings only (≥2/3 adversarial Opus verifiers).

---

## SEC-M3-01 — "Revoke user access" disables VKs in DB but pushes no cache invalidate — **MEDIUM** (3/3)
> **STATUS: FIXED** (2026-06-09) — `RevokeUserAccess` now pushes the ledger-backed `InvalidateConfigE(ai-gateway, virtual_keys)` after disabling the keys (same path as the per-VK `RevokeVirtualKey`); a failed push returns 502 (retry, re-driven by ReconcilePending) instead of a false success. Regression: `TestRevokeUserAccess_PushesVKInvalidate` + `_InvalidateFailure_Returns502`.

- **Persona:** A5 insider admin triggers (clicks *Revoke User Access* on a compromised/departing user); A2 (the just-revoked user) benefits, racing requests through any `nvk_` key during the window.
- **Invariant:** A successful access-revocation MUST stop the revoked principal's credentials from authenticating at the data plane no later than the action returns success (or the data plane must re-validate against fresh state every request).

**Preconditions.** ai-gateway VK auth resolves through `cachelayer.Layer` (prod wiring `vkauth.go:15`) with `VKTTL=30s`. Target user has ≥1 active `nvk_` VK resident in the LRU (a key in active use always is). Admin invokes `RevokeUserAccess`.

**Attack steps.**
1. `RevokeUserAccess` (`users.go:321`) runs 4 steps: `DisableVirtualKeysByOwner` (`enabled=false` in Postgres, `cross_path_governance.go:62`), `RevokeDevicesByUser`, `SuspendUser`, `revokeUserScope`.
2. `revokeUserScope` (`auth_sessions.go:34`) only mints an OAuth `scope=user` revocation + deletes `RefreshToken` rows — governs the agent OAuth plane, **NOT** VK admission (a separate HMAC-hash path).
3. **No** `h.hub.InvalidateConfig(...,"virtual_keys")` is called anywhere in `RevokeUserAccess` — yet the same handler file pushes `"organizations"` invalidates elsewhere, so the capability exists and was simply omitted here.
4. At the gateway, `vkauth.Authenticate` (`vkauth.go:138-160`) checks `vk.Enabled` against the row from `cachelayer.GetVirtualKeyByHash`, which serves the LRU entry loaded before the disable (`Enabled=true`), refreshed only when `loadedAt` exceeds `VKTTL` (`keycache.go:120,199`).
5. Every one of the user's VKs keeps passing auth and dispatching upstream AI calls for up to `VKTTL` (≤30s) after the admin sees HTTP 200. Contrast `approval.go:130 RevokeVirtualKey`, which explicitly pushes the invalidate with the comment *"Revocation MUST reach the gateway or the revoked key keeps authenticating from the VK cache"* — the same invariant is unmet on the bulk user-revoke path.

- **Affected:** `packages/control-plane/internal/identity/users/handler/users.go:321-375`; contrast `packages/control-plane/internal/ai/virtualkeys/handler/approval.go:143-148`; window bounded by `packages/ai-gateway/internal/cache/layer/layer.go:90-93` + `packages/ai-gateway/internal/auth/vkauth/vkauth.go:152`.
- **Re-check probe.** `rg -n "func .*RevokeUserAccess" -A40 packages/control-plane/internal/identity/users/handler/users.go | rg -q "virtual_keys"` — expect a match after fix. Behavioural test: load a VK into cachelayer, call `RevokeUserAccess` for its owner, immediately re-`Authenticate`, assert `ErrDisabled` without sleeping past `VKTTL`.
- **Remediation.** After `DisableVirtualKeysByOwner`, push the VK invalidate to ai-gateway via the error-returning ledger-backed path (`InvalidateConfigE`) so a failed push surfaces as 502 + is re-driven by `ReconcilePending` (as `RevokeVirtualKey` does). Ideally return the affected keyHashes for a targeted `{op:invalidate,ids:[hash…]}`; a full `virtual_keys` purge is an acceptable simpler fix.

---

## SEC-M3-02 — Virtual key accepted as `?key=` URL query parameter  
> **STATUS: FIXED** (2026-06-09) — removed the Gemini `?key=` URL-query VK carrier from `extractVKToken` entirely; only the `x-goog-api-key` header carries the VK (what real Gemini SDKs use). Removal also closes the OpenAI-route `x-nexus-aigw-body-format: gemini` escalation (no carrier left to inherit). Breaks kill-chains A1-KC4 / W-A6-4 / the A2 VK-in-URL replay. Regression: `TestExtractVKToken_Gemini_QueryParam_NotAccepted`. The `x-nexus-aigw-body-format` override header has since been removed outright, so remediation item (3) below is moot and no route can flip its ingress format to inherit another format's carrier.

### (original) — plaintext VK leaks to fronting-proxy/LB access logs, browser history, on-path Referer — **MEDIUM** (3/3)

- **Persona:** A6 host/log read (and A4 on-path).
- **Invariant:** A long-lived bearer credential (the VK) must never travel in a request component (the URL) that intermediaries, access logs, and browser history capture and persist in cleartext.

**Preconditions.** Caller hits the data plane (`:3050`) on a Gemini route (`/v1beta/...`) using Google's SDK-conventional `?key=<vk>` carrier, OR any OpenAI-compat route after forcing the ingress format with header `x-nexus-aigw-body-format: gemini` (`ingress.go:201-214` lets an OpenAI-route caller switch `BodyFormat` to gemini → stamps `FormatGemini` via `proxy.go:291` → unlocks the `?key=` carrier). At least one logging hop in front (appliance/reverse-proxy, ALB/CDN access logging, corporate egress proxy) — standard prod topology since `:3050` binds loopback behind a fronting proxy.

**Attack steps.**
1. `GET/POST https://gw/v1beta/models/gemini-2.5-pro:generateContent?key=nvk_<64hex>`.
2. `extractVKToken` (`vkauth.go:233-239`) reads `?key=`; auth succeeds. The VK is now in the request-line URL.
3. The gateway's **own** logs use `r.URL.Path` only (`middleware.go:47`, audit `Path=r.URL.Path` `proxy.go:326`) — so it does NOT leak. But every fronting/adjacent component that logs the request URI does: nginx/ALB/CDN access logs (`$request`/`$request_uri` includes the query by default), corporate forward-proxy logs, browser history, Referer.
4. A6 with read access to any access-log store recovers the full live VK in cleartext — no decryption. Since at rest the gateway stores only `HMAC(key)` + 12-char prefix, the URL/log copy is the **only** plaintext copy of a still-valid credential, and the easiest to read.
5. Replay `nvk_<64hex>` against `:3050`: full cross-VK impersonation (victim's model allowlist, quota, billing attribution, source-app identity) until rotation.

- **Affected:** `packages/ai-gateway/internal/auth/vkauth/vkauth.go:237`; unlocked-on-OpenAI-routes via `packages/ai-gateway/internal/ingress/proxy/ingress.go:201-214` + `proxy.go:291`.
- **Re-check probe.** `rg -n 'URL\.Query\(\)\.Get\("key"\)' packages/ai-gateway/internal/auth/vkauth/vkauth.go` (a hit = credential-in-URL carrier present). After remediation, a request carrying only `?key=nvk_valid` on a Gemini route should 401 unless the operator opted in; assert `extractVKToken(ctxGemini, reqWithQueryKey) == ""` by default.
- **Remediation.** Make `?key=` opt-in, not default-on: (1) gate behind an explicit per-deployment flag (default off) — header carriers (`x-goog-api-key`, `Authorization`) already cover real Gemini SDKs; (2) if kept for raw-URL SDK compat, document a binding fronting-proxy requirement to strip/redact the `key` query param from logs + never forward to Referer, and emit a startup warning when enabled; (3) restrict the `x-nexus-aigw-body-format` override so an OpenAI route cannot silently flip to `gemini` and inherit the URL-key carrier.
