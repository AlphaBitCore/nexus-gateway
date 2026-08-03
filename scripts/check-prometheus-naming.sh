#!/usr/bin/env bash
# check-prometheus-naming.sh — enforce prometheus-naming-architecture.md §1.
#
#   Every series is nexus_<subsystem>_<name>. THE SERVICE IS NOT IN THE NAME:
#   which binary emitted a metric is carried by the Prometheus `job` label, so
#   the same subsystem metric emitted by two services is ONE series name.
#
# Why this exists: the rule was documented and had two live violations, because
# nothing enforced it. A metric name is a shipped contract — every dashboard and
# alert that reads it breaks on a rename — so the cheap moment to catch this is
# before it merges, not after someone builds on it.
#
# What it checks, per prometheus-naming-architecture.md:
#   1. A metric name must not contain a SERVICE name. The service list is
#      derived from packages/shared/schemas/thingtype — the same source the
#      fleet uses — so a new service is covered without touching this script.
#   2. Namespace: must be exactly "nexus". A namespace is prefixed to the name,
#      so Namespace: "nexus_hub" produces nexus_hub_* — rule 1 by another route.
#
# Usage:
#   scripts/check-prometheus-naming.sh            # whole repo
#   scripts/check-prometheus-naming.sh --staged   # staged Go files only
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STAGED=false
[[ "${1:-}" == "--staged" ]] && STAGED=true

# Service names come from the canonical registry, not a list maintained here.
# Both the hyphenated wire form (ai-gateway) and the metric-legal underscored
# form (ai_gateway) are rejected — a name cannot contain a hyphen, so the
# underscored spelling is the one that actually shows up in violations.
SERVICES_FILE="packages/shared/schemas/thingtype/thingtype.go"
if [[ ! -f "$SERVICES_FILE" ]]; then
  echo "[check-prometheus-naming] cannot find $SERVICES_FILE — the service registry moved; fix this script rather than deleting the check." >&2
  exit 2
fi
# while-read rather than mapfile: macOS ships bash 3.2, which has no
# mapfile, and this hook must run on developer machines, not just CI.
SERVICES=()
while IFS= read -r svc; do
  [[ -n "$svc" ]] && SERVICES+=("$svc")
done < <(grep -oE '= "[a-z-]+"' "$SERVICES_FILE" | grep -oE '"[a-z-]+"' | tr -d '"' | sort -u)
if [[ ${#SERVICES[@]} -eq 0 ]]; then
  echo "[check-prometheus-naming] parsed zero services out of $SERVICES_FILE — refusing to pass vacuously." >&2
  exit 2
fi
# A parse that silently yields padded or empty entries would make every
# comparison miss and the check pass vacuously — the exact failure mode this
# script exists to prevent, so it is asserted rather than assumed.
for svc in "${SERVICES[@]}"; do
  if [[ -z "$svc" || "$svc" != "${svc// /}" ]]; then
    echo "[check-prometheus-naming] parsed a malformed service name: [$svc] — the registry format changed; fix the parse rather than trusting a green run." >&2
    exit 2
  fi
done

FILES=()
if $STAGED; then
  while IFS= read -r f; do
    [[ -n "$f" ]] && FILES+=("$f")
  done < <(git diff --cached --name-only --diff-filter=ACM | grep -E '\.go$' | grep -v '_test\.go$' || true)
else
  while IFS= read -r f; do
    [[ -n "$f" ]] && FILES+=("$f")
  done < <(git ls-files '*.go' | grep -v '_test\.go$' || true)
fi
if [[ ${#FILES[@]} -eq 0 ]]; then
  echo "[check-prometheus-naming] no Go files to check — skipping."
  exit 0
fi

VIOLATIONS=0
report() {
  printf '  ✗ %s\n      %s\n' "$1" "$2"
  VIOLATIONS=$((VIOLATIONS + 1))
}

# In --staged mode scan the INDEX blob — the content that would actually be
# committed — never the working tree, which may carry unstaged edits on a
# partially staged file.
file_content() {
  if $STAGED; then
    git show ":$1" 2>/dev/null
  else
    cat "$1" 2>/dev/null
  fi
}

for f in "${FILES[@]}"; do
  content="$(file_content "$f")" || continue
  [[ -z "$content" ]] && continue

  # Rule 1: a service name inside a metric name.
  while IFS= read -r hit; do
    line="${hit%%:*}"
    text="${hit#*:}"
    for svc in "${SERVICES[@]}"; do
      underscored="${svc//-/_}"
      if [[ "$text" == *"nexus_${underscored}_"* ]]; then
        report "$f:$line" "metric name carries the service \"$svc\": ${text#"${text%%\"*}"}"
        break
      fi
    done
  done < <(printf '%s\n' "$content" | grep -nE 'Name: *"nexus_' || true)

  # Rule 2: Namespace must be exactly "nexus". Anything else is prefixed onto
  # the name and reintroduces rule 1 through the back door.
  while IFS= read -r hit; do
    line="${hit%%:*}"
    text="${hit#*:}"
    ns="$(printf '%s' "$text" | grep -oE 'Namespace: *"[^"]*"' | grep -oE '"[^"]*"' | tr -d '"')"
    [[ -z "$ns" || "$ns" == "nexus" ]] && continue
    report "$f:$line" "Namespace must be \"nexus\", got \"$ns\" — it is prefixed onto the name, producing ${ns}_*"
  done < <(printf '%s\n' "$content" | grep -nE 'Namespace: *"' || true)
done

if [[ $VIOLATIONS -gt 0 ]]; then
  cat >&2 <<EOF

[check-prometheus-naming] $VIOLATIONS violation(s) of prometheus-naming-architecture.md §1.

  The rule: nexus_<subsystem>_<name>. The service is NOT in the name — it is
  carried by the Prometheus \`job\` label, set by the scrape config. The same
  subsystem metric emitted by two services must be ONE series name.

  Fix: drop the service segment (nexus_ai_gateway_admission_shed_total ->
  nexus_admission_shed_total), or set Namespace: "nexus" and put the subsystem
  in Subsystem.

  Renaming a metric breaks every dashboard and alert reading it, so do it before
  it ships, not after.
EOF
  exit 1
fi

echo "[check-prometheus-naming] ${#FILES[@]} file(s) checked, ${#SERVICES[@]} service name(s) enforced — clean."
