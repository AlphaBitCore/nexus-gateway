# E85 — Unit-test coverage 95% (close the allowlist)

> Status: In-progress

## Summary

The repo's binding rule (CLAUDE.md → "Unit test coverage ≥95% per Go package") is enforced by `scripts/check-go-coverage.sh`. Packages below 95% must either be brought up to threshold **or** be listed in `scripts/.coverage-allowlist` with one of the structural categories A-F and a one-line rationale. The long-term goal is an **empty allowlist**.

When E85 was scoped (2026-05-19), the allowlist held **40 active entries** — some genuinely structural (cmd entry points, network-infra-bound test helpers), some pre-existing debt (open-source readiness backlog, Phase-7 carve-outs). E85's critical gate: every remaining allowlist entry is structurally untestable per A-F with a concrete per-entry rationale.

## Functional requirements

- **FR-1** Every Go package in `packages/**` reaches ≥95% statement coverage **OR** is listed in `scripts/.coverage-allowlist` under a category A-F with a one-line rationale.
- **FR-2** Every test added asserts **observable business behavior** (a state change, a named log line, a HTTP status, a DB row written / not-written) — not coverage padding (`err == nil` without output assertion, function called only to bump the percentage).
- **FR-3** Types-only / sentinel-error / interface-only packages convert from `[no test files]` → `[no statements]` via a minimal `doc_test.go` (Phase 1) — these report `[no statements]` to the gate and auto-pass without needing an allowlist line.
- **FR-4** Production-code seams introduced for testability (PgxPool interface, narrow `replayer` interface, etc.) are minimal — narrow enough that `*pgxpool.Pool` / the concrete prod type satisfies them without an adapter, and the prod call sites do not change.

## Non-functional requirements

- **NFR-1** `scripts/check-go-coverage.sh` runs clean from scratch (no allowlist fallback for new code).
- **NFR-2** `scripts/check-go-coverage.sh --strict-allowlist` reports "0 removable entries" — every line on the allowlist still requires its structural justification.
- **NFR-3** Per-test docstrings name the failure mode they're guarding against where the failure mode is non-obvious from the test name alone.

## User roles & personas

- **Repo contributor** (primary) — adds a new function in a package; the pre-commit gate enforces 95% on the package the function lives in.
- **Reviewer** — reads `.coverage-allowlist` to understand which gaps are structural; relies on the per-entry rationale.

## Constraints & assumptions

- (1) Pre-GA codebase — no installed users; refactoring for testability is greenfield (no backwards-compat).
- (2) `go.work` / multi-module — coverage is measured per Go module (5 modules: `agent`, `ai-gateway`, `compliance-proxy`, `control-plane`, `nexus-hub`, `shared`).
- (3) CI runs the full gate via `npm run check:coverage`; pre-commit runs `--staged`.
- (4) Tests must pass under `-race`; production seams that fail under `-race` are not acceptable.

## Glossary

| Term | Meaning |
|---|---|
| **Allowlist** | `scripts/.coverage-allowlist` — packages exempt from the 95% threshold, each tagged with a category and rationale. |
| **(A) cmd entry point** | `cmd/*` `main()` wiring with no business logic. |
| **(B) test helper** | Used only from `*_test.go` of other packages (bufconn / testutil / idptest / storetest). |
| **(C) DB-bound** | Tests require live PostgreSQL — existing `*_integration_test.go` skip without `TEST_DATABASE_URL`. |
| **(D) OS-bound** | Tests need kernel APIs / system keychain / packet capture / WinDivert / NE bridge. |
| **(E) Network-infra-bound** | Tests need real S3 / NATS / Valkey-Sentinel / live TLS handshake. |
| **(F) Integration-only** | Tests exist behind a build tag (`//go:build integration`). |
| **`doc_test.go`** | A one-line `package x_test` file added to types-only / sentinel-only packages so `go test` reports `[no statements]` instead of `[no test files]`. The gate auto-passes `[no statements]`. |

## MoSCoW priority

- **Must** — FR-1, FR-2, NFR-1, NFR-2.
- **Should** — FR-3 (cleanest path for sentinel-only packages), FR-4 (minimal production seams).
- **Could** — Wiring packages re-categorized D/E if their construction calls into OS / network — improves rationale precision.
- **Won't** — Coverage on test helpers themselves (B), integration tests (F), DB-bound packages (C) without docker-compose Postgres in the test runner.

## Stories

| Story | Subject | Status |
|---|---|---|
| S1 | Phase-0 baseline: enumerate active allowlist, snapshot per-package coverage, classify each entry | ✅ Shipped |
| S2 | Phase-1 quick wins: convert 10 types-only packages from `[no test files]` to `[no statements]` via `doc_test.go`; remove from allowlist | ✅ Shipped |
| S3 | breakglass 0% → 100%: narrow interface seams (`shadowProbeClient`, `replayer`), `runReplayWith` test indirection | ✅ Shipped |
| S4 | replay 16% → 96.7%: `defaultBuildSink` injectable seam, skip-and-warn on malformed JSON, 20 behavior tests | ✅ Shipped |
| S5 | cp configdispatch 18% → 95.5%: sqlmock-driven all 9 key handlers, nil-dep degradation, DB-backed paths | ✅ Shipped |
| S6 | aig configdispatch 22.2% → 98.3%: 69 tests across 18 registered shadow keys (pgxmock + slog-buffer + fake stubs) | ✅ Shipped |
| S7 | cplane configdispatch 25% → 95.8%: extended existing test to 8 handlers (log_level/observability/BuildConfigChangedCallback) | ✅ Shipped |
| S8 | ingress/proxy 94.4% → 95.1%: 7 fakeexec-driven tests closed last non-cache-HIT branches; dropped from allowlist | ✅ Shipped |
| S9 | Phase-3 structural audit: 5 wiring packages + platformshim audited (5,400 LOC / 97 files); per-function ledger drafted; `platformshim` re-categorized (A)→(D) | ✅ Shipped |
| S10 | Phase-4 decision log: D-1 through D-9 captured; per-package closure ledger in D-7 | ✅ Shipped |
| S11 | Phase-5 self-audit + final gate run: 4Q × 2 rounds; `--strict-allowlist` returns 0 removable | 🟢 In-progress |

## Reading order

1. `scripts/.coverage-allowlist` — the live allowlist.
2. `scripts/check-go-coverage.sh` — the gate logic.
3. `docs/developers/workflow/coverage-allowlist-methodology.md` — the 5-step audit pattern.
4. `docs/developers/specs/e85/decision-log.md` — every E85 architectural decision.
5. `docs/developers/specs/e85/baseline-coverage.md` — Phase-0 snapshot.
