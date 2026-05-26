# E46 S7 — TrafficEvent UI Normalized + Raw tabs

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s7-ui-normalized.yaml](../openapi/e46-s7-ui-normalized.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 6 delivers the operator-facing payoff. The `TrafficEvent` detail page in the Control Plane UI gains a tab strip with `Normalized` (default) and `Raw` (fallback). Each kind has its own renderer; redaction spans render as inline badges keyed by rule id.

The UI reads `GET /api/admin/traffic/{id}/normalized` (introduced in S2) for the normalized JSON. The raw view continues to fetch `traffic_event_payload` and renders the bytes as today.

---

## Story

### S7 — Normalized + Raw tabs

**User story:** As a compliance officer, I open any traffic event and immediately see a readable conversation view; raw bytes are still one click away.

**Tasks:**

- **T7.1** — `packages/control-plane-ui/src/pages/traffic/TrafficEventDetail.tsx` (or equivalent existing detail page):
  - Replace the single payload viewer with a `<Tabs>` component (Radix) — tabs: `Normalized`, `Raw`.
  - Default tab: `Normalized`. Switching to `Raw` is sticky within session via URL hash (`#raw`).
- **T7.2** — Per-kind renderer components under `packages/control-plane-ui/src/components/traffic/normalized/`:
  - `AiChatView.tsx` — chat bubble layout, role chips (system / user / assistant / tool), tool-use cards, usage card, model + finish_reason badge.
  - `AiCompletionView.tsx` — single prompt + single completion column.
  - `AiEmbeddingView.tsx` — input list + vector summary (length only; vectors not displayed).
  - `AiImageView.tsx` — prompt + image grid (links to spill artifacts).
  - `HttpJsonView.tsx` — collapsible JSON tree (existing react-json-view-lite or similar).
  - `HttpTextView.tsx` — decoded text with monospace + line numbers.
  - `HttpFormView.tsx` — field/value table.
  - `HttpBinaryView.tsx` — metadata card (size, content-type, sha256, link to spill artifact if any).
  - `UnsupportedView.tsx` — placeholder with a link to the Raw tab.
- **T7.3** — Redaction span rendering: each renderer accepts `redactionSpans` prop and overlays inline badges (`<span class="redaction-badge">[redacted: pii-email]</span>`) at the span offsets in the relevant content block / body view. Tooltip shows rule id, replacement, and originating hook.
- **T7.4** — Normalize-failure banner: when `request_status` or `response_status` is `failed` / `partial`, the Normalized tab shows a top banner with the `error_reason` and a "View raw bytes" button that switches to the Raw tab. The banner is dismissible per-session but persists across page reloads (no localStorage; intentional — operators should re-see failures).
- **T7.5** — Reason-code chip: when `traffic_event.request_hook_reason_code` or `response_hook_reason_code` is one of the closed-set reason codes (`REDACT_INFLIGHT_UNSUPPORTED`, `REDACT_STORAGE_ONLY_BY_POLICY`, `STORAGE_DROPPED_BY_POLICY`, `AIGUARD_SUGGESTED_VS_POLICY`), render a colored chip next to the decision badge with i18n-translated explanatory text on hover.
- **T7.6** — i18n keys for every new string in `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` (subkey `traffic.normalized.*`). Copy to `public/locales/`. Verify key-count parity across all three locales.
- **T7.7** — Vitest unit tests for each renderer component (snapshot + a11y check).
- **T7.8** — Playwright e2e: open the traffic detail page after a streaming ai-chat request, verify the Normalized tab shows the assembled conversation, then switch to Raw and verify the SSE chunks are visible.

**Acceptance:**

- **AC-S7.1** — Normalized tab is the default for every kind; switching to Raw is reversible.
- **AC-S7.2** — A traffic event with `request_status = "failed"` shows the failure banner with the error reason; Raw tab still works.
- **AC-S7.3** — A traffic event with one or more redaction spans shows inline badges at the correct offsets in the chat bubble or JSON tree, with tooltip metadata.
- **AC-S7.4** — i18n: `npm test -- i18n-gap-check` (or the existing `i18n-gap-check` skill) reports zero gaps for the new keys across `en / zh / es`.
- **AC-S7.5** — Playwright test passes against a local stack.
