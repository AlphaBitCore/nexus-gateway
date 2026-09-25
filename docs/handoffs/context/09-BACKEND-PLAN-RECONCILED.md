# 05 · Backend Plan — Reconciled (portable context)

> **How to use this file:** self-contained context for the Nexus benchmark
> backend. Paste it into a fresh chat/AI to resume with full understanding of
> (a) the pasted "artifact ingestion" plan, (b) what is actually already built,
> (c) where they conflict, and (d) the agreed single contract. It does not
> depend on the other nexus-context files, though it references them.
>
> Reads as: *"Here is a proposed backend plan and the real system it must merge
> into — reconcile them and evolve the existing code, do not fork."*

---

## 0. One-paragraph situation

The team has a **static benchmark comparison site** (Bifrost-style) + a **live
`obs-api` backend** (Run button → live SSE benchmark) already built and pushed
to `Kaushik985/nexus-benchmark-site`. A detailed **"backend build plan"** was
then written (artifact normalizer + rich schema + publish gates + S3/CloudFront
+ deferred phone-home API). That plan is good but **overlaps** the existing
exporter/obs-api and proposes a **second data contract**. The decision captured
here: **converge onto one contract, evolve the existing code, adopt the plan's
net-new ideas (methodology gates, comparisons, governance fields), and don't
build a parallel normalizer or a second data model.**

---

## 1. What already exists (ground truth — verified, pushed)

Repo: **`Kaushik985/nexus-benchmark-site`** (private). Key commits:
`0b0a030` site · `37aa2b5` exporter · `fafbddd` provenance+EXCLUDE · `6cfbc4a`
Observatory V1 · `9cafb34` obs-backend B0 · `7ca6b58`/`ec29ad8` integration.

### 1a. The exporter (already the "normalizer")
`scripts/export-benchmark-json.py`:
- **Reads the rig's REAL artifact layout** — `<run>/<gateway>/summary-*.json`
  dirs (verified against `AlphaBitCore/llm-gateway-benchmark/benchmark-results`).
  Non-streaming = saturated stage; streaming = knee stage (matches
  `compare-gateways.py`).
- Emits **`data.json`** + appends **`history/<ISO>.json`** + `history/index.json`
  (idempotent — unchanged evidence mints no snapshot).
- **Provenance** block: `sourceRepo`, `sourceCommit`, `remoteVerified`.
- **`EXCLUDE` list** — keeps non-public gateways (currently `agentgateway`) out
  of published output; changing it is a reviewable code diff, not a data edit.
- `--embed index.html` refreshes the inline file:// fallback; `--history <dir>`.

### 1b. The live backend (obs-api) — `obs-backend/` (Go, B0 built + tested)
- Endpoints: `POST/GET /api/runs`, `GET /api/runs/{id}/stream` (SSE), `/api/presets`, `/healthz`.
- Run state machine with **guaranteed teardown** + **concurrency=1** + **layered timeouts** (40m in-proc < 45m watchdog).
- Driver seam: **sim** (works now), **compose** (local arena), **ec2** (B1 slot).
- `results.Persist` writes `history/<ts>.json` in the SAME schema as the exporter.
- Guards: daily cap, per-IP cooldown, CORS, admin-token for soak preset.

### 1c. Current data contract (what the site + obs-api emit today)
```json
{ "meta": { "runDate", "compute", "upstream", "rounds", "provenance": {...},
            "note", "higherIsBetter": {...} },
  "gateways": { "<id>": { "label", "kind", "governed" } },
  "tiers": [ { "id", "label", "streaming",
               "rows": { "<id>": { "rps", "latP99Ms", "ttftP95Ms", "okPct" } } } ] }
```
Served files today: `data.json`, `history/<ts>.json`, `history/index.json`.

---

## 2. The pasted plan (summary of intent)

A benchmark-artifact ingestion + serving layer:
- **V1 static:** normalizer (`normalize.py`) reads artifacts → `dist/data/`
  (`runs.json`, `latest.json`, `runs/:id.json`, `comparisons/`) → S3+CloudFront.
