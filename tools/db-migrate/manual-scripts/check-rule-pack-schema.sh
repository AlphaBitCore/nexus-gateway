#!/usr/bin/env bash
# Verify 4 rule-pack tables and traffic_event.blocking_rule column exist.
# psql -tA emits pipe-delimited rows (col|type|...); match "<col>|" prefix.
set -euo pipefail
CID=$(docker ps --filter name=postgres -q | head -1)

run() {
    local table="$1"; shift
    local cols="$1"; shift
    docker exec "$CID" psql -U postgres -d nexus_gateway -tA -c "\d $table" | \
        grep -E "^($cols)\|"
}

run rule_pack         "id|name|version|maintainer|description|signature|createdAt"
run rule              "id|packId|ruleId|category|severity|pattern|flags|description|labels"
run rule_pack_install "id|packId|pinVersion|boundHookId|enabled|installedAt"
run rule_override     "id|installId|ruleLocalId|disabled|severityOverride|updatedAt"

docker exec "$CID" psql -U postgres -d nexus_gateway -tA -c \
    "SELECT column_name || '|' || data_type FROM information_schema.columns WHERE table_name='traffic_event' AND column_name='blocking_rule'"
