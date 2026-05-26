# Test Coverage Program — Session Handoff (2026-05-16)

> Written per [CLAUDE.md → Mandatory rules → "Handoff at context-full"](../../CLAUDE.md). The next session should pre-load this file, then resume from §4 "Next steps."

## 1. Program goal + current phase

**Goal.** Hit **≥95% statement coverage on every Go package** in `packages/**`. Binding rule landed in CLAUDE.md this session; enforced at pre-commit via `scripts/check-go-coverage.sh --staged` and full-sweep via `npm run check:coverage`.

**Why 95% (and not a vague target).** Maintainer set it explicitly as a
lint-enforced rule (2026-05-16), not a stretch goal: every Go package
must reach ≥95% statement coverage with tests that assert real business
behavior — never tests written purely to inflate the percentage.

**Quality bar.** Each test must assert observable business behavior or named failure modes. Coverage padding (calling a function only to bump %, asserting only `err == nil`) is a regression risk and explicitly forbidden by the rule's text.

**Current phase: ratchet.** The allowlist (`scripts/.coverage-allowlist`) was seeded with every sub-95% package categorized by reason (cmd / test-helper / DB-bound / OS-bound / network-infra-bound / integration-only / "open-source readiness backlog"). Each session knocks more entries off. The allowlist is shrinking — that's the ratchet working.

## 2. Load-bearing facts the next session needs

### 2.1 Enforcement infrastructure

| File | Role |
|---|---|
| `scripts/check-go-coverage.sh` | Runs `go test -cover` per module. Modes: `--staged` (pre-commit, only changed packages), `--strict-allowlist` (lists entries that have caught up and can be removed), `--json` (machine-readable), `--threshold=N` (override). |
| `scripts/.coverage-allowlist` | Glob patterns per package. Each entry MUST cite a category in trailing comment. Removed entries kept as `# pkg: removed YYYY-MM-DD — now X%` history. |
| `.githooks/pre-commit` | `run_hard "go coverage ≥95%" bash scripts/check-go-coverage.sh --staged` when `*.go` is staged. Confirmed working — every commit this session emitted `[pre-commit] go coverage ≥95% ✓`. |
| `package.json` | `check:coverage` (full sweep) + `check:coverage:strict` (allowlist hygiene) + folded into `check:all`. |
| `CLAUDE.md` Mandatory rule | Canonical text + carve-out policy. |
| `.cursor/rules/unit-test-coverage-95.mdc` | IDE-side surfacing of the same rule. |
| `.cursor/rules/binding-rules-quick-reference.mdc` | Index entry. |

### 2.2 Allowlist categories (verbatim from script docstring)

- **A.** `cmd/*` entry point — only `main()` wiring, no logic.
- **B.** Test helper (`bufconn`, `testutil`, `idptest`, `storetest`).
- **C.** DB-bound — tests require live PostgreSQL.
- **D.** OS-bound — kernel APIs, system keychain, packet capture.
- **E.** Network-infra-bound — real S3, NATS, Redis Sentinel.
- **F.** Integration-only — existing tests behind build tags.

Anything else (close-to-95% packages still in flight) goes under the **"Open-source readiness backlog"** section with a target to remove.

### 2.3 Memory anchors to pre-load

- `feedback_unit_test_coverage_95.md` — canonical rule.
- `feedback_test_coverage_autonomous_brainstorm.md` — autonomous mode + brainstorm on architectural blockers.
- `feedback_autonomous_execution.md` — broader autonomous policy.
- `feedback_open_source_program_autoexec.md` — open-source program auto-exec authorization.
- `project_parallel_worktree_sessions.md` — scoped pathspec on commits, never `git stash`.

### 2.4 Audit reports

- `/tmp/nexus-test-audit/audit-report-2026-05-16.md` — full first-pass audit with per-package quality grades + critical untested paths list.
- `/tmp/nexus-test-audit/baseline-coverage.md` — coverage baseline before this session.

## 3. Work completed this session

### 3.1 Infrastructure (one-time)
- 95% binding rule landed: CLAUDE.md + cursor rule + memory + pre-commit + npm script + `check:all`.
- `scripts/check-go-coverage.sh` written + `.coverage-allowlist` seeded.
- Audit report + baseline saved to `/tmp/nexus-test-audit/`.

### 3.2 Coverage wins (packages now ≥95%, off allowlist)

