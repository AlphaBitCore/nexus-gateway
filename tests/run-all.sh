#!/usr/bin/env bash
# tests/run-all.sh — top-level orchestrator.
#
# Runs every test phase and writes a unified markdown report to
# $NEXUS_TEST_LOG_DIR/test-all-<UTC-timestamp>.md.
#
# Flags:
#   --quick       L1 smoke + L1 Go integration only (no Python, no UI)
#   --core        L1 smoke + ~10 high-priority scenarios (~3min sweep);
#                 covers onboarding, routing, PII, quota, cache, responses,
#                 embeddings, semantic, feedback, analytics — the surfaces
#                 most likely to regress in day-to-day development.
#   --full        Everything (default once Phases 4/5 land)
#   --phase N     Run only the named phase (smoke|go|ui|ai-judge|protocol)
#   --no-preflight  Skip the preflight check (use only when debugging)

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$_dir/lib/env.sh"
# shellcheck disable=SC1091
source "$_dir/lib/assert.sh"

mode="quick"
skip_preflight=0
phase_filter=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) mode="quick"; shift ;;
    --core)  mode="core";  shift ;;
    --full)  mode="full";  shift ;;
    --phase) phase_filter="$2"; shift 2 ;;
    --no-preflight) skip_preflight=1; shift ;;
    *) printf 'unknown flag: %s\n' "$1" >&2; exit 2 ;;
  esac
done

# Core scenario set — ~10 high-priority scenarios picked one per major family,
# each exercising a surface where a silent regression would matter:
#   S-001 onboarding         — proves CP login + VK create + first /v1/chat 200
#   S-010 single routing     — proves routing-rule executor + canonical payload
#   S-021 PII scanner        — proves hook pipeline + block-hard verdict
#   S-040 VK rate limit      — proves quota 429 + counter increment
#   S-060 L1 cache hit       — proves response-cache + cache_read_tokens
#   S-062 Responses API      — proves /v1/responses NS + error envelope
#   S-063 Embeddings         — proves /v1/embeddings single + dimensions + batch
#   S-064 semantic cache     — proves L2 semantic-cache hit
#   S-066 cache negative     — proves negative-feedback eviction
#   S-093 analytics cost     — proves cost-summary partition + non-negativity
# Mid-tier between --quick (S-001 only) and --full (66 scenarios). Targets
# ~3min runtime so it can run on every commit without paying the full
# 160s+ upstream-token cost of the entire suite.
core_scenarios='TestS001|TestS010|TestS021|TestS040|TestS060|TestS062|TestS063|TestS064|TestS066|TestS093'

ts=$(date -u +%Y%m%dT%H%M%SZ)
report="$NEXUS_TEST_LOG_DIR/test-all-$ts.md"
mkdir -p "$NEXUS_TEST_LOG_DIR/$ts"

printf '# Nexus Gateway test run %s\n\n' "$ts" >"$report"
printf -- '- Mode: `%s`\n' "$mode" >>"$report"
printf -- '- Repo HEAD: `%s`\n' "$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')" >>"$report"
printf '\n' >>"$report"

# 0. Preflight.
if [[ "$skip_preflight" -ne 1 ]]; then
  printf '== Preflight ==\n'
  if bash "$_dir/lib/preflight.sh" >"$NEXUS_TEST_LOG_DIR/$ts/preflight.log" 2>&1; then
    printf '✓ preflight\n'
    printf '## Preflight: PASS\n\n' >>"$report"
  else
    printf '✗ preflight (see %s/preflight.log)\n' "$NEXUS_TEST_LOG_DIR/$ts"
    printf '## Preflight: FAIL\n\n```\n' >>"$report"
    cat "$NEXUS_TEST_LOG_DIR/$ts/preflight.log" >>"$report"
    printf '\n```\n' >>"$report"
    printf '\nReport: %s\n' "$report"
    exit 1
  fi
fi

