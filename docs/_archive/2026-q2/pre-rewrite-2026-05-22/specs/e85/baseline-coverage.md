# E85 — Phase 0 baseline coverage snapshot

> Captured 2026-05-21 on `feature/E85` worktree, base commit `05f147735`.
> Methodology: `go test -cover -count=1 ./...` per affected package.

## Active allowlist entries at start of E85 (40)

### Already-structural (keep — A/B/D/E/F per CLAUDE.md spec) — 18 entries

| Category | Package | Why structural |
|---|---|---|
| A | `agent/cmd/agent` (and 4 sibling main entrypoints) | `main()` wiring only |
| A | 11 × `cmd/*/wiring`, `cmd/*/platformshim`, `cmd/*/configdispatch`, `cmd/*/replay`, `cmd/*/breakglass` sub-pkgs | Various — see Phase-2/3 audits below |
| B | `shared/transport/bufconn` | Test helper (used from `*_test.go` of other pkgs) |
| B | `control-plane/internal/identity/idptest` | Test helper |
| B | `control-plane/internal/identity/authserver/store/storetest` | Test helper |
| B | `compliance-proxy/internal/testutil` | Test helper |
| C | `control-plane/internal/store` | DB-bound (skipped without `TEST_DATABASE_URL`) |
| D | `agent/internal/identity/keystore` | OS keychain |
| D | `agent/internal/host/trayipc` | OS systray IPC |
| D | `agent/internal/host/openbrowser` | OS browser launcher |
| D | `agent/ui` | Wails desktop app — `//go:embed all:frontend/dist` requires `npm run build` first |
| E | `shared/storage/spillstore/s3` | Real S3 client |
| E | `shared/transport/mq` | Real NATS JetStream client |
| E | `shared/transport/tlsbump` | Live TLS handshake |
| F | `agent/internal/platform` | Integration tests behind build tag |

### Types-only / sentinel-only packages — 10 entries (CLOSED in Phase 1 via `doc_test.go`)

| Package | Source-file shape | Resolution |
|---|---|---|
| `nexus-hub/internal/jobs/defs` (root) | 3 files: AlertRaiser + PgxPool interfaces, 0 funcs | `doc_test.go` → `[no statements]` |
| `nexus-hub/internal/storage/hubstore` | 2 sentinel errors only | `doc_test.go` → `[no statements]` |
| `shared/policy/decision` | Decision/Approve/Reject const + Hook type defs, 0 funcs | `doc_test.go` → `[no statements]` |
| `shared/schemas/configtypes` (root) | doc.go only | `doc_test.go` → `[no statements]` |
| `shared/schemas/configtypes/enums` | BumpStatus enum, 0 funcs | `doc_test.go` → `[no statements]` |
| `shared/schemas/configtypes/observability` | 7 metric-row type files, 0 funcs | `doc_test.go` → `[no statements]` |
| `agent/internal/platform/api` | Platform interface, 0 funcs | `doc_test.go` → `[no statements]` |
| `agent/internal/platform/darwin/flow` | State struct, 0 funcs | `doc_test.go` → `[no statements]` (with `//go:build darwin`) |
| `compliance-proxy/internal/config/shadow` | doc.go only | `doc_test.go` → `[no statements]` |
| `compliance-proxy/internal/tls/pinning` | Pin var only | `doc_test.go` → `[no statements]` |

### Genuine debt entries — 12 entries (Phase 2 in-flight)

| Package | Baseline % | Notes |
|---|---|---|
| `compliance-proxy/cmd/compliance-proxy/breakglass` | 0.0% | No tests; small (82 lines); ShadowProbe adapter + RunReplay drain loop |
| `compliance-proxy/cmd/compliance-proxy/replay` | 16.0% | 177 lines; only `parseSpoolFile` happy path covered |
| `compliance-proxy/cmd/compliance-proxy/configdispatch` | 18.0% | 347 lines; only key-registration assertion covered |
| `compliance-proxy/cmd/compliance-proxy/wiring` | 7.1% | Wiring package — see Phase 3 audit |
| `ai-gateway/cmd/ai-gateway/configdispatch` | 22.2% | 525 lines; subset of key handlers covered |
| `ai-gateway/cmd/ai-gateway/wiring` | 0.0% | Wiring package — see Phase 3 audit |
| `control-plane/cmd/control-plane/configdispatch` | 25.0% | 122 lines (smallest configdispatch) |
| `control-plane/cmd/control-plane/wiring` | 0.0% | Wiring package — see Phase 3 audit |
| `nexus-hub/cmd/nexus-hub/wiring` | 0.0% | 19 files / 10,088 lines — see Phase 3 audit |
| `agent/cmd/agent/wiring` | 0.0% | Wiring package — see Phase 3 audit |
| `agent/cmd/agent/platformshim` | 0.0% | OS-specific shim — see Phase 3 audit (likely re-categorize to D) |
| `ai-gateway/internal/ingress/proxy` | 94.4% | Just under threshold; residual is cache-HIT integration path |

## Targets

- Phase 1: 10 entries removed via `doc_test.go` (Day 1 — DONE).
- Phase 2: 6 packages reach ≥95% via Sonnet subagent dispatch (in flight).
- Phase 3: 6 wiring/OS-bound packages — Explore-agent audit + per-function classification → keep as A/D with detailed rationale OR carve out testable logic.

## Verification (after Phase 5)

```sh
cd $REPO_ROOT
bash scripts/check-go-coverage.sh                  # full sweep — must be green
bash scripts/check-go-coverage.sh --strict-allowlist # must report "0 removable entries"
```
