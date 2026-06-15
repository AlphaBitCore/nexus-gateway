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
#   --blocking    Release-gate profile, run MANUALLY (human-controlled, around a
#                 release) — NOT an automatic CI/CD gate. L1 smoke + the core
#                 one-per-family scenario set + a bounded-model AI Gateway smoke
#                 (NEXUS_BLOCKING_MODELS). Real upstream; a red verdict means a
#                 real bug — the operator holds the release. The cross-cutting
#                 invariant layer + stable daemon essentials attach here.
#   --nightly     Broad profile, run MANUALLY / on demand. The full scenario
#                 sweep + the full-surface AI Gateway smoke (--all-ingress) + the
#                 full daemon-bound suite. Informational; does not gate.
#   --phase N     Run only the named phase (smoke|go|ui|ai-judge|protocol)
#   --no-preflight  Skip the preflight check (use only when debugging)
#   --target T    Deployment to run against: local (default) | dev | prod.
#                 Drives which tests/.env.<target> the loader reads and which
#                 deployment every phase hits. NON-LOCAL targets run against a
#                 REAL deployment — scenarios must only create their own data,
#                 never mutate existing rows (tests/scenarios/00-catalog.md §2).

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mode="quick"
skip_preflight=0
phase_filter=""
target="local"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick)    mode="quick";    shift ;;
    --core)     mode="core";     shift ;;
    --full)     mode="full";     shift ;;
    --blocking) mode="blocking"; shift ;;
    --nightly)  mode="nightly";  shift ;;
    --target)   target="$2";     shift 2 ;;
    --phase) phase_filter="$2"; shift 2 ;;
    --no-preflight) skip_preflight=1; shift ;;
    *) printf 'unknown flag: %s\n' "$1" >&2; exit 2 ;;
  esac
done

# Resolve the target BEFORE sourcing env.sh so the loader (loadenv.sh, invoked
# transitively) reads tests/.env.<target> and applies its fail-closed guards
# (target=local → all URLs must be loopback; target=prod → CP URL must NOT be
# loopback). Config for every phase comes from that env file — no hardcoded
# hosts in this script.
case "$target" in
  local|dev|prod) ;;
  *) printf 'run-all.sh: invalid --target %q (allowed: local|dev|prod)\n' "$target" >&2; exit 2 ;;
esac
export NEXUS_TEST_TARGET="$target"

# shellcheck disable=SC1091
source "$_dir/lib/env.sh"
# shellcheck disable=SC1091
source "$_dir/lib/assert.sh"

# Normalize the mode into orthogonal run controls so each phase gates on a
# single intent flag instead of re-deriving from $mode:
#   run_full_layers — run the heavyweight Go-integration / Playwright / Python tiers
#   scenario_set    — which L5 subset Phase 6 runs (quick|core|full)
#   gateway_smoke   — full-surface AI Gateway smoke scope ("" | bounded | all-ingress)
# The two release-gate profiles map onto these controls; --quick/--core/--full
# stay as the day-to-day developer sweeps.
run_full_layers=0
scenario_set="quick"
gateway_smoke=""
case "$mode" in
  quick)    scenario_set="quick" ;;
  core)     scenario_set="core" ;;
  full)     run_full_layers=1; scenario_set="full" ;;
  blocking) scenario_set="core"; gateway_smoke="bounded" ;;
  nightly)  run_full_layers=1; scenario_set="full"; gateway_smoke="all-ingress" ;;
esac

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
# Plus the P2 cross-cutting invariants (broad "silent break" catchers; each
# iterates the ingresses with one bounded model, SKIPping an arm whose provider
# is unseeded locally):
#   S-150 cost stamped       — every ingress stamps non-null estimated_cost_usd
#   S-151 cache mandatory     — repeat request hits across chat-like ingresses
#   S-152 normalize parity    — same prompt canonicalizes identically per ingress
#   S-154 hook always fires   — every request runs the compliance hook chain
# (The fifth invariant — every admin route enforces IAM — is the static
# scripts/check-iam-route-coverage.mjs check, run in the lint layer / check:all.)
# Mid-tier between --quick (S-001 only) and --full (66 scenarios). Targets
# ~3min runtime so it can run on every commit without paying the full
# 160s+ upstream-token cost of the entire suite.
core_scenarios='TestS001|TestS010|TestS021|TestS040|TestS060|TestS062|TestS063|TestS064|TestS066|TestS093|TestS150|TestS151|TestS152|TestS154'

