# E41-S5 — Admin API and UI (v2: unified Reliability surface + probe + overrides)

## Story
As an admin, I want a single "Reliability" cell on every credential row,
a dedicated Reliability tab on the detail page with 8-second polling,
a "Test credential" button that runs a real provider probe, and
threshold overrides editable both globally (Settings) and per
credential.

## Tasks

### T5.1 — Backend API (Control Plane)
- `GET /api/admin/credentials` and `/{id}` carry the new v2 fields and
  merge `liveCircuit` from Redis.
- `PUT /api/admin/credentials/{id}/reliability-overrides` accepts a
  partial Thresholds JSON; validates with `DefaultThresholds.Merge(...).Validate()`;
  triggers `hub.InvalidateConfig("ai-gateway", "credentials")`.
- `POST /api/admin/credentials/{id}/probe` proxies to AI Gateway's
  `/internal/v1/credentials/{id}/probe`, audits, returns result verbatim.
- `GET/PUT /api/admin/settings/credential-reliability` round-trips the
  global Thresholds row in system_metadata; PUT triggers
  `hub.InvalidateConfig("ai-gateway", "credential_reliability")`.

### T5.2 — AI Gateway probe endpoint
`POST /internal/v1/credentials/{id}/probe` resolves the credential via
the cachelayer snapshot, decrypts via the existing MultiDecryptor,
picks the adapter, runs `adapter.Probe`, returns
`{ ok, latencyMs, detail, error, providerName, adapterType, probedAt }`.
No traffic_event written.

### T5.3 — UI: unified Reliability cell on the list
`ReliabilityCell` component: single badge (worst-of tone) with hover
popover showing 5m / 1h rates, samples, dominant error, trend, circuit
state + reason + opened-at + next-probe, live `authFailsCurrent`, and
`healthCheckedAt`. Replaces the v1 separate Circuit and Health columns.
Old CSS classes deleted.

### T5.4 — UI: Reliability tab on the detail page
`ReliabilityPanel` component owns:
- `useApi` with `refetchInterval: 8000` for live polling;
- Test Credential button + inline result panel (silent on error, the
  panel shows the failure);
- Reliability Overrides editor (7 fields; blank = inherit global);
- Circuit Reset shortcut (only when state ≠ closed).

### T5.5 — UI: Settings page tab
`SettingsCredentialReliabilityTab` round-trips
`/api/admin/settings/credential-reliability`. Validates client-side
(positive integers, degraded < healthy, healthy ≤ 100) before saving.
"Reset to shipped defaults" button rehydrates the form from
`response.defaults`.

### T5.6 — i18n
All new strings in en / zh / es. Old `circuitClosed` / `circuitOpen` /
`circuitHalfOpen` / `health_*` flat keys replaced by the namespaced
`circuit_closed` / `health_*` / `dominantError_*` / `trend_*` set.
Mirror to `public/locales`.

### T5.7 — useApi + useMutation extensions
- `useApi` gains optional `refetchInterval: number` so polling consumers
  don't need to bypass the hook.
- `useMutation` gains `onError` + `silentError` so the probe panel can
  show inline failure without firing a toast.

## Acceptance Criteria
- The Reliability column on the list shows the right tone for all four
  health states + open / half-open circuits.
- Hovering the badge reveals the popover with every documented field.
- The detail page Reliability tab refetches every 8 s (verify via
  network panel).
- Saving a per-credential override with `authFailThreshold=1` flips a
  401-only credential to circuit-open after a single request.
- Saving an empty Reliability overrides form clears the row's JSONB
  back to `null`.
- The Settings page rejects degraded ≥ healthy with an inline error.

## Files Touched
- `packages/control-plane/internal/handler/admin_credentials.go`
- `packages/control-plane/internal/handler/admin_credential_reliability.go`
- `packages/control-plane/internal/handler/admin_routes.go`
- `packages/control-plane/internal/store/credential.go`
- `packages/ai-gateway/internal/handler/credential_probe_endpoint.go`
- `packages/ai-gateway/cmd/ai-gateway/main.go`
- `packages/control-plane-ui/src/api/types.ts`
- `packages/control-plane-ui/src/api/services/credentials.ts`
- `packages/control-plane-ui/src/api/services/index.ts`
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/ReliabilityCell.tsx`
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/ReliabilityPanel.tsx`
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/CredentialList.tsx`
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/CredentialList.module.css`
- `packages/control-plane-ui/src/pages/ai-gateway/credentials/CredentialDetail.tsx`
- `packages/control-plane-ui/src/pages/settings/SettingsCredentialReliabilityTab.tsx`
- `packages/control-plane-ui/src/pages/settings/SettingsPage.tsx`
- `packages/control-plane-ui/src/hooks/useApi.ts`
- `packages/control-plane-ui/src/hooks/useMutation.ts`
- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`
- `packages/control-plane-ui/public/locales/{en,zh,es}/pages.json`