- **V2 API:** thin Go server + `/internal/*` phone-home ingest — **only when the
  arena is live** (correctly deferred).
- Rich per-run schema: `methodology_status` enum, `publish_status`, per-gateway
  governance flags (`audit_enabled`, `audit_loss_detected`, `pii_redaction_*`,
  `traffic_events_captured`), `methodology{}` block, `scenarios[]`, `caveats[]`,
  `agent_gateway_claims_public` guard.
- Publish-blocker validation (`validate_methodology.py`), comparison builder,
  GitHub Action ingest on `benchmark-results/**` push.
- Explicit "must NOT" list (no fabrication, no public agent-gateway pre-sign-off,
  no `/internal/*` before Kanishk's contract, no secrets, nothing publishable
  without James).

---

## 3. Reconciliation — plan component → reality

| Plan component | Status vs built | Action |
|---|---|---|
| C1 Normalizer (`normalize.py`) | **Exists** as `export-benchmark-json.py` (+ provenance + EXCLUDE) | **Evolve it** — add rich schema; keep its real input reader |
| C2 Runs index (`runs.json`) | Partial — `history/index.json` | Extend index with best_rps/etc. |
| C3 Comparison builder | **Net-new** | Adopt |
| C4 Phone-home `/internal/*` | **= obs-api** (built); `/internal/*` correctly deferred | Keep deferred per INTEGRATION.md |
| C5 S3 + CloudFront | **Divergence** — we chose GitHub Pages | Decide one (see §5) |
| Methodology status + publish gate | **Net-new + high value** | Adopt |
| Governance per-gateway fields | Net-new; needs Tieben's V2 artifacts | Add when format lands |
| "Must NOT" list | Matches existing discipline | Already followed |

---

## 4. The 3 divergences that must be resolved

1. **Two data contracts.** Today: `data.json`/`tiers[].rows{rps,latP99Ms,ttftP95Ms,okPct}`. Plan: `runs/:id.json` with a much richer schema. They don't interoperate. **Decision: adopt the richer schema as the SINGLE contract; migrate the exporter + `obs-api` `results.Persist` + `index.html` to it in one pass.** One shape, static and live.
2. **"Live backend = V2/future" is stale.** obs-api B0 is built (sim driver + SSE + state machine). Only `ec2` driver + `/internal/*` ingest are deferred. Don't reshelve it.
3. **🐛 Input-format bug in the pasted normalizer.** It assumes `benchmark-results/v1/raw/<gateway>.json` + one `SUMMARY.csv`. The rig actually emits `<run>/<gateway>/summary-*.json` dirs. The built exporter reads the real format. **Keep the exporter's reader; graft the plan's richer output schema on top.** Writing the pasted `normalize.py` verbatim would parse nothing.

---

## 5. Hosting: reconcile S3+CloudFront vs GitHub Pages
- **Chosen (built for):** GitHub Pages (see repo `PUBLISH.md`). Blocked only by: private repo on free plan → need a **public site-only repo** (recommended) or plan upgrade.
- **Plan assumes:** S3 + CloudFront at `benchmark.alphabitcore.com` + GitHub Action ingest.
- **Reconcile:** pick one. Pages = free/simple on a public repo. S3 becomes relevant only if Kanishk's arena pushes artifacts to S3 (V2). Owner: James/infra. **Do not build both.**

---