# Phase wrappers — each produces a `<phase>.log` and appends a summary line.
_run_phase() {
  local key="$1" label="$2" cmd="$3"
  if [[ -n "$phase_filter" && "$phase_filter" != "$key" ]]; then
    return 0
  fi
  printf '== %s ==\n' "$label"
  local log="$NEXUS_TEST_LOG_DIR/$ts/$key.log"
  if eval "$cmd" >"$log" 2>&1; then
    printf '✓ %s\n' "$label"
    printf '## %s: PASS\n\nLog: `%s`\n\n' "$label" "$log" >>"$report"
    return 0
  else
    printf '✗ %s (see %s)\n' "$label" "$log"
    printf '## %s: FAIL\n\nLog: `%s`\n\nLast 40 lines:\n```\n' "$label" "$log" >>"$report"
    tail -n 40 "$log" >>"$report"
    printf '\n```\n\n' >>"$report"
    return 1
  fi
}

overall_status=0

# Phase 1: L1 smoke — bash scripts that wrap each test-* skill's curl flow.
# Phase 1 lands its own scripts under tests/smoke/. Until then this is a
# placeholder that succeeds.
_run_phase smoke "Phase 1: L1 smoke" \
  "bash $_dir/smoke/run-all.sh" || overall_status=1

# Phase 2: Go integration — `go test ./tests/integration-go/...`. Gated on
# the presence of *_test.go files so an empty scaffold does not fail the run.
if [[ "$mode" == "full" ]]; then
  if compgen -G "$_dir/integration-go/*_test.go" >/dev/null; then
    _run_phase go "Phase 2: L1 Go integration" \
      "cd $_dir/integration-go && go test ./..." || overall_status=1
  else
    printf '== Phase 2: L1 Go integration (skipped — not yet landed) ==\n'
    printf '## Phase 2: L1 Go integration: SKIPPED (no *_test.go yet)\n\n' >>"$report"
  fi

  # Phase 2b: Agent identity / enrollment integration tests. Five test
  # files cover CSR + cert renewal + SSO flow; previously not wired into
  # the orchestrator. Runs against the in-process httptest mocks; no
  # daemon required. E86 §3.5 closure.
  _agent_enroll_dir="$_dir/../packages/agent/internal/identity/enrollment"
  if compgen -G "$_agent_enroll_dir/*_test.go" >/dev/null; then
    _run_phase agent-enroll "Phase 2b: Agent identity / enrollment integration" \
      "cd $_agent_enroll_dir && go test -count=1 ./..." || overall_status=1
  fi
fi

# Phase 3: Playwright. Default off in --quick. Gated on package.json existing.
if [[ "$mode" == "full" ]]; then
  if [[ -f "$_dir/e2e-ui/package.json" ]]; then
    _run_phase ui "Phase 3: L4 Playwright" \
      "cd $_dir/e2e-ui && npx playwright test" || overall_status=1
  else
    printf '== Phase 3: L4 Playwright (skipped — not yet landed) ==\n'
    printf '## Phase 3: L4 Playwright: SKIPPED (e2e-ui not yet scaffolded)\n\n' >>"$report"
  fi
fi

# Phase 4 + 5: Python. Phase 5 protocol/ is empty until that phase lands —
# the run is gated on the directory existing AND containing test_*.py.
if [[ "$mode" == "full" ]]; then
  if compgen -G "$_dir/e2e-python/protocol/test_*.py" >/dev/null; then
    _run_phase protocol "Phase 5: L2 protocol" \
      "cd $_dir/e2e-python && uv run pytest protocol/" || overall_status=1
  fi
  _run_phase ai-judge "Phase 4: L3 AI-judge" \
    "cd $_dir/e2e-python && uv run pytest ai_judge/" || overall_status=1
fi

# Phase 6: scenario-driven business-flow tests. PM-grade e2e core —
# each scenario asserts HTTP shape + DB cross-check + runtime hot-reload
# signal + AdminAuditLog row + Prometheus metric delta where applicable.
# Full sweep is ~160s; runs in --full mode only. --quick gets an
# Onboarding-subset smoke (~15s) so the harness stays exercised on
# every PR without paying the full upstream-token cost.
#
# NEXUS_TEST_TARGET=local is required by the harness's fail-closed
# env-isolation guard (see tests/scenarios/00-catalog.md §2). Setting
# it here keeps run-all.sh runnable from CI / cron without an
# interactive prompt.
if compgen -G "$_dir/scenarios/*_test.go" >/dev/null; then
  if [[ "$mode" == "full" ]]; then
    _run_phase scenarios "Phase 6: scenario-driven business flows (full)" \
      "cd $_dir/scenarios && NEXUS_TEST_TARGET=local GOWORK=off go test -count=1 ." \
      || overall_status=1
  elif [[ "$mode" == "core" ]]; then
    # core mode: ~10 high-priority scenarios — see core_scenarios variable
    # above for the curated set + rationale.
    _run_phase scenarios-core "Phase 6: scenarios (core: 10 priority families)" \
      "cd $_dir/scenarios && NEXUS_TEST_TARGET=local GOWORK=off go test -count=1 -run '^(${core_scenarios})' ." \
      || overall_status=1
  else
    # quick mode: only the harness-validating S-001 hello-world, which
    # is enough to prove the toolchain wires up end-to-end.
    _run_phase scenarios-quick "Phase 6: scenarios (quick: S-001 only)" \
      "cd $_dir/scenarios && NEXUS_TEST_TARGET=local GOWORK=off go test -count=1 -run TestS001 ." \
      || overall_status=1
  fi
