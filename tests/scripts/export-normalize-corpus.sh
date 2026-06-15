#!/usr/bin/env bash
# export-normalize-corpus.sh — re-export real prod wire samples into the
# normalize conformance corpus.
#
# For each (event_id, side, case_dir, meta_json) entry below this script:
#   1. ssh's into prod (read-only) and selects the inline body column from
#      traffic_event_payload as text,
#   2. unwraps the stored payload envelope into raw wire bytes
#      ({"kind":"inline","encoding":"base64"|"raw",...} envelope, a quoted
#      JSON string, or a bare JSON object whose ::text IS the wire),
#   3. writes corpus/<case_dir>/wire and corpus/<case_dir>/meta.json.
#
# Usage:  tests/scripts/export-normalize-corpus.sh
#   (run from the repo root; requires tests/.env.prod with NEXUS_SSH_* vars)
#
# IMPORTANT — this script does NOT scrub PII. Scrubbing (replacing real
# account/org UUIDs, emails, message ids, session tokens with synthetic
# same-shape values) is a deliberate MANUAL review step after every export:
# automated scrubbing cannot be trusted to catch novel identifier shapes,
# and a human must eyeball each new wire file before it is committed.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CORPUS_DIR="$REPO_ROOT/packages/shared/transport/normalize/conformance/corpus"

# shellcheck source=../lib/loadenv.sh
source "$REPO_ROOT/tests/lib/loadenv.sh" prod

: "${NEXUS_SSH_HOST:?NEXUS_SSH_HOST missing from tests/.env.prod}"
: "${NEXUS_SSH_PGPASSWORD:?NEXUS_SSH_PGPASSWORD missing from tests/.env.prod}"
: "${NEXUS_SSH_PGUSER:?NEXUS_SSH_PGUSER missing from tests/.env.prod}"
: "${NEXUS_SSH_PGDB:?NEXUS_SSH_PGDB missing from tests/.env.prod}"

# ── Case list ───────────────────────────────────────────────────────────────
# Format: <traffic_event_id>|<side:request|response>|<case_dir>|<meta_json>
CASES=(
  '2a9a971c-353f-40a1-bef2-2d0ec5e40c7a|response|anthropic-sse-tooluse-only|{"adapterType":"anthropic","direction":"response","endpointPath":"/v1/messages","stream":true}'
  # Originally exported as "anthropic-sse-text", but the captured stream has
  # 0 text_delta / 12 input_json_delta frames (a Bash tool command stream) —
  # renamed to describe what it pins. The genuine text-delta case
  # (corpus/anthropic-sse-text) is a LOCAL gateway capture documented in
  # corpus/BASELINE.md: prod holds no pure text-delta /v1/messages stream
  # (the only two matches are tool-dominated mixed streams).
  'da372b09-d5df-4de0-861d-afb265980798|response|anthropic-sse-tooluse-bash|{"adapterType":"anthropic","direction":"response","endpointPath":"/v1/messages","stream":true}'
  'b7d0a75a-78b8-424e-8391-196d72f94d92|response|json-no-content-type|{"adapterType":"cursor","direction":"response","contentType":""}'
  '3a79a761-5cf8-4c54-a698-d788f6aad7dd|response|cursor-connectrpc-resp|{"adapterType":"cursor","direction":"response","contentType":"application/connect+proto"}'
)

run_psql() {
  local sql=$1
  # shellcheck disable=SC2029
  ssh -o StrictHostKeyChecking=no "$NEXUS_SSH_HOST" \
    "PGPASSWORD=$NEXUS_SSH_PGPASSWORD psql -h localhost -U $NEXUS_SSH_PGUSER -d $NEXUS_SSH_PGDB -At -c \"$sql\"" \
    2>/dev/null
}

# Python unwrap program kept in a variable (NOT a heredoc piped to python3's
# stdin) so the wire bytes can flow through stdin untouched.
UNWRAP_PY=$(cat <<'PYEOF'
import base64, json, sys

raw = sys.stdin.read()
if not raw.strip():
    sys.exit("empty body column — wrong event id or side?")
v = json.loads(raw)

out = sys.stdout.buffer
if isinstance(v, str):
    # Body stored as a quoted+escaped JSON string: the string IS the wire.
    out.write(v.encode("utf-8"))
elif isinstance(v, dict) and v.get("kind") == "inline":
    enc = v.get("encoding")
    if enc == "base64":
        out.write(base64.b64decode(v["inlineBytes"]))
    elif enc == "raw":
        ib = v["inlineBytes"]
        if isinstance(ib, str):
            out.write(ib.encode("utf-8"))
        else:
            # Embedded JSON value; jsonb storage already normalized the
            # original byte form, so a compact re-serialization is the
            # closest reproducible representation.
            out.write(json.dumps(ib, separators=(",", ":"),
                                  ensure_ascii=False).encode("utf-8"))
    else:
        sys.exit(f"unknown inline encoding: {enc!r}")
else:
    # Bare JSON object/array: the ::text rendering IS the wire.
    out.write(raw.rstrip("\n").encode("utf-8"))
PYEOF
)

unwrap_to_wire() {
  # stdin: the ::text rendering of the Json column. stdout: raw wire bytes.
  python3 -c "$UNWRAP_PY"
}

for entry in "${CASES[@]}"; do
  IFS='|' read -r event_id side case_dir meta_json <<<"$entry"
  case "$side" in
    request)  col="inline_request_body" ;;
    response) col="inline_response_body" ;;
    *) echo "bad side '$side' for $case_dir" >&2; exit 1 ;;
  esac

  dest="$CORPUS_DIR/$case_dir"
  mkdir -p "$dest"

  echo "exporting $event_id ($side) -> corpus/$case_dir/wire"
  run_psql "SELECT ${col}::text FROM traffic_event_payload WHERE traffic_event_id='${event_id}'" \
    | unwrap_to_wire > "$dest/wire"

  if [[ ! -s "$dest/wire" ]]; then
    echo "ERROR: corpus/$case_dir/wire is empty" >&2
    exit 1
  fi

  printf '%s\n' "$meta_json" | python3 -m json.tool --indent 2 > "$dest/meta.json"
done

echo
echo "Done. REMINDER: scrub PII manually before committing (see header comment)."
