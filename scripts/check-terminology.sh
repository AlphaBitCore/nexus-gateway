#!/usr/bin/env bash
# Fails if internal Thing-Model SOURCE identifiers leak into Control Plane UI
# page components.
#
# What is ACTUALLY scanned. Keep this banner in step with the globs below: a
# banner that overstates the scope is worse than none, because it is read as
# proof that a surface is covered.
#   packages/control-plane-ui/src/{pages,components,lib}/**  ·  *.ts *.tsx *.js *.jsx *.json
#   packages/agent/ui/frontend/src/{pages,components}/**   ·  same extensions
#
# What is deliberately NOT scanned here:
#   - Go handlers / packages/control-plane/internal/handler/  — the Hub↔product
#     translation seam; internal terms there are by design (handlers map them
#     onto product-facing response shapes).
#   - Product docs — reviewed via the doc-review skill, not this guard.
#
# Locale bundles — string VALUES are scanned via a JSON value-walk (Python3)
# against FORBIDDEN_COPY, which is BROADER than the source pattern (see the
# note there: a locale value is copy, never an identifier or a comment),
# for BOTH UI bundles: packages/control-plane-ui/src/i18n/locales and
# packages/agent/ui/frontend/src/i18n/locales. The agent bundle was outside the
# walk until 2026-08-27, and it was carrying "Drifted from admin" / "desired" /
# "Last reported" on the agent's /policies page the whole time. Interpolation tokens {{…}} are stripped before
# matching, so {{desiredVer}} template tokens are not flagged. JSON keys are
# not checked (they are the API contract and may legitimately mirror internal
# names).
#
# Internal terms (thing / shadow / drift / desired / reported) must never appear
# in product-facing surfaces per
# docs/developers/architecture/cross-cutting/foundation/thing-model.md Section 10.
#
# Self-test (guard-rot canary — run in CI / pre-commit):
#   scripts/check-terminology.sh --selftest
# verifies the detector still flags known-bad identifiers and passes known-good
# ones, so the guard can't silently rot into a no-op.
set -euo pipefail

# `drift` requires a Thing-Model-specific suffix (drifted / driftKeys /
# driftMonitor / driftedAt) so the regex doesn't false-positive on the
# generic English word (e.g. percentage-rounding adjustment vars).
FORBIDDEN='(\bthing(s|Type|Id)?\b|\bshadow\b|\bdrift(ed|edAt|Keys|Monitor)\b|\bdesired(Ver)?\b|\breported(Ver)?\b)'

# Locale VALUES are held to a broader vocabulary than source, and the reason is
# that they are a different kind of text. Source legitimately says "drift" 35
# times meaning "diverge" — in comments, in test names, in a rounding
# adjustment variable — which is why the pattern above demands a Thing-Model
# suffix. A locale value has no comments and no identifiers: every string in a
# bundle is copy a user reads, so any form of the internal vocabulary is a
# defect there, suffix or not.
#
# Sharing one pattern between the two is what let `"filterStatusDrift":
# "Drifting"` render on the prod Nodes page while this guard reported clean —
# the walk read that exact file and that exact value, and `drift` without one
# of the four suffixes did not match. Measured before adopting: across all 12
# bundles the broad pattern flags that one string and nothing else, so it costs
# no false positives.
FORBIDDEN_COPY='(\bthings?\b|\bshadow\w*\b|\bdrift\w*\b|\bdesired\b|\breported\b)'

# Scope: every UI tree that can render user-visible strings. Pages are
# the bulk, but components and lib also ship JSX text / formatted labels admins
# read. NOT scanned: src/api (the TypeScript mirror of the Hub/CP handler shapes;
# its `thingId` / `thingType` fields are the contract, internal by design — same
# rationale as the Go handlers below) and src/test (test fixtures).
PATHS=(
  'packages/control-plane-ui/src/pages'
  'packages/control-plane-ui/src/components'
  'packages/control-plane-ui/src/lib'
  'packages/agent/ui/frontend/src/pages'
  'packages/agent/ui/frontend/src/components'
)

# Only scan source file types in the UI pages tree.
INCLUDES=(
  '--include=*.ts'
  '--include=*.tsx'
  '--include=*.js'
  '--include=*.jsx'
  '--include=*.json'
)

