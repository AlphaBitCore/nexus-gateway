#!/usr/bin/env bash
# tests/lib/db.sh — thin wrapper around `docker exec psql` for assertions.
#
# Usage:
#   source tests/lib/loadenv.sh         # populates NEXUS_PG_* etc.
#   source tests/lib/db.sh
#   db_query "SELECT count(*) FROM \"Provider\""
#
# Two transports, one API. A local target reaches Postgres by `docker exec`;
# any other target reaches it by `ssh psql` against NEXUS_SSH_HOST, using the
# NEXUS_SSH_PG* credentials from tests/.env.<target>.
#
# Transport and safety are separate concerns, and conflating them cost more
# than it bought: refusing every non-local target did keep prod data safe, but
# only by making prod unreachable — and preflight.sh opens with a db_health
# call, so it took the entire `run-all.sh --blocking --target prod` release
# gate down with it. The gate could never run against the environment it gates.
#
# So the transport opens and the safety becomes explicit: on a remote target
# these helpers run READS only. Anything that could change data is refused
# here, at the helper, rather than left to the discipline of every caller.
#
# Local wrappers honour NEXUS_PG_CONTAINER / NEXUS_PG_DB / NEXUS_PG_USER;
# remote ones honour NEXUS_SSH_HOST / NEXUS_SSH_PGUSER / NEXUS_SSH_PGPASSWORD /
# NEXUS_SSH_PGDB. All from tests/.env.<target>.

set -u

: "${NEXUS_TEST_TARGET:?source tests/lib/loadenv.sh first to set NEXUS_TEST_TARGET}"
if [[ "$NEXUS_TEST_TARGET" == "local" ]]; then
  : "${NEXUS_PG_CONTAINER:?set NEXUS_PG_CONTAINER in tests/.env.local}"
  : "${NEXUS_PG_DB:?set NEXUS_PG_DB in tests/.env.local}"
  : "${NEXUS_PG_USER:?set NEXUS_PG_USER in tests/.env.local}"
else
  : "${NEXUS_SSH_HOST:?set NEXUS_SSH_HOST in tests/.env.$NEXUS_TEST_TARGET — db.sh reaches a remote Postgres over ssh}"
  : "${NEXUS_SSH_PGUSER:?set NEXUS_SSH_PGUSER in tests/.env.$NEXUS_TEST_TARGET}"
  : "${NEXUS_SSH_PGPASSWORD:?set NEXUS_SSH_PGPASSWORD in tests/.env.$NEXUS_TEST_TARGET}"
  : "${NEXUS_SSH_PGDB:?set NEXUS_SSH_PGDB in tests/.env.$NEXUS_TEST_TARGET}"
fi

# _db_reject_writes <SQL>
# On a remote target, refuse anything that is not purely a read. The check is a
# leading-keyword allowlist plus a statement-separator ban, not a blocklist of
# dangerous words: a blocklist has to guess every way to spell a write, while an
# allowlist only has to know the handful of ways to spell a read.
_db_reject_writes() {
  [[ "$NEXUS_TEST_TARGET" == "local" ]] && return 0
  local sql="$1" head
  head=$(printf '%s' "$sql" | tr -d '(' | awk '{print toupper($1); exit}')
  case "$head" in
    SELECT|WITH|TABLE|VALUES|SHOW|EXPLAIN) ;;
    *)
      echo "db.sh: target=$NEXUS_TEST_TARGET is READ-ONLY; refusing '$head'." >&2
      return 1 ;;
  esac
  # `SELECT 1; DELETE ...` reads like a read to the keyword check.
  if [[ "${sql%;}" == *';'* ]]; then
    echo "db.sh: target=$NEXUS_TEST_TARGET is READ-ONLY; refusing a multi-statement query." >&2
    return 1
  fi
  return 0
}

# _db_psql <psql-flags> <SQL> — the transport split, in one place.
_db_psql() {
  local flags="$1" sql="$2"
  if [[ "$NEXUS_TEST_TARGET" == "local" ]]; then
    docker exec -i "$NEXUS_PG_CONTAINER" \
      psql -U "$NEXUS_PG_USER" -d "$NEXUS_PG_DB" $flags "$sql"
  else
    ssh -o StrictHostKeyChecking=no "$NEXUS_SSH_HOST" \
      "PGPASSWORD=$(printf %q "$NEXUS_SSH_PGPASSWORD") psql -h localhost \
       -U $(printf %q "$NEXUS_SSH_PGUSER") -d $(printf %q "$NEXUS_SSH_PGDB") \
       $flags $(printf %q "$sql")"
  fi
}

# db_query <SQL>
# Runs the query and prints stdout in psql's default tabular format.
db_query() {
  local sql="$1"
  if [[ "${NEXUS_TEST_VERBOSE:-0}" == "1" ]]; then
    printf '  [db] %s\n' "$sql" >&2
  fi
  _db_reject_writes "$sql" || return 1
  _db_psql -c "$sql"
}

# db_scalar <SQL>
# Returns a single scalar value, trimmed. Use for count(*), single-column
# WHERE id = ... queries, etc.
db_scalar() {
  local sql="$1"
  if [[ "${NEXUS_TEST_VERBOSE:-0}" == "1" ]]; then
    printf '  [db scalar] %s\n' "$sql" >&2
  fi
  _db_reject_writes "$sql" || return 1
  _db_psql -tAc "$sql"
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
  if [[ "$NEXUS_TEST_TARGET" == "local" ]]; then
    docker exec "$NEXUS_PG_CONTAINER" pg_isready -U "$NEXUS_PG_USER" -d "$NEXUS_PG_DB" >/dev/null 2>&1
  else
    db_scalar "SELECT 1" >/dev/null 2>&1
  fi
}
