---
name: test-geminiweb-adapter
description: >
  End-to-end synthetic test for the Gemini Web (gemini.google.com) Tier-1
  normalizer (E46-S12 + Tier 2 batchexecute detector). Sends a hand-rolled
  Google batchexecute POST (`f.req=` form-urlencoded JSON envelope) to
  gemini.google.com's StreamGenerate endpoint through the prod compliance
  proxy at `cp.taskforce10x.com:3128`, then verifies the resulting
  traffic_event_normalized row shows `kind=ai-chat`,
  `detectedSpec=gemini-web`, the extracted user prompt, and confidence
  around 0.85. Use when validating gemini-web adapter end-to-end without
  waiting to capture real Gemini web traffic, after touching
  `packages/shared/traffic/adapters/web/geminiweb/` or
  `packages/shared/transport/normalize/extract/detector.go`'s BatchExecuteDetector,
  or after Hub/CP/Compliance-Proxy redeploy that changed the normalize
  pipeline. Trigger keywords: test gemini-web, gemini web test,
  gemini batchexecute test, geminiweb adapter smoke,
  /test-geminiweb-adapter.
user-invocable: true
---

# Test Gemini Web Adapter

Synthetic batchexecute round-trip for the Gemini Web Tier-1 normalizer.
Companion to `test-cursor-adapter` (cursor protobuf synthetic).

## When to use

- After editing `packages/shared/traffic/adapters/web/geminiweb/normalize.go`
  or `packages/shared/transport/normalize/extract/detector.go::BatchExecuteDetector`.
- After redeploying Hub / CP / compliance-proxy to confirm gemini-web
  Tier 1 is wired.
- When you want a Gemini-web-shaped audit row without driving real
  gemini.google.com traffic.

## How to run

```bash
python3 tests/manual/geminiweb_synthetic_chat.py
```

Flags: `--proxy` (default `cp.taskforce10x.com:3128`), `--target`
(default StreamGenerate URL), `--prompt`, `--locale`, `--secure`
(default insecure — Nexus CA may not be in certifi).

## Verification (after the script prints its trace id)

### A. Control Plane UI

`https://nexus.taskforce10x.com/traffic` → filter
`target_host=gemini.google.com:443`. Open the row. Expected:
- Tier-1 badge (green): `Tier 1 · gemini-web · 0.85`
- Kind: `ai-chat`
- 1 user Message with the `--prompt` text
- Model: empty on request side (carried in response chunks only)

### B. DB cross-check

```bash
ssh -o StrictHostKeyChecking=no ec2-user@18.204.174.212 "
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \
  \"SELECT ten.request_normalized->>'kind' AS kind,
           ten.request_normalized->>'detectedSpec' AS spec,
           ten.request_normalized->>'confidence' AS conf,
           ten.request_normalized->'messages'->0->'content'->0->>'text' AS prompt
     FROM traffic_event te JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
    WHERE te.target_host LIKE '%gemini.google.com%' AND te.timestamp > now() - interval '5 minutes'
    ORDER BY te.timestamp DESC LIMIT 5;\"
"
```

Expected: `kind=ai-chat`, `spec=gemini-web`, `conf≈0.85`,
`prompt` matches `--prompt` flag.

## Failure modes

| Symptom | Cause |
|---|---|
| No new row | Source IP not in allowlist, or gemini.google.com removed from interception_domain |
| `kind=http-text` | Tier-1 wiring missing — verify `adapters.RegisterTier1AdapterNormalizers` runs in binary main |
| `spec=pattern:google-batchexecute-chat` (amber) | Tier-1 returned ErrUnsupported; Tier-2 detector caught it instead. Still works; investigate adapter route |
| `msg_count=0` | Google changed `f.req` inner index — update BatchExecuteDetector.decodeRequest |
| `kind=http-binary` | Body byte-sniff failed both `f.req=` and `)]}'` — different endpoint? compressed? |

## Background

- `packages/shared/traffic/adapters/web/geminiweb/normalize.go` —
  ~70-line Tier-1 shim that delegates to
  `extract.BatchExecuteDetector` and stamps `adapterID=gemini-web`.
- `packages/shared/transport/normalize/extract/detector.go` — actual wire
  decoder for the batchexecute envelope. Same decoder also serves
  Tier-2 fallback for any other Google web AI surface that hasn't
  yet earned a dedicated adapter.
- `tests/manual/geminiweb_synthetic_chat.py` — this script's runner.

Wire layout captured against prod traffic_events
78911179-d123-4810-bf31-7bf4defde85a and
fa2b035a-c8ad-45f0-bbdb-3b436f1851c2 (StreamGenerate on
gemini.google.com, 2026-05-15).

See `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` "Adding support for a
new non-JSON wire format" for the framework cookbook.
