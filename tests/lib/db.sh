#!/usr/bin/env bash
# tests/lib/db.sh — thin wrapper around `docker exec psql` for assertions.
#
# Usage:
#   source tests/lib/loadenv.sh         # populates NEXUS_PG_* etc.
#   source tests/lib/db.sh
#   db_query "SELECT count(*) FROM \"Provider\""
#
# LOCAL TARGET ONLY: this wrapper assumes Postgres runs inside a Docker
# container reachable via `docker exec`. For prod/dev targets that expose
# Postgres over ssh+psql, callers should use the inline ssh pattern
# (NEXUS_SSH_HOST + ssh psql ...) — db.sh refuses to run when
# NEXUS_TEST_TARGET != local to keep prod data safe.
#
# All wrappers honour NEXUS_PG_CONTAINER / NEXUS_PG_DB / NEXUS_PG_USER
# from tests/.env.<target>.

set -u

: "${NEXUS_TEST_TARGET:?source tests/lib/loadenv.sh first to set NEXUS_TEST_TARGET}"
if [[ "$NEXUS_TEST_TARGET" != "local" ]]; then
  echo "db.sh: target=$NEXUS_TEST_TARGET — db_query/db_scalar are local-only." >&2
  echo "  Use ssh psql against the remote host for non-local targets." >&2
  return 1 2>/dev/null || exit 1
fi
: "${NEXUS_PG_CONTAINER:?set NEXUS_PG_CONTAINER in tests/.env.local}"
: "${NEXUS_PG_DB:?set NEXUS_PG_DB in tests/.env.local}"
: "${NEXUS_PG_USER:?set NEXUS_PG_USER in tests/.env.local}"

# db_query <SQL>
# Runs the query and prints stdout in psql's default tabular format.
db_query() {
  local sql="$1"
  if [[ "${NEXUS_TEST_VERBOSE:-0}" == "1" ]]; then
    printf '  [db] %s\n' "$sql" >&2
  fi
  docker exec -i "$NEXUS_PG_CONTAINER" \
    psql -U "$NEXUS_PG_USER" -d "$NEXUS_PG_DB" -c "$sql"
}

# db_scalar <SQL>
# Returns a single scalar value, trimmed. Use for count(*), single-column
# WHERE id = ... queries, etc.
db_scalar() {
  local sql="$1"
  if [[ "${NEXUS_TEST_VERBOSE:-0}" == "1" ]]; then
    printf '  [db scalar] %s\n' "$sql" >&2
  fi
  docker exec -i "$NEXUS_PG_CONTAINER" \
    psql -U "$NEXUS_PG_USER" -d "$NEXUS_PG_DB" -tAc "$sql"
}

# db_count <quoted_table>
# Convenience wrapper for SELECT count(*).
db_count() {
  local table="$1"
  db_scalar "SELECT count(*) FROM $table"
}

# db_exists <SQL_BOOLEAN>
# Returns 0 if the boolean SQL evaluates to t, non-zero otherwise.
db_exists() {
  local sql="$1"
  local result
  result=$(db_scalar "SELECT EXISTS($sql)")
  [[ "$result" == "t" ]]
}

db_health() {
  docker exec "$NEXUS_PG_CONTAINER" pg_isready -U "$NEXUS_PG_USER" -d "$NEXUS_PG_DB" >/dev/null 2>&1
}