else
  printf '== Phase 6: scenarios (skipped — not yet landed) ==\n'
  printf '## Phase 6: scenarios: SKIPPED (no *_test.go in tests/scenarios/)\n\n' >>"$report"
fi

# Phase 7: E86 coverage-matrix snapshot. Counts ✓/⚠/✗ cells in
# docs/developers/specs/e86-e2e-coverage-matrix.md and emits an
# informational line into the report. The matrix gate is enforced
# at PR time by scripts/doc-lockstep.config.mjs entry
# `e2e-coverage-matrix`; this block exists so the run-all.sh report
# carries the current coverage shape without requiring a separate
# script invocation. Run-all does NOT fail on `✗` count — deferred
# rows are documented in §4 of the matrix; the closure contract is
# a PR-level gate, not a per-run gate (see e86-decision-log.md D6).
# Compute matrix_doc lazily — `_repo_root` may be unset under `set -u` if
# the script reached here without an earlier helper assigning it.
if [[ -z "${_repo_root:-}" ]]; then
  _repo_root="$(cd "$_dir/.." && pwd)"
fi
matrix_doc="$_repo_root/docs/developers/specs/e86-e2e-coverage-matrix.md"
if [[ -f "$matrix_doc" ]]; then
  # Count rows in the per-category matrix tables (§3.*). The tables use
  # ✓ / ⚠ / ✗ / — cells; we count occurrences inside table rows only.
  ok_cells=$(grep -cE '^\| .* ✓ ' "$matrix_doc" || echo 0)
  partial_cells=$(grep -cE '^\| .* ⚠ ' "$matrix_doc" || echo 0)
  missing_cells=$(grep -cE '^\| .* ✗' "$matrix_doc" || echo 0)
  printf '== E86 matrix snapshot ==\n'
  printf '  ✓ %s   ⚠ %s   ✗ %s\n' "$ok_cells" "$partial_cells" "$missing_cells"
  {
    printf '## E86 E2E coverage matrix snapshot\n\n'
    printf -- '- ✓ covered: **%s** cells\n' "$ok_cells"
    printf -- '- ⚠ partial: **%s** cells\n' "$partial_cells"
    printf -- '- ✗ missing: **%s** cells\n' "$missing_cells"
    printf '\nSource: `docs/developers/specs/e86-e2e-coverage-matrix.md` (§3.*)\n\n'
  } >>"$report"
fi

# macOS pf gap-closure tests — opt-in via NEXUS_RUN_MACOS_PF_TESTS=true.
# The guard keeps default CI runs (Linux) and quick developer runs unaffected.
# macOS developers opt-in by setting NEXUS_RUN_MACOS_PF_TESTS=true in
# tests/.env.local or exporting it before running this script.
# Requires: agent daemon running with interceptMode=pf, root/sudo for pfctl,
#           compliance-proxy running on localhost:3128 (for consistency arm).
if [[ "$(uname)" == "Darwin" ]] && [[ "${NEXUS_RUN_MACOS_PF_TESTS:-false}" == "true" ]]; then
  echo "==> Running macOS pf gap-closure tests"
  _run_phase macos-pf "macOS pf gap-closure (E74-S7)" \
    "bash $(git rev-parse --show-toplevel)/.claude/skills/test-macos-pf-agent/runner.sh" \
    || overall_status=1
fi

printf '\nReport: %s\n' "$report"
exit "$overall_status"