# Allowlist — SPECIFIC files, never whole directories, that legitimately
# carry internal Thing identifiers as CODE props mirroring the API contract
# (`thingId` / `thingType` passed into a component, the ThingStats* surface, the
# `'thing'` SourceType enum value). Narrowed from the old blanket
# `pages/infrastructure/` so a NEW infra page that leaks an internal term into
# user-rendered TEXT is still caught.
#
# infrastructure node tabs all take thingId/thingType props from the parent
# node-detail page; the _shared tab tree + the node/override/proxy/diag pages are
# the contract-identifier surface:
#   pages/infrastructure/_shared/        — node detail tabs (config/metrics/runtime/logs).
#   pages/infrastructure/nodes/          — node list + detail (thingId/thingType plumbing).
#   pages/infrastructure/overrides/      — override editor takes thingId/thingType.
#   pages/infrastructure/proxy-rollout/  — per-node setup takes thingId.
#   pages/infrastructure/diag-mode/      — diag-events filter takes thingType.
#   pages/infrastructure/agent-setup/    — agent install wizard references thing identity.
# other product surfaces with contract identifiers:
#   pages/status/ServiceDetailPage       — service-status surface.
#   pages/traffic/                       — traffic event filter takes `thingType`.
#   pages/alerts/                        — `'thing'` SourceType enum value.
#   pages/devices/                       — node/device identity surfaces.
#   pages/settings/SettingsAgentTab      — agent settings surface.
#   lib/thingStatus.ts                   — maps the API `thing.status` column to UI labels.
ALLOW_RE='(pages/infrastructure/_shared/|pages/infrastructure/nodes/|pages/infrastructure/overrides/|pages/infrastructure/proxy-rollout/|pages/infrastructure/diag-mode/|pages/infrastructure/agent-setup/|pages/status/ServiceDetailPage|pages/traffic/|pages/alerts/|pages/devices/|pages/settings/SettingsAgentTab\.tsx|lib/thingStatus\.ts)'

# is_violation CODE → exit 0 if CODE is a real terminology violation.
# Skips source comments and the "shadow AI/IT/…" industry-term family, then
# requires a forbidden term. Shared by the main scan and --selftest so the two
# can never diverge.
is_violation() {
  local code="$1"
  local trimmed
  trimmed="$(printf '%s' "$code" | sed -E 's/^[[:space:]]+//')"
  case "$trimmed" in
    '//'*|'/*'*|'*'*|'*/'|'#'*) return 1 ;;
  esac
  # "shadow AI", "shadow IT", "shadow database/copy/file/register" are documented
  # domain jargon unrelated to the Thing-shadow.
  if [[ "$code" =~ [Ss]hadow[[:space:]]+(AI|IT|database|copy|file|register) ]]; then
    return 1
  fi
  # CSS / Tailwind shadow utilities are styling tokens, not the Thing-shadow:
  # `shadow-lg`, `shadow-xl`, `drop-shadow-*`, `text-shadow`, `var(--shadow-*)`.
  # (boxShadow already escapes via its capital S vs the lowercase regex, but the
  # hyphenated Tailwind forms do not — exclude them so widening the scan to
  # src/components doesn't drown in false positives.)
  if [[ "$code" =~ shadow-(sm|md|lg|xl|2xl|none|inner|\[) || "$code" =~ -shadow ]]; then
    # Re-check: only bail when shadow-* is the ONLY forbidden hit. If a real
    # internal term (thing/drift/desired/reported) is also on the line, fall
    # through so it is still caught.
    local without_shadow
    without_shadow="$(printf '%s' "$code" | sed -E 's/(text-|drop-|box-)?shadow-(sm|md|lg|xl|2xl|none|inner|\[[^]]*\])//g; s/--shadow[a-z-]*//g')"
    if ! printf '%s' "$without_shadow" | grep -Eq "$FORBIDDEN"; then
      return 1
    fi
  fi
  printf '%s' "$code" | grep -Eq "$FORBIDDEN"
}

