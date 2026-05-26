# Coverage Allowlist — Methodology

> Binding rule: every Go package in `packages/**` ≥ 95% statement coverage,
> OR listed in [`scripts/.coverage-allowlist`](../../../scripts/.coverage-allowlist) with a concrete category and rationale. Canonical rule text lives in `CLAUDE.md` → "Unit test coverage ≥95%"; this doc is the **methodology** the allowlist follows, not the rule itself.

This doc is what to read before:

- Writing a new test pass that closes a < 95% gap.
- Proposing a new allowlist entry.
- Running the periodic "can any allowlist entry be removed?" sweep.

## The 95% / allowlist contract

The rule's intent is **observable correctness**, not a coverage number. A package at 100% with single-line happy-path tests fails the spirit; a package at 90% where every assertion is tied to a named failure mode passes it. The allowlist exists for packages where the *correctness* signal genuinely lives outside `go test -cover` (entry points, OS / DB / network-infra coupling, integration suites behind build tags).

**Long-term goal: an empty allowlist.** Every entry should be a temporary acknowledgement, not a permanent comfort.

## Allowlist categories

When proposing an entry, pick one of these and include a one-line trailing rationale in the allowlist file:

| Tag | Category | Examples |
|---|---|---|
| (A) | `cmd/*` entry point — main wiring, no business logic | `cmd/ai-gateway`, `cmd/agent/wiring` |
| (B) | Test helper | `bufconn`, `testutil`, `idptest`, `storetest` |
| (C) | DB-bound — tests need live PostgreSQL | `shared/configstore`, `nexus-hub/internal/quotastore` |
| (D) | OS-bound — tests need kernel APIs / system keychain / packet capture | `agent/internal/identity/keystore`, `agent/internal/platform/darwin/flow` |
| (E) | Network-infra-bound — real S3, NATS, Redis Sentinel | `shared/storage/spillstore/s3`, `shared/transport/mq` |
| (F) | Integration-only — existing tests live behind a build tag | `shared/transport/tlsbump`, `nexus-hub/internal/storage/hubstore` |

**Carve-outs that are NOT acceptable categories:**

- "Test would be slow." — Slow tests still count. Move them to `_integration_test.go` with a build tag if the suite gets too long; allowlist under (F).
- "We'll add tests later." — "Later" is not a category. Open a tracked issue + cite it in the rationale.
- "The code is too coupled." — Tight coupling that prevents tests is itself a defect; refactor for testability.

## Audit methodology (the 5 steps)

For every package below 95% that you intend to lift up to threshold:

1. **Read the production source end-to-end** — no skimming. Trace every exported function back to a caller; map every error path.
2. **Identify the invariants the code enforces.** Examples encountered in this repo:
   - "OPEN circuits must never be selected."
   - "Track-only quota must not block."
   - "Fail-open to inline on any spill error."
   - "Drift detection ignores JSON key order."
   - "Fingerprint stable under transient kernel errors."
3. **Write tests targeting named failure modes**, not just the happy path. Where possible, cite the prior incident or memory anchor that motivated the invariant. Past incident-driven tests in this repo reference:
   - "2026-05-13 prod incident — NRN mismatch"
   - "agent-desktop-type-mismatch-bug"
   - "the v1 incident 'VK pinned to dead cred'"
   - "2026-05-16 personal-VK NULL org via hotfix `da073580`"
4. **Each test docstring states *why* the assertion matters**, not just what it asserts. This is the difference between coverage that protects from regressions and coverage that exists to pass a CI gate.
5. **Verify** `go test -race -count=1 -cover ./...` is green, and `scripts/check-go-coverage.sh --strict-allowlist` doesn't surface the package as a removal candidate elsewhere.

## Removing an allowlist entry

When the package's coverage genuinely reaches ≥ 95%:

1. Run `scripts/check-go-coverage.sh --strict-allowlist` — it lists allowlisted packages that now exceed the threshold and can be removed.
2. Delete the line from `scripts/.coverage-allowlist`.
3. Confirm the next run of `scripts/check-go-coverage.sh` still passes (a 95.5 → 94.9 wobble is real; don't ratchet on a single sample).
4. Commit with a message like `test(<pkg>): close <before> → <after>%, drop from allowlist`.

## Reference: the 2026-05-16 sweep

The first major application of this methodology brought 8 production packages from 0%-or-single-digit coverage to 52–100%. The full report is preserved in git history under commits between `adfd5ebc` ("promote 95% coverage program plan + baseline + gap-map") and `dc8ac1a0` ("close 94.3 → 95.4%, drop from allowlist"). When auditing future sweeps, those commits are the worked example.

## Related

- [`CLAUDE.md`](../../../CLAUDE.md) — canonical binding rule text.
- [`scripts/check-go-coverage.sh`](../../../scripts/check-go-coverage.sh) — enforcement script.
- [`scripts/.coverage-allowlist`](../../../scripts/.coverage-allowlist) — the live allowlist.
- [`.cursor/rules/unit-test-coverage-95.mdc`](../../../.cursor/rules/unit-test-coverage-95.mdc) — IDE-side rule surfacing.
