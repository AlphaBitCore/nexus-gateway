---
name: test-cursor-adapter
description: >
  End-to-end synthetic test for the Cursor IDE Tier-1 protobuf normalizer
  (E46-S12). Sends a hand-rolled GetChatRequest protobuf to api2.cursor.sh
  through the prod compliance proxy at `cp.taskforce10x.com:3128` (or any
  proxy the user supplies), then verifies the resulting
  traffic_event_normalized row shows `kind=ai-chat`,
  `detectedSpec=cursor`, `model=claude-sonnet-4-6`, `confidence≈0.95`, and
  three decoded user/assistant/user messages. Use when validating the
  cursor adapter end-to-end without waiting to capture real Cursor IDE
  traffic, after touching anything under
  `packages/shared/traffic/adapters/ide/cursor/`, or after a Hub/CP/Compliance-
  Proxy redeploy that touched the normalize pipeline. Trigger keywords:
  test cursor, cursor protobuf test, cursor adapter smoke, cursor synthetic,
  /test-cursor-adapter.
user-invocable: true
---

# Test Cursor Adapter

Synthetic protobuf round-trip for the Cursor IDE Tier-1 normalizer.

## When to use

- After editing `packages/shared/traffic/adapters/ide/cursor/normalize.go`,
  `cursor.go`, or any of the protobuf wire schema docs.
- After redeploying Hub / compliance-proxy / ai-gateway to confirm the
  Tier-1 cursor.Normalize is wired and the protobuf decode survived.
- Whenever the user wants to see the cursor-shape audit row in the
  Control Plane UI but doesn't have live Cursor IDE traffic to drive.
- As a regression probe after any change to
  `shared/normalize/extract/` (the cursor adapter falls through to
  pattern probe + verbatim if Tier 1 returns ErrUnsupported, so a
  framework change shouldn't quietly drop confidence).

## How to run

```bash
python3 tests/manual/cursor_synthetic_chat.py
```

Flags:

| Flag        | Default                                                       | Notes |
|-------------|---------------------------------------------------------------|-------|
| `--proxy`   | `cp.taskforce10x.com:3128`                                    | Override for a different proxy. Local dev: `--proxy localhost:3128`. |
| `--target`  | `https://api2.cursor.sh/aiserver.v1.AiService/StreamUnifiedChatWithTools` | The path doesn't have to exist upstream — Cursor's server returns 404 but the proxy still MITMs + audits. |
| `--secure`  | (off — defaults to TLS verify disabled)                       | The Nexus MITM cert is signed by our internal CA; certifi's bundle doesn't include it even when system trust does. Pass `--secure` only if you've added the Nexus root CA to Python's bundle. |
| `--model`   | `claude-sonnet-4-6`                                           | Embedded in the synthetic ModelDetails sub-message; appears in the normalized payload's `model` field. |

The script:

1. Hand-encodes a GetChatRequest protobuf (field 2 = repeated
   ConversationMessage with field 1 = text + field 2 = role enum;
   field 7 = ModelDetails with field 1 = model_name). No third-party
   protobuf library — uses stdlib varint + tag encoding.
2. POSTs through the HTTPS proxy with `Content-Type:
   application/connect+proto` and a fake `Authorization: Bearer …`
   token. Upstream Cursor returns 401/403/404 (expected — we have no
   real auth), but the proxy still bumps + audits the bumped body.
3. Prints the trace id and a checklist for verifying the resulting
   row in the Control Plane UI.

## Verification (post-run)

The script's trace id is the canonical way to find the audit row. It
prints something like `cursor-synth-1778838655`. Two checks:

### A. Control Plane UI

1. Open `https://nexus.taskforce10x.com/traffic`.
2. Filter by `target_host=api2.cursor.sh:443` or grep for the trace id.
3. Open the row. The Normalized panel should show:
   - **green Tier-1 badge**: `Tier 1 · cursor · 0.95`
   - **Kind**: `ai-chat`
   - **Messages**: three entries (user / assistant / user) with the
     exact text from the script
   - **Model**: `claude-sonnet-4-6` (or whatever `--model` was)

The Raw tab shows the protobuf as a BinaryRef (size +
`application/connect+proto`); the decoded structure lives on the
Normalized tab.

### B. DB cross-check (faster than UI for CI / automation)

```bash
ssh -o StrictHostKeyChecking=no ec2-user@18.204.174.212 "
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \
  \"SELECT te.id, te.target_host, te.status_code,
           ten.response_status, ten.request_status,
           ten.request_normalized->>'kind' AS kind,
           ten.request_normalized->>'detectedSpec' AS spec,
           ten.request_normalized->>'model' AS model,
           ten.request_normalized->>'confidence' AS conf,
           jsonb_array_length(ten.request_normalized->'messages') AS msg_count
     FROM traffic_event te
     JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
    WHERE te.target_host LIKE '%cursor.sh%'
      AND te.timestamp > now() - interval '5 minutes'
    ORDER BY te.timestamp DESC LIMIT 5;\"
"
```

Expected:

| field | expected |
|---|---|
| `kind` | `"ai-chat"` |
| `spec` | `"cursor"` |
| `model` | `"claude-sonnet-4-6"` |
| `conf` | `"0.95"` (string from JSONB; ≈ 0.85 base + 0.05 model + 0.05 multi-message) |
| `msg_count` | `3` |
| `response_status` | `"ok"` |
| `request_status` | `"ok"` |

## What "wrong" looks like

| Symptom | Likely cause |
|---|---|
| No new traffic_event row | Source IP not in `accessControl.sourceIpAllowlist` in `compliance-proxy.{dev,prod}.yaml` (yaml-only since PR-7; redeploy required) — or `api2.cursor.sh` was removed from `interception_domain` |
| `kind=http-binary` instead of `ai-chat` | Hub Tier-1 registration for `cursor` adapter didn't apply — verify `adapters.RegisterTier1AdapterNormalizers(reg)` runs in `nexus-hub/cmd/nexus-hub/main.go` after `RegisterDefaultAIBuiltins` |
| `kind=ai-chat`, `spec="pattern:cursor"` (amber Tier-2 badge) | cursor.Normalize returned ErrUnsupported on the body; pattern probe took over. Inspect the protobuf bytes — script's `--target` path matters (Cursor adapter only claims chat paths) |
| `msg_count < 3` or `model` empty | Wire layout drift — check `packages/shared/traffic/adapters/ide/cursor/normalize.go::normalizeRequestProto` field numbers against the script's encoder (field 2 conv, field 7 ModelDetails, field 1 inside ModelDetails) |

## Background

The cursor adapter implementation lives at:

- `packages/shared/traffic/adapters/ide/cursor/cursor.go` — original
  runtime ExtractRequest path (segments-based output for hooks).
- `packages/shared/traffic/adapters/ide/cursor/normalize.go` — E46-S12
  Tier-1 Normalize producing structured `Messages[]` + `Model` via the
  same protowire field-walk.
- `tests/manual/cursor_synthetic_chat.py` — the script this skill runs.

Wire schema reverse-engineered from the cursor-agent bundle and
cross-validated against `github.com/eisbaw/cursor_api_demo`'s
`cursor_proper_protobuf.py`. See
[[project_cursor_capture_investigation]] memory for the captures that
seeded the field-number guesses.

Companion skill for end-to-end ChatGPT-web SSE testing:
`prod-debug` (manual flow); for AI Gateway full-surface smoke:
`smoke-gateway`. For audit-pipeline freshness:
`prod-deploy` Step 7d.