run_selftest() {
  local fail=0 s
  # Known-bad: internal source identifiers that MUST be flagged.
  local bad=(
    'const thingId = resolveNode()'
    'let thingType: string'
    'shadow.report(payload)'
    'driftedAt: now'
    'const desiredVer = 1'
    'props.reportedVer'
    'state.things'
  )
  # Known-good: must NOT be flagged.
  local good=(
    'const nodeId = resolveNode()'
    '// thingId mirrors the API contract'
    'detect shadow AI usage in the org'
    'const appliedConfig = load()'
    'const outOfSync = true'
  )
  for s in "${bad[@]}"; do
    if ! is_violation "$s"; then
      echo "selftest FAIL: expected violation, none flagged: $s" >&2
      fail=1
    fi
  done
  for s in "${good[@]}"; do
    if is_violation "$s"; then
      echo "selftest FAIL: false positive: $s" >&2
      fail=1
    fi
  done
  if [[ $fail -ne 0 ]]; then
    echo "selftest: FAILED" >&2
    exit 1
  fi
  echo "selftest: ${#bad[@]} bad + ${#good[@]} good case(s) passed."
  exit 0
}

if [[ "${1:-}" == "--selftest" ]]; then
  run_selftest
fi

hits=0
for p in "${PATHS[@]}"; do
  if [[ ! -d "$p" ]]; then
    echo "Path not found: $p (terminology guard misconfigured)" >&2
    exit 2
  fi
  rc=0
  out=$(grep -rEn "${INCLUDES[@]}" "$FORBIDDEN" "$p" 2>&1) || rc=$?
  # grep exit codes: 0 = matches, 1 = no matches (expected clean case),
  # >=2 = real error (unreadable file, bad regex, missing path). Surface real
  # errors so the guard can't silently pass when misconfigured.
  if [[ $rc -gt 1 ]]; then
    echo "grep failed for $p (rc=$rc): $out" >&2
    exit 2
  fi
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    if [[ "$line" =~ $ALLOW_RE ]]; then
      continue
    fi
    # `code` is the matched text AFTER grep's `<file>:<lineno>:` prefix.
    code="${line#*:*:}"
    if is_violation "$code"; then
      echo "TERMINOLOGY: $line"
      hits=$((hits + 1))
    fi
  done <<< "$out"
done

# Locale bundle value-walk: parse each JSON file under
# src/i18n/locales/, strip {{…}} interpolation tokens from each string value,
# then test the remainder against the same FORBIDDEN pattern. Keys are never
# tested — they are the API contract. Python3 is required; its absence is a
# guard-misconfiguration error (exit 2).
LOCALE_DIRS=(
  'packages/control-plane-ui/src/i18n/locales'
  'packages/agent/ui/frontend/src/i18n/locales'
)
for d in "${LOCALE_DIRS[@]}"; do
  if [[ ! -d "$d" ]]; then
    echo "Locale dir not found: $d (terminology guard misconfigured)" >&2
    exit 2
  fi
done
if ! command -v python3 &>/dev/null; then
  echo "python3 not found — required for locale value scan (terminology guard misconfigured)" >&2
  exit 2
fi
# Run the scanner to completion FIRST and check its exit code — a process
# substitution would discard a Python failure (unparsable JSON, crash) and
# let the guard pass vacuously on the very runs where it saw nothing.
locale_rc=0
JOB_DEFS_DIR="packages/nexus-hub/internal/jobs/defs"
if [[ ! -d $JOB_DEFS_DIR ]]; then
  echo "Job defs dir not found: $JOB_DEFS_DIR (terminology guard misconfigured)" >&2
  exit 2
fi

locale_out="$(python3 - "$FORBIDDEN_COPY" "${LOCALE_DIRS[@]}" <<'PYEOF'
import json, os, re, sys

pattern     = sys.argv[1]
locale_dirs = sys.argv[2:]
FORBIDDEN  = re.compile(pattern, re.IGNORECASE)
INTERP     = re.compile(r'\{\{[^}]*\}\}')

def walk(obj, path, locale, fname):
    if isinstance(obj, dict):
        for k, v in obj.items():
            walk(v, f'{path}.{k}' if path else k, locale, fname)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            walk(v, f'{path}[{i}]', locale, fname)
    elif isinstance(obj, str):
        cleaned = INTERP.sub('', obj)
        m = FORBIDDEN.search(cleaned)
        if m:
            rel = f'{locale}/{fname}'
            print(f'{rel}: key={path!r} value={obj!r} matched={m.group()!r}')

scanned = 0
for locale_dir in locale_dirs:
    for locale in sorted(os.listdir(locale_dir)):
        lpath = os.path.join(locale_dir, locale)
        if not os.path.isdir(lpath):
            continue
        for fname in sorted(os.listdir(lpath)):
            if not fname.endswith('.json'):
                continue
            fpath = os.path.join(lpath, fname)
            with open(fpath) as f:
                try:
                    data = json.load(f)
                except json.JSONDecodeError as e:
                    print(f'ERROR: cannot parse {fpath}: {e}', file=sys.stderr)
                    sys.exit(2)
            scanned += 1
            walk(data, '', f'{locale_dir}/{locale}', fname)

