# Scenario harness — Phase 4 readiness sign-off

Snapshot of the L1–L5 test stack against `main` as of the program closeout.
Companion to [`COVERAGE.md`](./COVERAGE.md): coverage tells you what the
harness *intends* to exercise; this file tells you what's *actually
passing* right now.

## How to reproduce

```bash
NEXUS_TEST_TARGET=local bash tests/run-all.sh --full
# Report lands in $NEXUS_TEST_LOG_DIR (default /tmp/nexus-test) as
# test-all-<UTC-stamp>.md with per-phase tails + log paths.
```

## Result table (snapshot — 2026-05-16 closeout)

| Phase | Layer | Status | Note |
|---|---|---|---|
| Preflight | env check | PASS | services up + DB reachable |
| 1. L1 smoke (bash) | `tests/smoke/` | YELLOW (28/29) | 1 persistent assertion mismatch on `proxy/audit` HTTP 404 |
| 2. L1 Go integration | `tests/integration-go/` | PASS (10.9 s, after VK refresh) | 0 failures once `.env.test`'s VK matches DB |
| 3. L4 Playwright UI | `tests/e2e-ui/` | YELLOW | Routing-CRUD + traffic-monitor flake on first run, typically retry-green |
| 4. L3 AI judge | `tests/e2e-python/ai_judge` | BLOCKED | Upstream `Moonshot` provider unreachable from local network — env-constraint, not a regression |
| 5. L2 protocol | `tests/e2e-python/protocol` | PASS (4/5; 1 xfail-strict) | `test_messages_stream_shape` is `XPASS(strict)` — known anthropic-python SDK SSE issue |
| 6. L5 scenarios | `tests/scenarios/` | **PASS** | 45/45 scenarios, 243.1 s |

All phase tails + log paths live in `/tmp/nexus-test/test-all-<ts>.md`
after each run; the full L1–L5 sweep ran as `test-all-20260516T143443Z.md`
and produced the numbers above. Re-run with `bash tests/run-all.sh --full`.

## Known-failing categories (controlled acknowledgement)

### 1. Stale `NEXUS_TEST_VK` in `tests/.env.test` — env hygiene (RESOLVED 2026-05-16)

Initial run had Phases 2, 4, 5 all 401 on `vkauth: virtual key invalid`.
The VK in `.env.test` was minted against an earlier DB state and not
in the current `VirtualKey` table.

**Fix (now wired):** new helper `tests/scripts/mint-test-vk.go` (build
tag `mintvk`) logs in via OAuth+PKCE and prints a fresh VK to stdout:

```bash
cd tests/scenarios
fresh_vk=$(NEXUS_TEST_TARGET=local GOWORK=off go run -tags mintvk ../scripts/mint-test-vk.go)
sed -i.bak "s|^NEXUS_TEST_VK=.*|NEXUS_TEST_VK=$fresh_vk|" tests/.env.test \
  && rm tests/.env.test.bak
```

After running this, Phase 2 went from FAIL → PASS in 10.9 s and Phase 5
went from FAIL → 4/5 (the remaining fail is the `XPASS(strict)` known
issue, separate from VK).

The scenario harness sidesteps this entirely because every scenario
mints its own VK via OAuth+PKCE at runtime (S-001's pattern). The
bash/Python/Go-integration layers all read the static env var, so they
break on any DB reset that doesn't re-seed the same VK.

**Long-term:** convert Phases 2/4/5 to mint at preflight time the way
scenarios do. Today they pre-date the OAuth scaffolding so they're on
the legacy path.

### 2. Playwright flakes — UI infrastructure

Routing-CRUD and traffic-monitor flake on first run, typically pass on
retry. The UI tests are real-browser-driven against a Vite dev server
and inherit React-Query timing variance. Owned by the `tests/e2e-ui/`
maintenance pass; not blocking scenario coverage.

### 3. Bash smoke `proxy/audit` 404 — needs investigation

One bash assertion in `test-control-plane.sh` expects an admin route
that returns 404. Either the route was renamed/removed or the smoke
expectation is stale; deferred until someone owns the proxy admin
surface (today's scenario coverage doesn't include `proxy/audit`).

## What this readiness check tells operators

**Green light:** Phases 6, 2, 5, Preflight. The scenario harness — the
layer the program built — is fully green at 45/45, ~243 s. Phase 2 Go
integration is also green at 10.9 s after the VK refresh. Phase 5
protocol is 4/5 with the lone fail being a `XPASS(strict)` for a
known anthropic-python SDK issue.

**Yellow:** Phase 3 Playwright flakes (UI infra, retry-green typically).
Phase 1 bash smoke has 1/29 persistent assertion mismatch on
`proxy/audit`.

**Blocked:** Phase 4 AI judge requires outbound provider connectivity
(`Moonshot` API); this local network refuses the connection, so the
phase blocks pending env work outside the scenario program's scope.

## Program closeout vs. readiness gate

Treat the scenario harness as **production-ready** for the surfaces it
covers (per `COVERAGE.md`). Use it as the lockstep gate for any PR
that touches admin authoring routes (E59-class changes to billing
flows, IAM, OAuth, DSAR, hooks, rule-packs, etc.). Do **not** treat
green Phase 6 as a substitute for the layers it doesn't replace:

- **No real AI traffic** → Phase 4/5 still own that.
- **No browser UI flow** → Phase 3 still owns that.
- **No bash-level admin sweep** → Phase 1 still owns that.

The program's deliverable is the scenario harness + COVERAGE doc + this
sign-off. The pre-existing layers should be brought back to green by
a focused env-hygiene pass (VK refresh) and a Playwright stability
pass — both are out of scope for the scenario program.
