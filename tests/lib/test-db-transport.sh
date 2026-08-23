#!/usr/bin/env bash
# tests/lib/test-db-transport.sh — self-test for db.sh's transport + safety split.
#
# db.sh used to refuse every non-local target outright, which kept prod data
# safe by making prod unreachable — and took the release gate with it:
# preflight.sh's first check is a db_health call, so `run-all.sh --blocking
# --target prod` died before it reached Hub, CP OAuth, or a single scenario.
#
# Transport and safety are separate concerns. This pins both halves:
# a remote target routes through ssh psql, and a statement that could change
# data is refused there no matter who asks.
#
# Run: bash tests/lib/test-db-transport.sh

set -uo pipefail

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_fail=0

_check() {  # _check <name> <expected> <actual>
  if [[ "$2" == "$3" ]]; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"
    _fail=1
  fi
}

# A stub ssh that echoes the command it was handed, so we can assert the
# remote path is taken without touching a real host.
_stub_dir="$(mktemp -d)"
trap 'rm -rf "$_stub_dir"' EXIT
cat > "$_stub_dir/ssh" <<'STUB'
#!/usr/bin/env bash
echo "SSH_INVOKED $*"
STUB
chmod +x "$_stub_dir/ssh"

_remote_env() {
  export PATH="$_stub_dir:$PATH"
  export NEXUS_TEST_TARGET=prod
  export NEXUS_SSH_HOST=someone@somewhere.example
  export NEXUS_SSH_PGUSER=nexus NEXUS_SSH_PGPASSWORD=x NEXUS_SSH_PGDB=nexus_gateway
}

# 1. A remote target must SOURCE cleanly — this is the line that killed preflight.
out=$(_remote_env; source "$_dir/db.sh" 2>&1; echo "SOURCED=$?")
_check "db.sh sources against a remote target" "0" "$(sed -n 's/.*SOURCED=//p' <<<"$out" | tail -1)"

# 2. A read goes over ssh, not docker.
out=$(_remote_env; source "$_dir/db.sh" >/dev/null 2>&1; db_scalar "SELECT count(*) FROM \"Provider\"" 2>&1)
case "$out" in
  SSH_INVOKED*) _check "a read on a remote target goes through ssh" "yes" "yes" ;;
  *)            _check "a read on a remote target goes through ssh" "yes" "no — got: ${out:0:80}" ;;
esac

# 3. Anything that could change data is refused on a remote target.
for sql in "DELETE FROM \"Provider\"" "UPDATE \"Provider\" SET name='x'" \
           "TRUNCATE traffic_event" "DROP TABLE traffic_event" \
           "INSERT INTO \"Provider\" (id) VALUES ('x')" \
           "SELECT 1; DELETE FROM \"Provider\""; do
  out=$(_remote_env; source "$_dir/db.sh" >/dev/null 2>&1; db_query "$sql" 2>&1; echo "RC=$?")
  rc=$(sed -n 's/.*RC=//p' <<<"$out" | tail -1)
  if [[ "$rc" == "0" || "$out" == *SSH_INVOKED* ]]; then
    _check "remote target refuses: ${sql:0:34}" "refused" "EXECUTED"
  else
    _check "remote target refuses: ${sql:0:34}" "refused" "refused"
  fi
done

# 4. The local target keeps using docker exec — no behaviour change there.
out=$(export NEXUS_TEST_TARGET=local NEXUS_PG_CONTAINER=c NEXUS_PG_DB=d NEXUS_PG_USER=u
      source "$_dir/db.sh" >/dev/null 2>&1; type -t db_scalar)
_check "local target still defines the helpers" "function" "$out"

[[ $_fail -eq 0 ]] && printf 'db transport self-test: PASS\n' || printf 'db transport self-test: FAIL\n'
exit $_fail
