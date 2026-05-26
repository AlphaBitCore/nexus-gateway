# E42-S7 — SIEM Bridge phantom-UI fix (SDD)

## User story

As an SRE I save SIEM forwarding settings (enable / URL / format /
headers / event types) in the Control Plane UI, and within one poll
interval (≤ 30 s by default) the Hub-side SIEM Bridge picks up the new
config — no operator restart, no separate "Apply" action.

## Background

The Control Plane UI's **Settings → SIEM** tab edits
`system_metadata['siem.config']` via
`PUT /api/admin/settings/siem`
(`packages/control-plane/internal/handler/admin_siem.go`). The Hub-side
`siem.Bridge` reads this row exactly **once** at startup
(`packages/nexus-hub/cmd/nexus-hub/main.go::initSIEMBridge`) and bakes
the result into a static `Sink` + `BridgeConfig` pair. Every admin save
after process start is silently ignored — the operator's UI confirms
"Saved" but the running forwarder keeps the old URL / headers / event
types. This is the canonical **phantom UI** the user flagged: a field
that is configurable in the admin UI but has no runtime effect — that
should not exist.

## Tasks

1. **`siem.Bridge` hot-swap rework**:
   - Replace the static `sink Sink` + `cfg BridgeConfig` fields with
     `atomic.Pointer[Sink]` + `atomic.Pointer[BridgeConfig]`.
   - Add `Bridge.Reload(ctx)` that re-reads `system_metadata['siem.config']`,
     rebuilds the HTTP sink + cfg, and atomically swaps. A missing row /
     `Enabled=false` / empty URL nils the active sink so subsequent
     Poll calls become no-ops.
   - Add `Bridge.ActiveSinkName()` so `initSIEMBridge` can log the
     boot-time state.
   - `Poll()` now calls `Reload(ctx)` at the head of every cycle, then
     short-circuits when the active sink is nil. Within one poll
     interval, an admin UI save propagates without restart.
   - `PollInterval()` reads the live cfg snapshot so a `pollIntervalSeconds`
     change ripples through on the next reload (the scheduler still
     uses the original interval until a refresh is added in a future
     epic — minor latency, no phantom).
2. **`initSIEMBridge` always-on**:
   - Construct the bridge unconditionally (even when SIEM is disabled
     or no config row exists). The scheduler registers an always-on
     job; the bridge lights up the first time `Enabled=true` is
     observed during Reload.
3. **Drive-by: backward-compat checkpoint loader**:
   - `loadCheckpoint` previously rejected the legacy
     `{"lastForwardedAt": "..."}` shape with an unmarshal error,
     bricking the bridge on any deploy that had the old format on disk.
     Accept either shape and warn-and-recover on unparseable rows
     (next save normalises to the canonical bare-string form).
4. **Drive-by: E29 schema catch-up**:
   - `queryEvents` referenced the pre-E29 `hook_decision` /
     `hook_reason` / `hook_reason_code` columns; the hooks region
     refactor split these into `request_hook_*` + `response_hook_*`
     pairs. The SQL is updated to select both halves and emit them as
     `requestHook*` / `responseHook*` in the event payload. Legacy
     flat `hookDecision` / `hookReason` / `hookReasonCode` aliases are
     preserved (response wins when both present) so existing SIEM
     dashboards built against the old schema keep matching.
5. **No new shadow key, no NOTIFY plumbing**: this is a pure pull-based
   hot-swap. The Hub Bridge already polls the DB on a cadence; piggy-
   backing one extra siem.config read per poll cycle costs a single
   `SELECT` and keeps the design footprint minimal.

## Out of scope

- `pollIntervalSeconds` live-changing without a Hub restart. The Hub
  scheduler captures the interval at job registration; runtime
  re-cadence would require teaching the scheduler to accept interval
  updates, which is its own epic. Documented limitation only.
- compliance-proxy SIEM forwarder (`packages/compliance-proxy/internal/siem/forwarder.go`).
  Investigation showed compliance-proxy does NOT consume
  `system_metadata['siem.config']` — its forwarder is YAML-only and the
  UI write path does not target it. No phantom there.

## Acceptance criteria

- [ ] `go test ./packages/nexus-hub/internal/observability/siem/... -race -count=1`
      passes.
- [ ] Hub start with no `siem.config` row → bridge logs
      "registered in disabled state — will activate when
      siem.config.enabled becomes true"; no further siem-bridge log
      entries until enable.
- [ ] Insert `siem.config` row with `enabled=true`, valid `url`, and
      `pollIntervalSeconds=30` → within ≤ 30 s the bridge logs
      `siem bridge: forwarded events sink=http:<url>` (or a delivery
      error if the SIEM endpoint is unreachable — both prove the
      bridge picked up the new sink).
- [ ] Flip `enabled=false` → next poll cycle is silent (no log entry
      from `siem bridge`).
- [ ] Stale `siem.bridge.checkpoint` rows in the legacy
      `{"lastForwardedAt": "..."}` shape are migrated transparently —
      bridge logs `siem bridge: migrated legacy checkpoint format`
      and continues forwarding.
- [ ] `traffic_event` rows are correctly queried using the post-E29
      `request_hook_*` / `response_hook_*` schema with no
      `column does not exist` errors.
