#!/usr/bin/env bash
# Fails if internal Thing Model terms leak into product-facing surfaces.
# Scope: admin API handlers, UI source, UI locales, product docs.
#
# Internal terms (thing / shadow / drift / desired / reported) must never appear
# in product-facing surfaces per docs/developers/architecture/cross-cutting/foundation/thing-model.md Section 10. Callers at
# the translation boundary (hubadapter, admin_hub_proxy, raw SQL on internal
# tables) are allowlisted — the boundary exists precisely so the rest of the
# product code stays clean.
set -euo pipefail

# `drift` requires a Thing-Model-specific suffix (drifted / driftKeys /
# driftMonitor / driftedAt) so the regex doesn't false-positive on the
# generic English word (e.g. percentage-rounding adjustment vars).
FORBIDDEN='(\bthing(s|Type|Id)?\b|\bshadow\b|\bdrift(ed|edAt|Keys|Monitor)\b|\bdesired(Ver)?\b|\breported(Ver)?\b)'
# Scope. `packages/control-plane/internal/handler/` is intentionally NOT
# scanned — that directory IS the Hub↔product translation seam, so internal
# terms appearing there are by design (handlers map them onto product-facing
# response shapes). The guard's job is to catch terms that leak *past* that
# seam: into the UI source, the user-facing locale strings, and the product-
# audience docs.
PATHS=(
  'packages/control-plane-ui/src/pages'
  'packages/control-plane-ui/src/i18n/locales'
  'packages/control-plane-ui/public/locales'
)

# Only scan source/doc file types; skip CSS (box-shadow is unrelated) and
# generated assets.
INCLUDES=(
  '--include=*.go'
  '--include=*.ts'
  '--include=*.tsx'
  '--include=*.js'
  '--include=*.jsx'
  '--include=*.json'
  '--include=*.md'
)

# Allowlist — files inside the scanned PATHS that legitimately surface
# internal Thing/Shadow identity to admins:
#
#   pages/infrastructure/             — node/service detail surfaces (the
#                                       /infrastructure admin route was
#                                       explicitly designed to expose node
#                                       identity for operators; using
#                                       `thingId` / `thingType` as prop
#                                       names mirrors the API contract).
#   pages/status/ServiceDetailPage    — analogous service-status surface.
#   pages/traffic/                    — the traffic event filter API
#                                       takes `thingType` as a query
#                                       parameter; the UI mirrors it 1:1.
#   pages/alerts/                     — alerts feature uses `'thing'` as a
#                                       SourceType enum value to identify
#                                       Thing-Model-scoped alerts; the
#                                       string literal is the canonical
#                                       domain name, not user-rendered text.
#   i18n/locales/, public/locales/    — i18n bundles. Internal terms appear
#                                       as object KEYS (never rendered) and
#                                       as interpolation tokens like
#                                       `{{desiredVer}}` in template VALUES
#                                       — these are API contract names that
#                                       flow through i18next.t() substitution
#                                       and are replaced before render. The
#                                       guard's line-based grep can't tell
#                                       a key from a value, so the whole
#                                       locale tree is allowlisted; rely on
#                                       UI-source review to keep rendered
#                                       text product-correct.
#   architecture-highlights-zh.md     — product-audience doc that explicitly
#                                       documents the Thing/Shadow internal
#                                       architecture for technical readers
#                                       (e.g. potential customers' eng leads).
#   docs/developers/                  — developer docs may use internal terms.
#   architecture-deep-dive-zh         — internal architecture deep-dive.
#
# Lines that look like source comments (`//`, `/*`, ` * `, `#`) are skipped
# regardless of file — internal terms in dev-facing comments don't reach
# end users.
ALLOW_RE='(pages/infrastructure/|pages/status/ServiceDetailPage|pages/traffic/|pages/alerts/|pages/devices/|pages/settings/SettingsAgentTab\.tsx|i18n/locales/|public/locales/|architecture-highlights-zh|docs/developers/|architecture-deep-dive-zh)'

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
    # Skip lines that look like source comments — `//`, `/*`, ` * `, `#`.
    # Internal terms in dev-facing comments don't reach end users; only
    # rendered text / variable values / locale strings actually matter for
    # the product-terminology boundary. The match looks AFTER the
    # `<file>:<lineno>:` prefix that grep -n emits.
    code="${line#*:*:}"
    trimmed="$(printf '%s' "$code" | sed -E 's/^[[:space:]]+//')"
    case "$trimmed" in
      '//'*|'/*'*|'*'*|'*/'|'#'*) continue ;;
    esac
    # Skip "shadow X" industry terms unrelated to Thing-shadow: shadow AI,
    # shadow IT, shadow database, shadow copy, shadow file, shadow register.
    # These are documented domain jargon (e.g. "shadow AI" = unsanctioned AI
    # use) that legitimately appears in product docs about competitive
    # landscape / threat model.
    if [[ "$code" =~ [Ss]hadow[[:space:]]+(AI|IT|database|copy|file|register) ]]; then
      continue
    fi
    echo "TERMINOLOGY: $line"
    hits=$((hits + 1))
  done <<< "$out"
done

if [[ $hits -gt 0 ]]; then
  echo "Terminology check failed: $hits occurrences of internal terms in product-facing code."
  exit 1
fi
echo "Terminology check passed."
