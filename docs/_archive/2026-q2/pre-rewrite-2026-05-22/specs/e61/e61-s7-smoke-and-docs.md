# E61-S7 — Full-Surface Smoke, ROI Verification, and Final Docs

> Story: e61-s7
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §NFR-4, §FR-6 (ROI accounting verification)
> Architecture: cross-cutting verification — not a new architecture surface
> Blocked by: e61-s1, s2, s2b, s3, s4, s5, s6, s6b (everything)
> Blocks: epic closeout

## User Story

As the engineer closing E61, I want a documented evidence trail that the dual-tier cache works end-to-end — no traffic_event regression, embedding cost stamped correctly, semantic hits derive `cache_status=HIT` correctly, time-sensitive prompts route around the cache, freshness metrics show expected counts — so we can ship E61 with confidence and the post-deploy handoff is clean.

## Tasks

### T1 — Full-surface smoke (CLAUDE.md binding)

- T1.1 Run `tests/scripts/smoke-gateway.py --all-ingress` against a freshly-seeded dev environment with E61 fully applied:
    - All routing rules' `response_cache_policy` migrated to the new shape.
    - Default `semantic.enabled=false` for most rules; one rule explicitly set to `semantic.enabled=true` with OpenAI text-embedding-3-small.
    - Freshness detector loaded with seed rules.
- T1.2 Verify the smoke passes for every ingress × model combination (29 models × 4 ingress).
- T1.3 Verify `traffic_event` cross-checks: every smoke request produces a row with the expected `gateway_cache_status`, `gateway_cache_kind`, `gateway_cache_skip_reason`, `embedding_cost_usd`.

### T2 — Targeted E2E for E61-specific behaviours

- T2.1 New synthetic test script `tests/scripts/smoke-e61.py`:
    - Send "What's the current stock price of AAPL?" → expect `gateway_cache_status=skipped`, `gateway_cache_skip_reason=time_sensitive`. Verify upstream WAS called.
    - Send "Summarize this article in 3 bullets" twice with semantic enabled → second request expects `gateway_cache_kind=semantic`, similarity ≥ 0.96. Verify upstream NOT called on the second.
    - Send the same prompt but rephrased ("Give me the 3 main points") → expect semantic hit on the second (same response served). Verify embedding cost stamped on BOTH requests (one cached the entry, the other looked it up).
    - Send a prompt large enough to exceed text-embedding-3-small's 8191-token context → expect `gateway_cache_skip_reason=oversize_for_embedding` on the L2 side; L1 unaffected.
    - Send identical concurrent requests (10 parallel) with a fresh cache → expect exactly 1 embedding call (singleflight). Verify via Prometheus counter delta.
- T2.2 Each test asserts HTTP shape + DB row + Prometheus delta.

- T2.6 — **Reindex-in-flight race test**. While a steady stream of L2-eligible requests is hitting the gateway (e.g., 5 RPS over 30s), PUT `/api/admin/semantic-cache/config` with a new embedding model id mid-stream. Assertions:
    - No request returns a 5xx during or after the swap.
    - Some requests in the swap window stamp `GatewayCacheSkipReason=semantic_reindex_in_progress` (the explicit detection path); none stamp `semantic_search_error`.
    - After the invalidate-all job completes (poll Hub job-status endpoint, expect <30s), subsequent requests embed under the new fingerprint and write new entries.
    - The old index's FT.DROPINDEX completed (verify via `FT._LIST` on Valkey or the Hub job's audit row).
    - Cache ROI page (or `admin_cache_roi` API) shows the embedding-model-id breakdown rolling over from old → new model without any "null" bucket bleed beyond the swap minute.
    - No orphan Valkey hash keys remain after 2x extract-TTL has elapsed (test polls `DBSIZE` baseline before / after).
    - Optional: trigger the swap WHILE a single L2 write is in flight (use a synthetic test that holds the write goroutine via an injected sleep) — assert the orphan-hash entry never appears in FT.SEARCH results under the new fingerprint.

- T2.7 — **Failure-mode coverage test**. For each `GatewayCacheSkipReason` introduced in S2 (10 reasons total), simulate the upstream failure and assert the right reason is stamped on the row + the right Prometheus counter increments:
    - `valkey_unavailable` — point ai-gateway at a non-existent Valkey port for one request, restore, assert.
    - `embedding_timeout` — point at a sleeping stub embedding server (200ms sleep > 100ms hard timeout), assert.
    - `embedding_provider_error` — stub returns 503, assert.
    - `embedding_dim_mismatch` — stub returns a vector of wrong dimension, assert.
    - `semantic_search_error` — break the FT.SEARCH index temporarily (DROPINDEX without recreate), assert.
    - `semantic_search_timeout` — inject a Valkey latency proxy, assert.
    - `semantic_reindex_in_progress` — see T2.6.
    - `semantic_unavailable` — boot ai-gateway against a stock Redis (no valkey-search module), assert L2 stays inert.
    - `time_sensitive` — see T2.1 already covers.
    - `oversize_for_embedding` — see T2.1 already covers.
    - Each scenario also asserts L1 (extract) continues to function — semantic failures must NEVER take down extract.