# A walk that reached no bundle would report a clean sweep having read nothing.
if scanned < 10:
    print(f'ERROR: locale walk opened only {scanned} bundle(s) across '
          f'{len(locale_dirs)} dir(s) — the scan is broken, not the bundles.',
          file=sys.stderr)
    sys.exit(2)
PYEOF
)" || locale_rc=$?
if [[ $locale_rc -ne 0 ]]; then
  echo "Locale value scan failed (python3 rc=$locale_rc) — the guard cannot vouch for locale bundles (terminology guard misconfigured)" >&2
  exit 2
fi
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  echo "TERMINOLOGY (locale value): $line"
  hits=$((hits + 1))
done <<< "$locale_out"

# ---------------------------------------------------------------------------
# Job labels — Go strings that become rows an operator reads.
#
# The Go trees are excluded from the scan above on purpose: they are the
# Hub-to-product translation seam and internal terms belong there. But the
# `JobName` / `JobDescription` consts under packages/nexus-hub/internal/jobs/
# defs are not internal — the scheduler upserts them into the `job` table and
# /infrastructure/jobs renders both columns verbatim. They are product copy
# that happens to live in Go, and nothing was checking them: thirteen of the
# fifty-three rows on prod read "Stale Thing Sweep", "Per-Thing Rollup Merge",
# "Config Drift Detection", "Detects Things whose reported config version
# differs from desired".
#
# A description may legitimately name a table or column an operator needs —
# thing_agent.cert_expires_at, thing.offline, thing_metric_rollup_5m. So the
# pattern here is PROSE-only: the vocabulary must not be glued to a dot or an
# underscore. "Detects Things whose reported config" is the defect;
# "Aggregates traffic_event rows" is documentation.
job_out="$(python3 - "$JOB_DEFS_DIR" <<'PYEOF'
import os, re, sys

base = sys.argv[1]
# Vocabulary standing alone as a word — not part of an identifier.
PROSE = re.compile(r'(?<![._\w])(things?|shadow|drift|drifting|drifted|desired|reported)(?![._\w])', re.I)
# JobName / JobDescription consts, and the `name:` fields of the table-driven
# merge jobs, which never become consts.
LABEL = re.compile(r'(?:JobName|JobDescription)\s*=\s*"((?:[^"\\]|\\.)*)"'
                   r'|(?:^\s*name:\s*)"((?:[^"\\]|\\.)*)"', re.M)

# Allowlisted, with the reason. "Vendor Bill Drift Alerts" is billing-
# reconciliation drift — an unrelated concept that shares the word with the
# Thing-Model term. Renaming it would make the label wrong.
ALLOW = {'Vendor Bill Drift Alerts'}

scanned = 0
for root, _, files in os.walk(base):
    for fn in files:
        if not fn.endswith('.go') or fn.endswith('_test.go'):
            continue
        scanned += 1
        with open(os.path.join(root, fn), encoding='utf-8') as f:
            src = f.read()
        for m in LABEL.finditer(src):
            val = m.group(1) or m.group(2) or ''
            if val in ALLOW:
                continue
            hit = PROSE.search(val)
            if hit:
                rel = os.path.join(root, fn)
                print(f'{rel}: matched={hit.group()!r} label={val[:110]!r}')

# A walk that opened nothing would report clean having read nothing.
if scanned < 20:
    print(f'ERROR: job-label walk opened only {scanned} .go file(s) under '
          f'{base} — the scan is broken, not the labels.', file=sys.stderr)
    sys.exit(2)
PYEOF
)" || job_rc=$?
if [[ ${job_rc:-0} -ne 0 ]]; then
  echo "Job-label scan failed (python3 rc=${job_rc}) — the guard cannot vouch for job labels (terminology guard misconfigured)" >&2
  exit 2
fi
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  echo "TERMINOLOGY (job label): $line"
  hits=$((hits + 1))
done <<< "$job_out"

if [[ $hits -gt 0 ]]; then
  echo "Terminology check failed: $hits occurrences of internal terms in product-facing code."
  exit 1
fi
echo "Terminology check passed."
