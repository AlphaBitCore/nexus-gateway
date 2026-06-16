# Merge Log — Teammate's 4 Files Integrated

**Date:** 2026-06-15
**Source:** Files received from teammate (Kanishk, ran on `Kanishks-NEW-MacBook-Air.local`) at ~3:30 PM.
**Files reviewed:** `models.py`, `runner.py`, `results_79414a61.{json,csv}` (Nexus), `results_e323ac29.{json,csv}` (LiteLLM), `results_de8d1107.{json,csv}` (Bifrost).

## Decisions

| File | Action | Reason |
|---|---|---|
| `engine/runner.py` | **Partial merge** — took the dual cache-header read | Their version reads `X-Nexus-Cache` (Nexus's actual emitted header) OR falls back to `x-cache-status`. Ours only read `x-cache-status` — a known harness bug from our earlier session notes. Applied to both stream and non-stream paths. |
| `engine/models.py` | **No change** — kept ours | Their version handles `${VAR}` and `$VAR`. Ours handles `${VAR}`, `${VAR:-default}`, AND `$VAR` — strictly a superset. Replacing would lose the `:-default` form. Their reported "401 fix" is something this codebase already had; they likely diverged from an older `models.py` and re-implemented part of it. |
| `requirements.txt` | **No change** — already pinned | `click==8.1.7` was already pinned from our session yesterday. |
| `results_79414a61.json` (Nexus) | **Imported** to `results/teammate-results_79414a61.json` | Used for combined-n analysis. |
| `results_e323ac29.json` (LiteLLM) | **Imported** to `results/teammate-results_e323ac29.json` | Same. |
| `results_de8d1107.json` (Bifrost) | **Imported** to `results/teammate-results_de8d1107.json` | Same. |
| Per-gateway `config/*.yaml` (3 files) | **Not received** — assumed unchanged | They said they "changed config to 3 VUs, 60s, 0s warmup." Our local YAML still has the original 20 VU / 300s / 30s warmup defaults, which is correct for full S-01. The teammate's small-run YAML values are reproducible via the `BENCH_VUS / BENCH_DURATION / BENCH_WARMUP` env overrides our `cli.py` already supports — no YAML change needed. |

## What is intentionally *not* merged

### Their nonce format
Their `runner.py` produces nonces via `f"bench-{hex(int(time.time()))[2:]}-{hex(req_id)[2:]}"` (e.g. `bench-66a9f3c2-1f`). Ours produces them via `secrets.token_urlsafe(8)` + `req_id:x` (e.g. `[bench nonce AbCdEf1g-1f]`).

- Both are PII-safe (mixed alphanumeric, no `\d{3+}` runs).
- Both produced 100% success on Nexus across independent runs today.
- Keeping ours because: (a) `secrets.token_urlsafe` is cryptographically strong and robust against future hook additions; (b) our existing result set (`results_ee4c202b`, `results_5b0f963d`, `results_2b5a8d2a`) was produced with this format and changing it would invalidate that dataset; (c) the module-level `_RUN_NONCE` lets you grep gateway logs by run.

This is a stylistic preference. The teammate's version is also acceptable and could be swapped in later.

### Their inline `os.environ.get("BENCH_UNIQUE_PROMPTS")` check in the worker
Theirs checks the env var inside the worker hot loop (once per request); ours checks once at module load and stores in `_UNIQUE_PROMPTS`. Functionally identical; ours is marginally more efficient (one env lookup per process vs. N per run). Kept ours.

## Code change applied

**`engine/runner.py`** — dual cache-header read at both call sites:

```python
# streaming path
cache_header = (
    sse.response.headers.get("X-Nexus-Cache")
    or sse.response.headers.get("x-cache-status", "")
)

# non-streaming path
cache_header = (
    resp.headers.get("X-Nexus-Cache")
    or resp.headers.get("x-cache-status", "")
)
```

Importance: closes the cache-hit telemetry hole identified in the bias-and-methodology review (Gotcha #4). With this fix, S-08 (cache feature scenario) will correctly report `cache_hit_rate_pct` on Nexus going forward. For cache-disabled scenarios it makes no difference, which is why our valid datasets (cache-disabled) were unaffected by the bug.

## Verification

- `python -c "from engine import runner, models; print(runner._RUN_NONCE)"` → imports cleanly, runs.
- The applied edits do not change any code path used by cache-disabled S-01 — pre-existing result files remain valid.
- Combined-n analysis written to `results/combined-comparison-s01.md`.

## What's still open

1. **YAML defaults**: teammate said they edited config YAMLs to 3 VU / 60s / 0s warmup. Our YAMLs still have 20 VU / 300s / 30s warmup. We rely on env overrides instead. No action needed unless the team wants to change the YAML defaults.
2. **`requirements.txt` clarification**: teammate said they "pinned click==8.1.7" — our `requirements.txt` already has it. Confirmed no-op.
3. **The 401 fix narrative**: teammate's "models.py fix prevented 401" claim is inaccurate against this codebase. Our `models.py` resolves `${VAR}` correctly (and has been doing so this whole session — that's why our Nexus runs returned 200 once the VK and provider credential were valid). Worth clarifying with the teammate so they know the canonical version is more complete than what they had.