### T3 — Cost ROI verification

- T3.1 Seed the dev environment with a known cost pattern: 1000 requests of which 200 hit L1, 100 hit L2, 700 are full upstream calls.
- T3.2 Query the Cache ROI page and verify:
    - L1 savings reflect 200 × avoided cost.
    - L2 gross savings reflect 100 × avoided cost.
    - L2 net savings = gross - (embedding cost for the 800 misses that ran embedding).
    - The "net contribution" chart shows positive cumulative contribution.
- T3.3 If net contribution is negative for any sustained period in dev seed, document this as expected behaviour (small workloads have negative semantic-cache ROI; the page must surface this honestly, not hide it).

### T3a — Production Valkey migration runbook (P/A11 Round-1 review)

Mentioned in S3 T1.4 but not previously a tracked deliverable. Make it explicit:

- T3a.1 Create `docs/operators/ops/runbooks/e61-valkey-migration.md` with sections: (a) preflight check (Valkey RDB snapshot, valkey-search module presence verification on staging); (b) deploy steps (docker-compose pull + restart, expected downtime ~30s on single-EC2); (c) verification (FT.CREATE ping, sample HSET/FT.SEARCH); (d) rollback (revert image, restart, accept losing in-flight L2 entries); (e) operator alerts to watch during the migration window.
- T3a.2 Cross-link the runbook from `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §6 (Failure modes — operational recovery), `docs/developers/specs/e61-s3-...md` T1.4, and `docs/developers/specs/e61-s7-...md` T4.

### T4 — Architecture / requirements / SDD doc final pass

- T4.1 Re-read `response-cache-architecture.md` — make sure §3 reflects the as-built code (not the design intent if the two diverged during implementation).
- T4.2 Audit doc anchors — verify all cross-references in the three updated architecture docs resolve.
- T4.3 Update `MEMORY.md` index entry for `project_e61_smart_response_cache.md` to status "shipped 2026-MM-DD" instead of "in progress".
- T4.4 Confirm the architecture-doc-triggers.md row added in `e61-doc0` still matches the final file layout.

### T5 — CLAUDE.md binding check

- T5.1 No `TODO`, `FIXME`, `XXX`, `unimplemented`, `not implemented`, `stub`, `mock` in production code (test files are fine).
- T5.2 Per-package coverage ≥95% on all new packages; if any falls short, list in `scripts/.coverage-allowlist` with rationale AND get user approval.
- T5.3 Run `npm run check:configkey-coverage`, `npm run check:design-tokens`, `npm run check:i18n`, `npm run check:workspace-replace` — all green.
- T5.4 No new yaml secret fields (the embedding-provider credential lives in the existing Credential table).

### T6 — Two-round completion self-audit

- T6.1 Round 1 — the 4 questions:
    - Q1: Every story-level todo completed? (S1, S2, S2b, S3, S4, S5, S6, S6b, S7.)
    - Q2: Production code free of placeholder strings? (Grep diff.)
    - Q3: Every changed code path exercised by a test, or explicitly acknowledged untested?
    - Q4: No "we'll fix this later" left unmarked?
- T6.2 Round 2 — re-verify each Round-1 answer. If new issues found, fix and run Round 3 until two consecutive rounds are clean.

### T7 — Ask user about commit

- T7.1 Per CLAUDE.md "Commit reminder, no auto-commit" — when smoke + audit pass, ask the user to commit. Provide a suggested message of the form:
    > `feat(e61): smart response cache — extract+semantic dual tier with freshness skip + Valkey vector store`
    > Body: high-level summary + reference to requirements + SDD + memory.

## Acceptance Criteria

- A1: `smoke-gateway.py --all-ingress` passes on the E61-applied dev stack.
- A2: `smoke-e61.py` passes for every E61-specific scenario (time-sensitive skip, semantic hit, oversize skip, singleflight, cost stamping).
- A3: Cache ROI dashboard renders the new L2 net-contribution data correctly.
- A4: All `check:*` lints green.
- A5: 2-round self-audit clean.
- A6: User reviews the commit proposal and either accepts or rejects.

## Out of Scope (S7)

- Production Valkey migration — operational task, not code. Tracked in `docs/operators/ops/runbooks/e61-valkey-migration.md` (S3 T1.4).
- E68 / E69 / E70 / E71 — future epics (E62-E67 reserved by the cross-adapter embeddings / multimodal epic family).
- Smart Routing / Ai-Guard adoption of `inputstaging` — captured as tasks #14 / #15.