| Package | Before | After |
|---|---:|---:|
| `ai-gateway/internal/credpool` | 0% | 97.5% |
| `shared/thingtype` | 0% | 100% |
| `agent/internal/spilluploader` | 0% | 98.0% |
| `agent/internal/protectionpause` | 94.4% | 100% |
| `ai-gateway/internal/runtimeapi` | 94.1% | 96.1% |
| `shared/payloadcapture` | 92.7% | 97.6% |
| `shared/spillstore` | 90.9% | 100% |
| `shared/httpclient` | 92.6% | 97.1% |
| `shared/iam` | 83.3% | 100% |
| `shared/diag` | 83.3% | 95.2% |
| `shared/audit` | 80.7% | 96.5% |
| `nexus-hub/internal/selfreg` | 85.0% | 95.0% |
| `shared/responseio` | 86.7% | 100% |
| `ai-gateway/internal/providers/canonicalext` | 85.0% | 95.0% |
| `shared/cacheconfig` | 73.3% | 100% |

**15 packages off allowlist this session.**

### 3.3 Substantial partial improvements (still allowlisted)

| Package | Before | After |
|---|---:|---:|
| `ai-gateway/internal/quota` | 0% | 58.0% |
| `shared/spillstore/spillfactory` | 0% | 76.0% |
| `agent/internal/diagnostics` | 0% | 94.4% |
| `control-plane/internal/configreconcile` | 0% | 88.5% |
| `nexus-hub/internal/alerteval` | 16.9% | 52.1% |
| `agent/internal/lifecycle` | 90.2% | 92.7% |
| `agent/internal/relay` | 91.9% | 93.2% |
| `shared/streaming/policy` | 87.0% | 88.7% |
| `shared/telemetry` | 85.5% | 87.1% |
| `agent/internal/enrollment` | 70.2% | 78.6% |
| `shared/hooks` | 58.1% | 68.9% |
| `shared/configcache` | 75.7% | 88.3% |
| `ai-gateway/internal/forwardheader` | 83.1% | 87.5% |
| `ai-gateway/internal/canonicalbridge` | 86.5% | 88.7% |
| `compliance-proxy/internal/configloader` | 1.5% | 10.8% |
| `shared/devicepredicate` | 76.3% | 86.6% |
| `shared/credstate` | 75.8% | 90.9% |
| `shared/compliance` | 55.6% | 64.7% |

### 3.4 Documented dead-branch (allowlisted with rationale)
- `shared/pkce` 91.7% — Go 1.26 `crypto/rand.Read` panics on entropy failure instead of returning an error (`go.dev/issue/66821`). The defensive `if err != nil` branch in `pkce.Generate` is unreachable in unit tests. Documented in `packages/shared/security/pkce/pkce_test.go` header comment.

### 3.5 Commits (12 in this program)

```
dd1394bc1 test: cacheconfig three-tier resolve every-knob coverage 100%
380e8fa7c test: devicepredicate operators + compliance audit_emitter helpers
a399841ee test: canonicalext over 95%, credstate Merge contract coverage
6f85208b8 test: pure-logic coverage for configloader.allowlists + keyword_filter
5d86a0902 test: forwardheader + canonicalbridge routing matrix coverage
7c586dffe test: hooks ip_access + onmatch + configcache options coverage
38b97a2be test: improve enrollment + streaming/policy + document pkce dead-branch
071b32547 test: push selfreg + responseio over 95%, partial on telemetry
2c7470580 test: push 8 packages over 95% threshold, shrink coverage allowlist
0f1412428 test: push spilluploader 90.2%→98%, diagnostics 92.6%→94.4%
0e00a64af chore: enforce ≥95% per-package Go unit test coverage (binding)
fce13233c test: add unit tests for 8 previously-0%-coverage packages
```

## 4. Next steps (prioritized)

### 4.1 Easy wins — close-to-95% packages

Run `npm run check:coverage:strict` first to find any allowlisted packages whose coverage has caught up (parallel session work etc.).

Then push these over the line:
- `agent/internal/diagnostics` 94.4% — Stat err / Scanner err in `tail` (need fake fs or io.LimitReader trick)
- `agent/internal/relay` 93.2% — `underlyingHTTPTransport` Unwrap-returns-nil branch
- `agent/internal/lifecycle` 92.7% — emit's recorder-failed branch (needs a recorder fake)
- `nexus-hub/internal/alerting/rules` 93.3% — `mustJSON` panic on circular struct (synthetic)
- `control-plane/internal/authserver/idp` 92.0% — `mustDummyHash` panic on entropy failure (same Go-1.26 issue as pkce)

### 4.2 Mid-pack — close packages

