# RETIRED — v2 measurement core

The closed-loop Python measurement engine in `benchmark/v2` is **retired**. It is
not valid for cross-gateway capacity or latency ranking, for the reasons documented
in Tiebin's PR #57 review: closed-loop self-throttling load (up to 25× delivered-load
skew within one table), censored percentiles (failures excluded from p50/p95/p99),
co-located topology, t-class burstable fleet, floating image tags, and unverified
upstream.

**Retired for cross-gateway measurement — source kept, not deleted:**

- `engine/runner.py`, `engine/metrics.py` (closed-loop load engine)
- `run_full_suite.sh` (turnkey path that bypassed the integrity gate)
- the S-04 / S-05 / S-06 scenario implementations
- v2 cache (S-08) measurement

The files remain in the tree for history and for the parts that carry forward — the
hooks A/B runtime-proof machinery (`scripts/hooks_toggle.sh`) and the S-02 padded
dataset + preflight are **kept**, to be promoted/hardened into v3.

**Use instead:** `benchmark/v3` — Story A (continuous open-loop latency observatory)
or Story B (rig saturation-campaign importer). See `../v3/README.md`.
