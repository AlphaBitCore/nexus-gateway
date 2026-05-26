# E50 — Story 1: Schema Migration + Shared Phase Timer

## Context

Establishes the data foundation for the rest of the epic: 5 new columns + JSONB on
`traffic_event`, matching wire fields on `TrafficEventMessage`, and a shared
`phasetimer` helper that all three forwarders (ai-gateway, compliance-proxy,
agent) call so the phase taxonomy stays single-sourced.

## User Story

**As a** platform owner,
**I want** `traffic_event` and its MQ wire format to carry per-phase latency in a
shape that is identical across all forwarding services,
**so that** every downstream consumer (admin API, dashboards, alerts) reads the
same field names and the "Us vs Upstream" framing is consistent across the
platform.

## Tasks

### 1.1 Prisma migration — `tools/db-migrate/`

Append to model `traffic_event` (placed after `latency_ms` for column ordering
to read top-to-bottom as a phase progression):

```prisma
upstream_ttfb_ms        Int?  @map("upstream_ttfb_ms")
upstream_total_ms       Int?  @map("upstream_total_ms")
request_hooks_ms        Int?  @map("request_hooks_ms")
response_hooks_ms       Int?  @map("response_hooks_ms")
latency_breakdown       Json? @map("latency_breakdown")
```

Migration name: `20260522000000_e50_traffic_event_latency_phases`.

CHECK constraints via raw SQL appended after Prisma's auto-generated ALTER:

```sql
ALTER TABLE traffic_event
  ADD CONSTRAINT chk_traffic_event_upstream_ttfb_nonneg
    CHECK (upstream_ttfb_ms IS NULL OR upstream_ttfb_ms >= 0),
  ADD CONSTRAINT chk_traffic_event_upstream_total_nonneg
    CHECK (upstream_total_ms IS NULL OR upstream_total_ms >= 0),
  ADD CONSTRAINT chk_traffic_event_request_hooks_nonneg
    CHECK (request_hooks_ms IS NULL OR request_hooks_ms >= 0),
  ADD CONSTRAINT chk_traffic_event_response_hooks_nonneg
    CHECK (response_hooks_ms IS NULL OR response_hooks_ms >= 0),
  ADD CONSTRAINT chk_traffic_event_ttfb_le_total
    CHECK (upstream_ttfb_ms IS NULL OR upstream_total_ms IS NULL
           OR upstream_ttfb_ms <= upstream_total_ms);
```

No GIN index on `latency_breakdown` in V1 — the early access pattern is
"select the column, render in UI", not "filter by JSONB key". Promote to an
indexed key when query shape demands it.

### 1.2 Go codegen

Re-run `node tools/db-migrate/codegen-go.mjs` to regenerate
`packages/shared/schemas/configtypes/traffic_event.go`. Verify the five new fields are
present with `*int` types and the JSONB column is `json.RawMessage` or the
codegen's chosen Map type.

### 1.3 Shared MQ wire — `packages/shared/transport/mq/messages.go`

Extend `TrafficEventMessage`:

```go
// E50 latency phase breakdown. All optional; producers omit when not measured.
UpstreamTtfbMs    *int                 `json:"upstreamTtfbMs,omitempty"`
UpstreamTotalMs   *int                 `json:"upstreamTotalMs,omitempty"`
RequestHooksMs    *int                 `json:"requestHooksMs,omitempty"`
ResponseHooksMs   *int                 `json:"responseHooksMs,omitempty"`
LatencyBreakdown  map[string]int       `json:"latencyBreakdown,omitempty"`
```

The Hub consumer (`packages/nexus-hub/internal/storage/store/traffic_event_writer.go`)
maps these straight to the new columns. Existing producers that don't set them
write NULL — no behaviour change for pre-E50 traffic.

### 1.4 Shared phase timer — `packages/shared/traffic/phasetimer.go` (new)

Single source of truth for the phase enum + the timing helper. All three
forwarding services use this — no per-service phase definitions.

```go
package traffic

import "time"

// PhaseTimer records named phase durations against a single request lifetime.
// Concurrency: a single PhaseTimer is owned by one request; not safe for
// concurrent use.
type PhaseTimer struct {
    start  time.Time
    phases map[Phase]time.Duration
}

type Phase string

// Phase names — closed enum. Per source.
const (
    PhaseAuth           Phase = "auth_ms"           // ai-gateway only
    PhaseQuota          Phase = "quota_ms"          // ai-gateway only
    PhaseRouting        Phase = "routing_ms"        // ai-gateway only
    PhaseCacheLookup    Phase = "cache_lookup_ms"   // ai-gateway only
    PhaseReqAdapter     Phase = "req_adapter_ms"    // ai-gateway only
    PhaseRespAdapter    Phase = "resp_adapter_ms"   // ai-gateway only
    PhaseConnSetup      Phase = "conn_setup_ms"     // compliance-proxy only
    PhaseTlsHandshake   Phase = "tls_handshake_ms"  // compliance-proxy only
    PhaseIntercept      Phase = "intercept_ms"      // agent only
)

func NewPhaseTimer() *PhaseTimer {
    return &PhaseTimer{start: time.Now(), phases: make(map[Phase]time.Duration, 8)}
}

// Mark records the duration since the previous Mark/NewPhaseTimer call against
// the given Phase. Returns the recorded duration.
func (p *PhaseTimer) Mark(name Phase) time.Duration { /* impl */ }

// MarkBetween records an arbitrary duration against the given Phase.
// Used when a phase's start point is not the previous Mark (e.g. TTFB
// recorded from httptrace callback).
func (p *PhaseTimer) MarkBetween(name Phase, d time.Duration) { /* impl */ }

// Snapshot returns the map[string]int (ms) for serialization into
// latency_breakdown JSONB. Zero-valued phases are omitted.
func (p *PhaseTimer) Snapshot() map[string]int { /* impl */ }
```

### 1.5 Shared latency breakdown — `packages/shared/traffic/latencybreakdown.go` (new)

Typed wrapper for the JSONB shape consumed by the Hub writer + Control Plane
read paths. Producers write via `PhaseTimer.Snapshot()`; consumers read via
strongly-typed accessors.

```go
type LatencyBreakdown map[string]int

func (lb LatencyBreakdown) Get(p Phase) (int, bool) { /* … */ }
```

### 1.6 Unit tests — `packages/shared/traffic/phasetimer_test.go`

- `Mark` records correct durations against fake clock.
- `Snapshot` omits zero-valued phases.
- `MarkBetween` records the exact provided duration.
- Concurrent use detected: not tested (single-owner invariant documented; race
  detector confirms via service-level tests).

## Acceptance Criteria

- `npx prisma migrate dev` succeeds on a clean DB; the migration applies on a
  prod-shaped DB without locking `traffic_event` writes beyond standard ALTER
  cost.
- `\d+ traffic_event` in psql shows the 5 new columns and CHECK constraints.
- `go build ./packages/shared/...` succeeds; `phasetimer.go` and
  `latencybreakdown.go` exist.
- `TrafficEventMessage` JSON marshal/unmarshal round-trips the new fields when
  present and omits them when absent.
- `packages/shared/schemas/configtypes/traffic_event.go` carries the new fields after
  codegen.
- All Go unit tests in `packages/shared/traffic/...` pass with `-race -count=1`.

## Non-Goals

- Populating the new columns — that's S2 / S3 / S4 per service.
- Backfilling historical rows — that's S5.
- Admin API extensions or UI — that's S6 / S7.