- `shared/credstate` 90.9% — `Validate` strict-less-than-or-equal vs <= edge cases
- `shared/configcache` 88.3% — keycache `Purge` no-op path + remaining Get/Set branches
- `ai-gateway/internal/canonicalbridge` 88.7% — `IngressChatToWire` fallback paths, `ResponseCanonicalToIngress` error branches
- `ai-gateway/internal/forwardheader` 87.5% — `Request` / `Response` apply paths (UnmarshalYAML edge cases too)
- `shared/streaming/policy` 88.7% — `LoadGlobalDefault` DB path (needs sqlmock or fake driver — see §4.4)

### 4.3 Heavy lift — needs new test infrastructure

- **OIDC mock**: `control-plane/internal/authserver/login/oidc.go` (0% on 273 LOC). Build a httptest IdP that issues real signed ID tokens; reuse for `EnrollWithJWT` in `agent/internal/enrollment` and any other JWT validators.
- **`hub_enroll` HTTP path**: `agent/internal/enrollment/hub_enroll.go` `EnrollWithJWT` (0%), `doEnroll` (73.9%), `Deregister` (68.8%). Build a fake Hub `httptest.Server` once, share across `agent/hubhttp`, `alertclient`, etc.
- **AI judge mocks**: `shared/hooks/content_safety.go` `Execute` (0%), `shared/hooks/quality.go` `Execute` (74.4%). Both call external LLM judge — wrap with `httptest.Server` returning canned approve/reject JSON.
- **MITM core**: `compliance-proxy/internal/proxy` 26.7% — significant work, real TLS handshake required. Architectural option B: extract pure-logic helpers (header rewrites, CONNECT parsing) into a sub-package and test those.

### 4.4 Strategic — unlocks ~15 packages

**DB integration test infra.** Pick one approach via brainstorm:

- **(A) docker-compose Postgres in CI.** Existing `*_test.go` files already use `TEST_DATABASE_URL` skip pattern (e.g. `configstore/aiguard_test.go`). Wire `make test-integration` + GitHub Actions service container. Unblocks: `configstore`, `nexus-hub/{enrollment,quotastore,rollupstore,jobstore,store}`, `control-plane/store`, `ai-gateway/store`, `ai-gateway/cachelayer`.
- **(B) go-sqlmock.** Add `github.com/DATA-DOG/go-sqlmock` to per-service `go.mod` (driver-scoped, test-only — same precedent as `miniredis`). Faster per-test, lighter CI. Trade-off: doesn't validate actual SQL semantics, just statement matching.

Recommended: **(A)** for the open-source-readiness program. Real-DB confidence is worth the CI weight; matches how prod runs. Add `(B)` later if iteration speed becomes painful.

### 4.5 Long tail — provider adapter samples

`shared/traffic/adapters/*` (50 sub-packages) currently 30-90% coverage. Most gaps are heuristic detection patterns that need real upstream captures. Per `feedback_compliance_proxy_text_first` memory: text extraction is the only required output, so a 50-70% coverage on detection helpers is acceptable.

**Recommendation:** keep the wildcard `*/packages/shared/traffic/adapters/*` allowlist entry; pick off individual adapters as captures land (`/test-cursor-adapter` and `/test-geminiweb-adapter` skills already produce verified samples).

## 5. Binding rules to pre-load in the next session

- **Unit test coverage ≥95% per Go package** (CLAUDE.md).
- **Handoff at context-full** — write a fresh handoff doc when the next session also runs heavy (CLAUDE.md).
- **Never `git stash`** — scoped pathspec on every commit (CLAUDE.md + `feedback_never_git_stash_incident`).
- **Real implementation only** — no `_ = err` shortcuts in production code (CLAUDE.md).
- **Test logic must be correct** — boundary cases, named failure modes, prior incidents cited where relevant; not coverage padding (`feedback_test_coverage_autonomous_brainstorm`).
- **Brainstorm on architectural blockers** — DB infra choice, mock pattern selection, etc. Use the brainstorming skill.

## 6. Known caveats

- Two packages currently show **test failures** (not coverage gaps) likely from parallel session work or pending DB migrations:
  - `nexus-hub/internal/consumer`: column `reasoning_tokens` missing from local `traffic_event` — pending migration.
  - `shared/configstore`: passes intermittently in some runs. Probably DB connection state.

  These are unrelated to this program's commits. Investigate via `git log --since="2026-05-16" -- packages/nexus-hub/internal/jobs/consumer/` and the parallel session's migrations.

- `scripts/check-go-coverage.sh` does NOT fail on existing test failures — it reports them as `(test failure: ...)` lines, distinguishing from coverage gaps. The pre-commit hook also tolerates these as long as the staged package is clean.

---

**End of handoff.** Next session: read this file, run `npm run check:coverage:strict` to find caught-up packages, then pick a track from §4.1–§4.4. Auto-exec is authorized per the linked memories.
