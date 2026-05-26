#!/usr/bin/env bash
# E32-S1 — fail the build if any code drops back into TZ-incorrect
# patterns: bare time.Now() in persistence paths or `timestamp`
# (no tz) columns in Prisma migrations. See docs/developers/workflow/timezone.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail=0

# Allowlist regex — call-sites where bare time.Now() is correct
# because the value is consumed by monotonic-clock APIs (deadline /
# sleep / Add / Sub) and never persisted.
allow_re='SetReadDeadline|SetWriteDeadline|WithTimeout|WithDeadline|time\.Sleep|\.Add\(|\.Sub\(|time\.Until|\.AddDate\(|time\.Since\('

# Persistence-relevant directories — anything that might write a
# timestamp into the database / MQ / response body / audit record.
search_dirs=(
  "packages/ai-gateway/cmd"
  "packages/ai-gateway/internal/observability/audit"
  "packages/ai-gateway/internal/store"
  "packages/ai-gateway/internal/handler"
  "packages/ai-gateway/internal/pipeline/quota"
  "packages/control-plane/cmd"
  "packages/control-plane/internal/audit"
  "packages/control-plane/internal/store"
  "packages/control-plane/internal/handler"
  "packages/nexus-hub/cmd"
  "packages/nexus-hub/internal/jobs"
  "packages/nexus-hub/internal/jobs/consumer"
  "packages/nexus-hub/internal/storage/store"
  "packages/compliance-proxy/cmd"
  "packages/compliance-proxy/internal/compliance"
  "packages/agent/internal/audit"
  "packages/shared/audit"
  "packages/shared/configstore"
  "packages/shared/store"
)

# (1) bare time.Now() in persistence paths.
echo "[tz-lint] scanning Go persistence paths for bare time.Now()…"
hits=$(grep -rnE 'time\.Now\(\)[^.]' "${search_dirs[@]}" 2>/dev/null \
  | grep -vE "$allow_re" \
  | grep -v '_test\.go:' \
  | grep -v '// timeutil-skip' \
  || true)
if [ -n "$hits" ]; then
  echo "FAIL: bare time.Now() in persistence path. Use timeutil.Now()."
  echo "$hits"
  echo
  fail=1
fi

# Historical migration SQL files are intentionally not scanned: they
# created the wrong column types but the timestamps_to_timestamptz
# migration converted everything in place. schema.prisma is the
# source of truth; new migrations are auto-generated from it and
# will pick up the @db.Timestamptz(3) attribute correctly.

# (2) `DateTime` in schema.prisma without @db.Timestamptz.
echo "[tz-lint] scanning schema.prisma for tz-less DateTime fields…"
schema_hits=$(grep -E '^\s+\w+\s+DateTime' tools/db-migrate/schema.prisma \
  | grep -v '@db\.Timestamptz' \
  || true)
if [ -n "$schema_hits" ]; then
  echo "FAIL: DateTime field in schema.prisma without @db.Timestamptz(3)."
  echo "$schema_hits"
  echo
  fail=1
fi

if [ $fail -eq 0 ]; then
  echo "[tz-lint] OK"
fi
exit $fail