# Prod safe-e2e subset — admin control-plane READ-ONLY scenarios only. They use
# setupScenarioNoVK (admin login, no VK) and issue GET-only admin calls; none
# hit /v1/* (gateway traffic is the ai-gateway smoke's job, run separately).
# When --target prod, the scenario phase runs ONLY these and arms
# NEXUS_PROD_SAFE_E2E=1 so GuardProdSafeE2E refuses any mutating call to a
# shared/global surface at the CPDoJSON choke point — data-safe by construction.
#   S-045 quota analytics  S-072 fleet analytics  S-093 analytics cost
#   S-103 audit export     S-113 IAM simulate     S-144 hub runtime introspection
prod_safe_scenarios='TestS045|TestS072|TestS093|TestS103|TestS113|TestS144'

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
if [[ "$run_full_layers" -eq 1 ]]; then
  if compgen -G "$_dir/integration-go/*_test.go" >/dev/null; then
    _run_phase go "Phase 2: L1 Go integration" \
      "cd $_dir/integration-go && go test ./..." || overall_status=1
  else
    printf '== Phase 2: L1 Go integration (skipped — not yet landed) ==\n'
    printf '## Phase 2: L1 Go integration: SKIPPED (no *_test.go yet)\n\n' >>"$report"
  fi

  # Phase 2b: Agent identity / enrollment integration tests. Five test
  # files cover CSR + cert renewal + SSO flow. Runs against the in-process
  # httptest mocks; no daemon required.
  _agent_enroll_dir="$_dir/../packages/agent/internal/identity/enrollment"
  if compgen -G "$_agent_enroll_dir/*_test.go" >/dev/null; then
    _run_phase agent-enroll "Phase 2b: Agent identity / enrollment integration" \
      "cd $_agent_enroll_dir && go test -count=1 ./..." || overall_status=1
  fi
fi

# Phase 3: Playwright. Default off in --quick. Gated on package.json existing.
if [[ "$run_full_layers" -eq 1 ]]; then
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
if [[ "$run_full_layers" -eq 1 ]]; then
  if compgen -G "$_dir/e2e-python/protocol/test_*.py" >/dev/null; then
    _run_phase protocol "Phase 5: L2 protocol" \
      "cd $_dir/e2e-python && uv run pytest protocol/" || overall_status=1
  fi
  _run_phase ai-judge "Phase 4: L3 AI-judge" \
    "cd $_dir/e2e-python && uv run pytest ai_judge/" || overall_status=1
fi

# Phase 5b: build the daemon container image so the daemon-bound scenarios
# (S-084 containerized agent enrollment, S-155 containerized agent transparent
# interception) RUN instead of SKIPping. Nightly / full only, gated on docker +
# the Dockerfile being present; S-155 additionally needs a host that can grant
# NET_ADMIN/--privileged for the iptables chain (see its SKIP). The proxy CONNECT
# scenario (S-083) additionally needs provider-key/CA secrets (see its SKIP).
if [[ "$run_full_layers" -eq 1 ]] && command -v docker >/dev/null 2>&1 && [[ -f "$_dir/../packages/agent/Dockerfile" ]]; then
  _run_phase daemon-image "Phase 5b: build agent daemon image" \
    "docker build -f $_dir/../packages/agent/Dockerfile -t \${NEXUS_AGENT_IMAGE:-nexus-agent:citest} $_dir/.." \
    || overall_status=1
fi

# Phase 6: scenario-driven business-flow tests. PM-grade e2e core —
# each scenario asserts HTTP shape + DB cross-check + runtime hot-reload
# signal + AdminAuditLog row + Prometheus metric delta where applicable.
# Full sweep is ~160s; runs in --full mode only. --quick gets an
# Onboarding-subset smoke (~15s) so the harness stays exercised on
# every PR without paying the full upstream-token cost.
#
# NEXUS_TEST_TARGET is passed through from --target (default local) so the
# harness's fail-closed env-isolation guard (see tests/scenarios/00-catalog.md
# §2) reads the matching tests/.env.<target>. Setting it explicitly on the
# command keeps run-all.sh runnable from CI / cron without an interactive
# prompt. WARNING: --target prod runs these scenarios against the live
# deployment; only the data-safe (create-own / read-only) subset is safe there
# — scenarios that mutate shared global state (kill-switch, passthrough, IP
# access, streaming-compliance, settings, semantic-cache, routing, config-sync,
# node-overrides) must NOT run against prod.
if compgen -G "$_dir/scenarios/*_test.go" >/dev/null; then
  if [[ "$target" == "prod" ]]; then
    # PROD: ONLY the curated read-only admin subset, with the shared-state
    # mutation guard armed. The scenario harness (helpers.MustBeLocalOrProdSafeE2E)
    # refuses to start against prod unless NEXUS_PROD_SAFE_E2E=1, then blocks any
    # mutating call to a shared/global surface at the CPDoJSON choke point. These
    # scenarios are GET-only admin reads — no /v1/* gateway traffic.
    _run_phase scenarios-prod-safe "Phase 6: scenarios (prod safe-e2e read-only subset)" \
      "cd $_dir/scenarios && NEXUS_TEST_TARGET=prod NEXUS_PROD_SAFE_E2E=1 GOWORK=off go test -count=1 -run '^(${prod_safe_scenarios})' ." \
      || overall_status=1
  else
  case "$scenario_set" in
    full)
      # nightly / --full: the entire scenario sweep.
      _run_phase scenarios "Phase 6: scenario-driven business flows (full)" \
        "cd $_dir/scenarios && NEXUS_TEST_TARGET=$target GOWORK=off go test -count=1 ." \
        || overall_status=1 ;;
    core)
      # blocking / --core: the curated one-per-family set (see core_scenarios above).
      _run_phase scenarios-core "Phase 6: scenarios (core: one per family)" \
        "cd $_dir/scenarios && NEXUS_TEST_TARGET=$target GOWORK=off go test -count=1 -run '^(${core_scenarios})' ." \
        || overall_status=1 ;;
    *)
      # quick: only the harness-validating S-001 hello-world, enough to prove
      # the toolchain wires up end-to-end.
      _run_phase scenarios-quick "Phase 6: scenarios (quick: S-001 only)" \
        "cd $_dir/scenarios && NEXUS_TEST_TARGET=$target GOWORK=off go test -count=1 -run TestS001 ." \
        || overall_status=1 ;;
  esac
  fi
