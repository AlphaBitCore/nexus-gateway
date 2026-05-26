#!/usr/bin/env bash
# apply-migration.sh — apply one or more pending Prisma migrations to prod
# without running `prisma migrate deploy`.
#
# Why this exists:
#   - The team historically applies prod migrations by hand (psql + INSERT
#     into _prisma_migrations) rather than `prisma migrate deploy`. The
#     2026-05-23 deploy revealed two recurring bugs in the ad-hoc shell
#     loops: (1) ssh's exit code did not propagate psql failures, so
#     ERROR'd migrations were silently marked "OK"; (2) escaping JSON or
#     multi-statement SQL through nested heredocs broke at random.
#   - This script fixes both: it sends the migration SQL as a file (no
#     escaping), wraps it in BEGIN/COMMIT + INSERT _prisma_migrations,
#     and runs psql with -v ON_ERROR_STOP=1 + -f so a failure bubbles
#     all the way up to the shell exit code.
#   - Checksum: the INSERT records the real sha256(migration.sql) instead
#     of the legacy 'manual' sentinel, so the long-term migration to
#     `prisma migrate deploy` doesn't have to backfill afterwards.
#
# Usage (from repo root):
#   .claude/skills/prod-deploy/apply-migration.sh <migration_name> [...]
# or:
#   HOST=... .claude/skills/prod-deploy/apply-migration.sh <migration_name>
#
# Where <migration_name> is the directory name under
# tools/db-migrate/migrations/ (e.g. 20260601000000_e62_model_capability).
#
# Exits non-zero on the first migration that fails so the caller can
# stop the deploy.

set -euo pipefail

HOST="${HOST:-ec2-user@18.204.174.212}"
PGPASSWORD="${PGPASSWORD:-VclwRVYAAadpVPJfY9hzd0cM}"
PGUSER="${PGUSER:-nexus}"
PGDB="${PGDB:-nexus_gateway}"
MIG_DIR="${MIG_DIR:-tools/db-migrate/migrations}"

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <migration_name> [migration_name ...]" >&2
  exit 2
fi

compute_checksum() {
  # Prisma uses plain sha256 of the migration.sql file contents (no
  # newline strip, no normalization). Verified empirically 2026-05-23
  # against two pre-existing prod rows whose checksums were not 'manual'.
  shasum -a 256 "$1" | awk '{print $1}'
}

apply_one() {
  local name=$1
  local sql_file="${MIG_DIR}/${name}/migration.sql"

  if [ ! -f "$sql_file" ]; then
    echo "MISSING: $sql_file" >&2
    return 3
  fi

  echo "=== applying ${name} ==="

  local checksum
  checksum=$(compute_checksum "$sql_file")

  # Build a wrapper SQL file locally so:
  #   1. The migration body is included via `\i` (no escaping pain).
  #   2. The INSERT uses single-quoted SQL string literals safely.
  #   3. The whole thing runs in one transaction; psql with
  #      ON_ERROR_STOP=1 propagates failure to its exit code, which
  #      ssh propagates to ours.
  local wrap
  wrap=$(mktemp)
  cat >"$wrap" <<SQLEOF
BEGIN;
\\i /tmp/mig-${name}.sql
INSERT INTO _prisma_migrations
  (id, checksum, finished_at, migration_name, logs, rolled_back_at, started_at, applied_steps_count)
VALUES
  (gen_random_uuid()::text, '${checksum}', now(), '${name}', NULL, NULL, now(), 1);
COMMIT;
SQLEOF

  # Ship both files to the remote /tmp, run, clean up.
  scp -q -o StrictHostKeyChecking=no "$sql_file" "$HOST:/tmp/mig-${name}.sql"
  scp -q -o StrictHostKeyChecking=no "$wrap" "$HOST:/tmp/wrap-${name}.sql"
  rm -f "$wrap"

  # `-v ON_ERROR_STOP=1` + `-f` makes psql return non-zero on any error.
  # `set -e` + `set -o pipefail` (top of script) means we exit here.
  ssh -o StrictHostKeyChecking=no "$HOST" \
    "PGPASSWORD='${PGPASSWORD}' psql -h localhost -U ${PGUSER} -d ${PGDB} \
       -v ON_ERROR_STOP=1 -f /tmp/wrap-${name}.sql && \
     rm -f /tmp/mig-${name}.sql /tmp/wrap-${name}.sql"

  echo "OK ${name} (checksum=${checksum:0:12}...)"
}

for m in "$@"; do
  apply_one "$m"
done

echo
echo "===== all ${#} migration(s) applied successfully ====="
