#!/usr/bin/env bash
# Migration timestamp uniqueness check (binding rule, memory: feedback_migration_timestamp_unique).
#
# Two Prisma migration folders sharing the same YYYYMMDDHHMMSS prefix cause
# Prisma to silently skip one of them. Production audit-gap incident 2026-05-14
# was caused exactly by this. Pre-commit + CI enforce uniqueness here.

set -euo pipefail

MIGRATIONS_DIR="tools/db-migrate/migrations"

if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "[check:migration-timestamps] No migrations directory ($MIGRATIONS_DIR); skipping."
  exit 0
fi

dupes="$(ls "$MIGRATIONS_DIR" | grep -E '^[0-9]{14}_' | cut -c1-14 | sort | uniq -d || true)"

if [ -n "$dupes" ]; then
  echo "[check:migration-timestamps] FAILED -- duplicate migration timestamp prefix(es) found:"
  for ts in $dupes; do
    echo "  - $ts"
    ls "$MIGRATIONS_DIR" | grep "^${ts}_" | sed 's/^/      /'
  done
  echo ""
  echo "Two migrations sharing a YYYYMMDDHHMMSS prefix cause Prisma to silently skip one."
  echo "Rename one of them with a +1 second / minute suffix and rebase any references."
  exit 1
fi

echo "[check:migration-timestamps] OK -- all migration prefixes unique."