else
  printf '== Phase 6: scenarios (skipped — not yet landed) ==\n'
  printf '## Phase 6: scenarios: SKIPPED (no *_test.go in tests/scenarios/)\n\n' >>"$report"
fi

# Phase 6b: full-surface AI Gateway smoke (tests/scripts/smoke-gateway.py).
# Release-gate profiles only:
#   --blocking → bounded model set (NEXUS_BLOCKING_MODELS, comma-separated)
#   --nightly  → every model across every ingress (--all-ingress)
# Gated on a virtual key being configured so a checkout without provider
# credentials still produces a report instead of failing.
if [[ -n "$gateway_smoke" ]]; then
  if [[ -z "${NEXUS_TEST_VK:-}" ]]; then
    printf '== Phase 6b: gateway smoke (skipped — NEXUS_TEST_VK unset) ==\n'
    printf '## Phase 6b: gateway smoke: SKIPPED (NEXUS_TEST_VK unset)\n\n' >>"$report"
  elif [[ "$gateway_smoke" == "all-ingress" ]]; then
    _run_phase gateway-smoke "Phase 6b: AI Gateway smoke (all ingress)" \
      "python3 $_dir/scripts/smoke-gateway.py --target $target --vk \"$NEXUS_TEST_VK\" --all-ingress" \
      || overall_status=1
  elif [[ -n "${NEXUS_BLOCKING_MODELS:-}" ]]; then
    _run_phase gateway-smoke "Phase 6b: AI Gateway smoke (bounded model set)" \
      "python3 $_dir/scripts/smoke-gateway.py --target $target --vk \"$NEXUS_TEST_VK\" --models \"$NEXUS_BLOCKING_MODELS\"" \
      || overall_status=1
  else
    printf '== Phase 6b: gateway smoke (skipped — NEXUS_BLOCKING_MODELS unset) ==\n'
    printf '## Phase 6b: gateway smoke: SKIPPED (set NEXUS_BLOCKING_MODELS for the --blocking profile)\n\n' >>"$report"
  fi
fi

# Phase 7: coverage-matrix snapshot. Counts ✓/⚠/✗ cells in
# docs/developers/specs/e2e-coverage-matrix.md and emits an informational
# line into the report. The matrix is enforced at PR time by the
# scripts/doc-lockstep.config.mjs `e2e-coverage-matrix` entry; this block
# exists so the run-all.sh report carries the current coverage shape without
# a separate script invocation. Run-all does NOT fail on the `✗` count — the
# closure contract is a PR-level gate, not a per-run gate.
# Compute matrix_doc lazily — `_repo_root` may be unset under `set -u` if
# the script reached here without an earlier helper assigning it.
if [[ -z "${_repo_root:-}" ]]; then
  _repo_root="$(cd "$_dir/.." && pwd)"
fi
matrix_doc="$_repo_root/docs/developers/specs/e2e-coverage-matrix.md"
if [[ -f "$matrix_doc" ]]; then
  # Count rows in the per-category matrix tables (§3.*). The tables use
  # ✓ / ⚠ / ✗ / — cells; we count occurrences inside table rows only.
  ok_cells=$(grep -cE '^\| .* ✓ ' "$matrix_doc" || echo 0)
  partial_cells=$(grep -cE '^\| .* ⚠ ' "$matrix_doc" || echo 0)
  missing_cells=$(grep -cE '^\| .* ✗' "$matrix_doc" || echo 0)
  printf '== Coverage matrix snapshot ==\n'
  printf '  ✓ %s   ⚠ %s   ✗ %s\n' "$ok_cells" "$partial_cells" "$missing_cells"
  {
    printf '## Coverage matrix snapshot\n\n'
    printf -- '- ✓ covered: **%s** cells\n' "$ok_cells"
    printf -- '- ⚠ partial: **%s** cells\n' "$partial_cells"
    printf -- '- ✗ missing: **%s** cells\n' "$missing_cells"
    printf '\nSource: `docs/developers/specs/e2e-coverage-matrix.md` (§2)\n\n'
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
