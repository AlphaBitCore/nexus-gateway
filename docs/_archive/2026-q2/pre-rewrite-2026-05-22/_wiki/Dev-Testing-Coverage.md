# Dev Testing Coverage

*Audience: contributors writing Go tests or proposing allowlist entries.*

Every Go package in `packages/**` must reach at least 95% statement coverage under `go test -cover -count=1 ./...`, or be listed in `scripts/.coverage-allowlist` with a concrete category and rationale. The 95% threshold is a proxy for **observable correctness**, not a number to hit by any means available. Coverage padding — calling a function only to bump the percentage, or asserting only `err == nil` without checking outputs — defeats the rule's purpose. This page explains the rule, the six allowlist categories, what counts as a qualifying assertion, and how to audit and clean up the allowlist.

---

## The 95% rule

The rule is binding for every Go package in `packages/**`. Enforcement runs in two modes:

- **Pre-commit hook** (`scripts/check-go-coverage.sh --staged`): runs only on packages with staged `*.go` changes, takes under a second, blocks the commit if a non-exempt package is below 95%.
- **Full sweep** (`npm run check:coverage` or `npm run check:all`): covers all modules; used by CI on every PR push.

The `[no statements]` packages (pure type definitions, doc-only files) are skipped automatically.

---

## Six allowlist categories

When a package genuinely cannot reach 95% under unit tests alone, it may be added to `scripts/.coverage-allowlist` with one of these categories:

| Tag | Category | Typical examples |
|---|---|---|
| (A) | `cmd/*` entry point — only `main()` wiring, no business logic | `cmd/ai-gateway`, `cmd/nexus-hub` |
| (B) | Test helper — not production code itself | `transport/bufconn`, `internal/testutil`, `idptest` |
| (C) | DB-bound — tests require a live PostgreSQL connection | `shared/storage/configstore`, quota stores |
| (D) | OS-bound — tests need kernel APIs, system keychain, or packet capture | `agent/internal/identity/keystore`, NE platform packages |
| (E) | Network-infra-bound — tests need real S3, NATS, or Redis Sentinel | `shared/storage/spillstore/s3`, `shared/transport/mq` |
| (F) | Integration-only — existing tests live behind a build tag | `shared/transport/tlsbump`, hub store packages |

**Not acceptable categories:**

- "Test would be slow." — Slow tests still count. Move them to `*_integration_test.go` behind a build tag, then allowlist under (F).
- "Will add tests later." — "Later" is not a category. Open a tracked issue and cite it if the work is deferred; otherwise write the tests now.
- "The code is too coupled." — Tight coupling that prevents tests is a defect. Refactor for testability; the seam pattern is `PgxPool` interface over `*pgxpool.Pool`, enabling pgxmock in tests.

Adding to the allowlist requires explicit user approval. The long-term goal is an empty allowlist.

---

## What counts as an observable-behavior assertion

A test is useful if it asserts that the code enforced a named invariant or handled a named failure mode. Examples from this repo:

- "OPEN circuits must never be selected." — test sends traffic during an OPEN window and asserts the circuit selector skips it.
- "Track-only quota must not block." — test sets up a track-only quota, sends a request over limit, and asserts HTTP 200, not 429.
- "Fail-open to inline on any spill error." — test makes the spill store return an error and asserts the request continues with inline body, not a 500.
- "Drift detection ignores JSON key order." — test provides two semantically equivalent JSON objects with different key ordering and asserts no drift is reported.

A test that calls `handler.Process(input)` and asserts only `err == nil` does not count — it passes regardless of whether the handler actually enforced the invariant. The test must check the output or the observable side-effect (HTTP status, DB row, Prometheus counter delta).

---

## The 5-step audit methodology

For each package below 95% that needs to reach threshold:

1. **Read the production source end-to-end.** No skimming. Trace every exported function back to a caller; map every error path.
2. **Identify the invariants the code enforces.** What conditions must always hold? What failure modes does the code guard against?
3. **Write tests targeting named failure modes.** Each test docstring states *why* the assertion matters — the invariant being protected, not just what is being tested.
4. **Verify** `go test -race -count=1 -cover ./...` is green and the coverage threshold is met.
5. **Confirm** `scripts/check-go-coverage.sh --strict-allowlist` does not surface any newly closeable allowlist entries.

The full methodology with worked examples from the 95% sweep is in [`docs/developers/workflow/coverage-allowlist-methodology.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/coverage-allowlist-methodology.md).

---

## Removing an allowlist entry

When a package's coverage genuinely reaches ≥95%:

1. Run `scripts/check-go-coverage.sh --strict-allowlist` — it lists allowlisted packages that now exceed the threshold and can be removed.
2. Delete the line from `scripts/.coverage-allowlist`.
3. Confirm the next run of `scripts/check-go-coverage.sh` still passes (a 95.5% → 94.9% sample wobble is real; verify on two separate runs before committing).
4. Commit with a message like `test(<pkg>): close <before>% → <after>%, drop from allowlist`.

---

## Common pgxmock seam pattern

Most DB-bound packages below 95% in this repo use a `*pgxpool.Pool` directly. The fix is to introduce a `PgxPool` interface covering the handful of methods the package uses (`Exec`, `Query`, `QueryRow`, `Begin`, `SendBatch`), have the constructor accept the interface, and use pgxmock in tests. The existing packages that have done this (hub store, control-plane store, ai-gateway store, cache layer) are the worked examples. Run `git log --oneline packages/nexus-hub/internal/store/` to see a representative migration commit.

---

## Canonical docs

- [`docs/developers/workflow/coverage-allowlist-methodology.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/coverage-allowlist-methodology.md) — the 5-step audit methodology, allowlist categories, and removal procedure
- [`scripts/check-go-coverage.sh`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/check-go-coverage.sh) — enforcement script (staged and full-sweep modes)
- [`scripts/.coverage-allowlist`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/.coverage-allowlist) — the live allowlist with categories and rationales
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — "Unit test coverage ≥95% per Go package" binding rule

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Dev Code Review Checklist](Dev-Code-Review-Checklist) · [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep)
