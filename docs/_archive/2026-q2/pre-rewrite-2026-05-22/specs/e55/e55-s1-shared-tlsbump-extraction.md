# E55-S1 — Extract `shared/tlsbump` from `compliance-proxy/internal/proxy`

**Epic:** E55 (`docs/developers/specs/e55/e55-tls-bump-trinity.md`)
**Status:** in implementation 2026-05-15

## User Story

> As a platform engineer maintaining the MITM/TLS-bump core, I want a single
> Go package consumed by both compliance-proxy and the macOS agent so that
> behavior changes (new streaming modes, capture flags, pinning rules,
> protocol upgrades) ship to both ingresses simultaneously and cannot drift.

## Tasks

### S1.T1 — `shared/audit` upgrade
- Move `packages/compliance-proxy/internal/audit/types.go` → `packages/shared/audit/types.go`. Bring the `Writer`, `AuditEvent`, `QueueInspector`, `Stage*` and related field structs.
- Existing cp `internal/audit/types.go` becomes a one-line re-export shim during transition; deleted in S1.T6.
- Agent's `internal/audit/queue.go` adds a `WriterAdapter` that satisfies `shared/audit.Writer`. Internally it maps `audit.AuditEvent` → agent's existing `Event` row (which is sqlite-shaped).

### S1.T2 — `shared/compliance` upgrade
- Move `packages/compliance-proxy/internal/compliance/emitter.go` (528 LOC) → `packages/shared/compliance/audit_emitter.go`.
- Move `packages/compliance-proxy/internal/compliance/types.go` (already mostly aliases) — fold its `AuditInfo` struct into `shared/compliance/audit_emitter.go`. The other type aliases (`Decision`, `HookConfig`, etc.) are re-exports of `shared/hooks` types — drop the alias file and let cp callers import `shared/hooks` directly.
- Move `packages/compliance-proxy/internal/compliance/metrics.go` (14 LOC) → `packages/shared/compliance/audit_metrics.go`.

### S1.T3 — `shared/domainpolicy` extraction
- Move `packages/compliance-proxy/internal/domainpolicy/{types.go, engine.go, engine_test.go}` → `packages/shared/policy/domainpolicy/`.
- Resolve any package-name collisions with `shared/configtypes.InterceptionDomain` — they are different shapes (cp's row is denser); rename cp's to `domainpolicy.InterceptionDomain` with a short doc comment explaining the relationship.

### S1.T4 — `shared/tlsbump` package creation
- Create `packages/shared/transport/tlsbump/` with files (1:1 mapping from cp/internal/proxy):

  | new path | source |
  |---|---|
  | `bump.go` | `cp/internal/proxy/bump.go` |
  | `forward_handler.go` | `cp/internal/proxy/forward_handler.go` |
  | `sse.go` | `cp/internal/proxy/sse.go` |
  | `passthrough.go` | `cp/internal/proxy/passthrough.go` |
  | `reject.go` | `cp/internal/proxy/reject.go` |
  | `pinning.go` | `cp/internal/proxy/pinning.go` |
  | `upstream.go` | `cp/internal/proxy/upstream.go` |
  | `utls_dialer.go` | `cp/internal/proxy/utls_dialer.go` |
  | `hello_capture.go` | `cp/internal/proxy/hello_capture.go` |
  | `tunnel.go` | `cp/internal/proxy/tunnel.go` |
  | `markercontext.go` | `cp/internal/proxy/markercontext.go` |
  | `markerhook.go` | `cp/internal/proxy/markerhook.go` |
  | `*_test.go` | the 8 test files |

- Update package declaration to `package tlsbump`.
- Update imports: replace `cp/internal/compliance` → `shared/compliance`, `cp/internal/domainpolicy` → `shared/domainpolicy`, `cp/internal/audit` → `shared/audit`, `cp/internal/metrics` → an injectable `Metrics` struct passed into `Deps` (do NOT import cp's prometheus registry).
- Add `tlsbump.Deps` struct + `tlsbump.HandleConnection` entrypoint:

```go
package tlsbump

type Deps struct {
    Logger          *slog.Logger
    Inspector       Inspector       // wraps shared/compliance.Pipeline + AuditEmitter
    AuditEmitter    *compliance.AuditEmitter
    DomainEngine    *domainpolicy.Engine
    PolicyResolver  *compliance.PolicyResolver
    DomainSnapshot  *atomic.Pointer[traffic.DomainSnapshot]
    AdapterRegistry *traffic.AdapterRegistry
    PayloadCapture  *payloadcapture.Store
    StreamPolicy    streampolicy.Policy
    Pinning         *PinningStore       // already in tlsbump
    Reject          RejectConfig        // already in tlsbump
    Upstream        *UpstreamTransport  // already in tlsbump
    GetCert         func(*tls.ClientHelloInfo) (*tls.Certificate, error)
    Metrics         Metrics             // injected; implementations register their own promauto vars
    PerHookTimeout  time.Duration
    TotalTimeout    time.Duration
    ParallelHooks   bool
}

type Metrics interface {
    ObserveTLSHandshakeMs(float64)
    // ... other observers used by forward_handler / sse / etc
}

func HandleConnection(ctx context.Context, conn net.Conn, dst Destination, deps Deps) error {
    // Calls BumpConnection internally with deps.GetCert + the bumpOption setters
    // populated from Deps.
}
```

- Existing `BumpConnection` + `BumpOption` setters stay public (transition period). `HandleConnection` is the new preferred entry; cp + agent both use `HandleConnection`.

### S1.T5 — `compliance-proxy` switchover
- `cp/internal/proxy/listener.go` calls `tlsbump.HandleConnection(ctx, conn, dst, deps)` instead of `proxy.BumpConnection(...)`. The `listener` constructs `deps` from cp's existing wiring (`NewAuditEmitter`, `NewPolicyResolver`, etc.).
- Delete the 23 files moved in S1.T4 from `cp/internal/proxy/`. `listener.go`, `listener_*_test.go`, and cp's metrics shim (a `Metrics` impl that registers cp's promauto vars) are the only files left.
- All cp tests pass: `go test -race -count=1 ./packages/compliance-proxy/...`.

### S1.T6 — Re-export shim deletion
- Delete the transition shims left in cp's `internal/audit/`, `internal/compliance/`, `internal/domainpolicy/` (after fixing all cp imports to point at `shared/*`).

## Acceptance Criteria

- [ ] `find packages/compliance-proxy/internal/proxy -type f -name '*.go' | wc -l` returns ≤ 5 (listener.go + its tests + cp Metrics impl).
- [ ] `go build ./...` from repo root passes.
- [ ] `go test -race -count=1 ./packages/shared/transport/tlsbump/... ./packages/compliance-proxy/...` passes.
- [ ] `go vet ./packages/shared/transport/tlsbump/...` clean.
- [ ] No file under `packages/shared/transport/tlsbump/` imports anything from `packages/compliance-proxy/...` or `packages/agent/...` (enforced by `grep -r "compliance-proxy\|/agent/" packages/shared/transport/tlsbump/` returning empty).
- [ ] `go.mod` of `shared/` does NOT gain a new third-party dep beyond what cp's proxy package already pulled (vetted set in CLAUDE.md).
- [ ] Smoke check: `cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml` boots cleanly and a CONNECT-then-bumped curl through it returns 200 OK against `https://api.openai.com/v1/chat/completions`.

## Out of Scope (explicit)

- Agent migration (covered by E55-S5).
- New streaming modes or capture knobs (S2 / S3 reuse existing).
- HTTP/2 added to agent (S4 — depends on S5 landing first).
- ai-gateway integration (Won't per requirements doc).