## 6. The agreed single contract (proposed — richer schema, superset)
Keep today's fields, add the plan's. Per-run object:
```jsonc
{
  "run_id", "benchmark_version", "created_at", "operator",
  "methodology_status": "draft|indicative|externally_referenced|publishable|invalid|superseded",
  "publish_status": "<same enum>",
  "agent_gateway_claims_public": false,
  "environment": { "cloud","instance_type","layout","run_mode","mock_provider","loadgen","region" },
  "provenance": { "sourceRepo","sourceCommit","remoteVerified","docker_image_tags":{} },  // from built exporter
  "gateways": [ {
      "name","mode","image","version",
      "rps","ttft_p95_ms","e2e_p95_ms","itl_ms","error_rate","timeout_rate","streaming_success_rate",
      "hooks_enabled","compliance_enabled","audit_enabled","cache_enabled","pii_redaction_enabled",
      "traffic_events_captured","audit_loss_detected"      // governance — populate when Tieben's V2 carries them
  } ],
  "scenarios": [ { "name","prompt_type","concurrency","duration_s","vu_count" } ],
  "methodology": { "same_machine","same_instance_type","configs_matched","caching_matched",
                   "hooks_mode_labeled","repeat_count","statistically_powered","independent_verification","notes" },
  "caveats": [ ... ]
}
```
Plus `runs.json` (index) + `latest.json` (latest publishable). **Note:** the
current frontend reads `tiers[].rows`; migrating means updating `index.html`'s
render + `obs-api` `results.Persist` to this shape — that is the migration cost,
and it must be done once, not forked.

`methodology_status` derivation + publish-blockers (adopt from plan):
- any of `configs_matched|caching_matched|same_machine == false` → `indicative`.
- `agent_gateway_claims_public == true` without James sign-off → **BLOCK publish**.
- nothing `publishable` without explicit James sign-off.

---

## 7. Recommended sequencing (converge, don't fork)
1. **Agree the single contract (§6)** — team, before code. This file is that proposal.
2. **Evolve `export-benchmark-json.py`** → richer schema + `validate_methodology.py` publish-gate + comparison builder. Keep real rig-input reader + provenance + EXCLUDE. Unify EXCLUDE with `agent_gateway_claims_public` (one switch).
3. **Migrate `obs-api results.Persist` + `index.html`** to the same contract.
4. **Hosting decision** (§5), then wire the ingest (GitHub Action for static path; obs-api already covers the live path).
5. **Deferred (unchanged):** `/internal/*` ingest (until Kanishk's Run() contract), governance fields (until Tieben's V2 artifact format), agent-gateway public rows (until James).

---

## 8. Open questions (deduped against INTEGRATION.md + PUBLISH.md)
| # | Question | Owner | Blocks | Already tracked? |
|---|---|---|---|---|
| 1 | Phone-home vs SSM | James/Kanishk | `/internal/*` | Yes (INTEGRATION.md) — recommend phone-home |
| 2 | Parallel vs sequential runs + mock sizing | Tieben | fleet size, run duration, comparability | Yes (INTEGRATION.md) |
| 3 | **V2 artifact format** | Tieben | normalizer input reader + governance fields | **New — the real schema blocker** |
| 4 | Agent-gateway claims public? | James | EXCLUDE / `agent_gateway_claims_public` | Yes |
| 5 | S3+CloudFront vs GitHub Pages | James/infra | hosting/deploy | Yes (PUBLISH.md) — pick one |
| 6 | `benchmark.alphabitcore.com` DNS | James/infra | public launch | Yes |
| 7 | V1 publish label | James | `methodology_status` value | New — likely `externally_referenced` |

---

## 9. Hard rules (must survive any refactor)
- Serve only what the rig produced — **never fabricated/guessed numbers**.
- **No public Agent-Gateway claims** until James signs off (one switch: EXCLUDE ⇄ `agent_gateway_claims_public`).
- **Never mix live gateway traffic with benchmark traffic** in one data model.
- **No `/internal/*` ingest** until Kanishk's Run() contract is defined.
- **No secrets** in any artifact or API response.
- **Nothing `publishable`** without James's methodology sign-off.

---

## 10. Pointers
- Built code: `Kaushik985/nexus-benchmark-site` → `scripts/export-benchmark-json.py`, `obs-backend/`, `index.html`, `history/`.
- Contracts: `obs-backend/INTEGRATION.md` (obs-api↔Arena), `PUBLISH.md` (hosting), `HANDOFF.md` (delegation).
- Rig: `AlphaBitCore/llm-gateway-benchmark` (`benchmark-results/`, `compare-gateways.py`, `deploy.sh`).
- Sibling context: `00-OVERVIEW`, `01-PLANS-EVOLUTION`, `02-BACKEND`, `03-FRONTEND`, `04-CONTAINERS-AND-DEPLOY`.
