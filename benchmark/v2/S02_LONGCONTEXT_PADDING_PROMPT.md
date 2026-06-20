# Claude Code Prompt — S-02 Long Context Dataset Padding

> Paste everything below this line into Claude Code as a single prompt.

---

## Goal

Pad the S-02 long-context benchmark dataset (`benchmark/v2/datasets/long_context_v2.json`) so every prompt reaches a true ~16,000 token context window. The scenario code (`benchmark/v2/scenarios/s02_long_context.py`) is already correct and does not need to change. Only the dataset needs work.

## Non-goals

- Do not modify `s02_long_context.py`
- Do not modify any other scenario file
- Do not touch `engine/runner.py`, `engine/metrics.py`, or any gateway adapter
- Do not add new dependencies to `requirements.txt`
- Do not create new scenario files

---

## Current state of the dataset

File: `benchmark/v2/datasets/long_context_v2.json`

The file currently has 10 prompt strings but the entire file is only ~2,794 bytes. The prompts are one-sentence topic instructions — completely unpadded. Feeding these to a 16k context benchmark produces results that are indistinguishable from S-01 (short chat), defeating the entire purpose of the scenario.

Current prompts (all 10 UUIDs and topics are correct — do not change them):

1. `[REQUEST-af3e0bf1...]` — semiconductor industry competitive dynamics
2. `[REQUEST-a411a2e5...]` — central bank monetary policy transmission
3. `[REQUEST-d27c51a6...]` — distributed database technical architecture
4. `[REQUEST-021a7132...]` — value vs growth investing comparison
5. `[REQUEST-23839121...]` — HTTPS request end-to-end flow
6. `[REQUEST-f01027b6...]` — private equity business model
7. `[REQUEST-7699478f...]` — microservices architecture and design patterns
8. `[REQUEST-ead6f87c...]` — options pricing mechanics
9. `[REQUEST-82378311...]` — ESG in institutional investing
10. `[REQUEST-299244f5...]` — recommendation systems end-to-end

---

## What needs to be built

### 1. A Python padding script

Create `benchmark/v2/scripts/pad_long_context_dataset.py`.

This script must:

1. Load `benchmark/v2/datasets/long_context_v2.json`
2. For each of the 10 prompts, construct a padded version that reaches **~16,000 tokens** total (measured as `len(text) // 4` as a fast approximation — this is the standard cl100k_base approximation used by gpt-4o-mini)
3. Write the padded result to `benchmark/v2/datasets/long_context_v2_padded.json` in the same schema as the original
4. Print a summary table showing: prompt index, original token count, padded token count, and whether the target was met

### 2. The padding strategy

Each prompt has two parts: the **instruction** (the original one-line topic sentence, keep exactly as-is including the `[REQUEST-uuid]` prefix) and the **context body** (the padding you add).

The padding must be **semantically relevant to the prompt topic**, not garbage text or lorem ipsum. Garbage padding causes models to produce degenerate outputs that contaminate the benchmark results. Each padding block should be a realistic document that a real user might paste into a chat: a research excerpt, a technical reference, a market report, historical data, a specification, or similar. It does not need to be factually perfect but it must be coherent, domain-appropriate prose.

**Padding structure per prompt:**

```
[REQUEST-uuid] <original instruction>

--- CONTEXT ---

<~15,500 tokens of domain-relevant background text>

--- END CONTEXT ---

Based on the context above, <repeat original instruction without UUID>
```

The instruction is repeated at the end (without the UUID prefix) so the model has a clean anchor after reading the full context. This mirrors how real long-context RAG requests are structured.

**Target token counts:**

| Metric | Value |
|--------|-------|
| Target total tokens per prompt | ~16,000 |
| Tolerance | ±500 tokens (15,500–16,500) |
| Approximation method | `len(text) // 4` |
| Minimum acceptable | 14,000 tokens |

### 3. Padding content per topic

Generate the padding inline in the script as multi-line Python string literals (not loaded from external files — keep it self-contained). Each block should be 60,000–62,000 characters of domain prose to hit the ~15,500 token target.

Here is the required domain for each prompt's padding block:

| # | UUID prefix | Domain for padding |
|---|-------------|-------------------|
| 1 | `af3e0bf1` | Semiconductor industry: include sections on TSMC/Samsung/Intel market share data, CHIPS Act provisions, HBM memory demand curves, EUV lithography economics, lead time trends 2020–2026, geopolitical export controls timeline |
| 2 | `a411a2e5` | Monetary policy: include Fed/ECB/BOJ historical rate decisions 2015–2026, QE/QT balance sheet mechanics, credit spread data, Taylor Rule derivations, transmission lag research, 2022–2023 inflation episode detail |
| 3 | `d27c51a6` | Distributed systems: include Raft algorithm pseudocode walkthrough, Paxos variants comparison, CockroachDB/Spanner/Cassandra architecture details, vector clocks, conflict-free replicated data types (CRDTs), replication factor tradeoffs |
| 4 | `021a7132` | Investing: include Buffett/Munger/Lynch/Klarman quotations and investment letters excerpts, P/E vs P/FCF vs EV/EBITDA historical data, factor return research (Fama-French), behavioural finance literature (Kahneman, Thaler) |
| 5 | `23839121` | Networking/TLS: include RFC 8446 (TLS 1.3) handshake sequence detail, X.509 certificate chain validation steps, OCSP stapling, HTTP/2 frame format, HPACK compression, QUIC vs TCP comparison, DNS-over-HTTPS |
| 6 | `f01027b6` | Private equity: include LBO model walkthrough with numbers, IRR vs MOIC mechanics, management fee and carry structures, vintage year return data, KKR/Blackstone/Apollo fund performance history, portco operational improvement playbooks |
| 7 | `7699478f` | Microservices: include Netflix OSS architecture history, Kubernetes service mesh (Istio/Linkerd) config examples, Saga pattern vs 2PC tradeoff analysis, OpenTelemetry trace propagation spec, Kafka consumer group rebalancing mechanics |
| 8 | `ead6f87c` | Options/derivatives: include Black-Scholes derivation steps, binomial tree model, VIX methodology, vol surface construction, delta-hedging P&L attribution, term structure of vol, 0DTE options mechanics, realized vs implied vol spread |
| 9 | `82378311` | ESG: include EU SFDR/CSRD regulatory text excerpts, MSCI ESG rating methodology, carbon accounting Scope 1/2/3 definitions, climate scenario analysis (TCFD), stranded asset risk models, greenwashing enforcement cases |
| 10 | `299244f5` | Recommender systems: include Netflix Prize winning approach details, Amazon item-item collaborative filtering patent, YouTube DNN architecture (2016 paper), two-tower model design, feature store architecture, A/B testing frameworks for ranking |

### 4. Output schema

The padded JSON must match this exact schema (same as original):

```json
{
  "version": "v2-padded",
  "count": 10,
  "target_tokens_per_prompt": 16000,
  "prompts": [
    "<full padded prompt string 1>",
    "<full padded prompt string 2>",
    "..."
  ]
}
```

Keep the `prompts` array as a flat list of strings — `s02_long_context.py` calls `load_prompts("long_context_v2.json")` and expects that schema. After padding, **rename the output to `long_context_v2.json`** (overwrite the original) OR update `s02_long_context.py` to load `long_context_v2_padded.json` — pick one and be consistent.

### 5. Validation script

Add a validation function at the bottom of the padding script that runs automatically after writing the file:

```python
def validate(path):
    data = json.load(open(path))
    assert data["count"] == 10, "Must have exactly 10 prompts"
    results = []
    for i, p in enumerate(data["prompts"]):
        tokens = len(p) // 4
        ok = 14000 <= tokens <= 17000
        results.append((i+1, tokens, "PASS" if ok else "FAIL"))
    print("\nValidation results:")
    print(f"{'#':>3}  {'tokens':>8}  {'status'}")
    print("-" * 25)
    for row in results:
        print(f"{row[0]:>3}  {row[1]:>8}  {row[2]}")
    fails = [r for r in results if r[2] == "FAIL"]
    if fails:
        raise AssertionError(f"{len(fails)} prompts outside token range — see above")
    print("\nAll 10 prompts validated. Dataset ready for S-02.")
```

---

## Instance recommendation for running S-02

S-02 must NOT run on the same instance as S-01/S-04/S-08/S-09. It requires:

- **Instance type:** `m6i.large` minimum (2 vCPU, 8 GB RAM). There is a stopped `m6i.large` instance (`<REDACTED-INSTANCE-ID>`, us-east-1d) in the current AWS account — start and use that one.
- **Why:** gpt-4o-mini processing a 16k token prompt generates a much larger response payload. The streaming SSE chunks are larger and more numerous, which stresses the harness's async event loop more than short-chat. On a t3.medium (2 vCPU, 4 GB) the Python async loop can lag under this load.
- **VU count:** Use `BENCH_VUS=5` not 20 — 5 concurrent 16k-token requests is already substantial upstream load. 20 VUs at 16k tokens each will hit OpenAI's TPM rate limit.
- **Duration:** 300s (same as other scenarios).
- **BENCH_UNIQUE_PROMPTS=1** — mandatory, same reason as all other scenarios.

---

## Acceptance criteria

Before marking this task complete, run the validation and confirm all of the following:

- [ ] `benchmark/v2/datasets/long_context_v2.json` (or `long_context_v2_padded.json`) exists and is readable
- [ ] `data["count"] == 10` — exactly 10 prompts
- [ ] Every prompt passes `14000 <= len(prompt) // 4 <= 17000`
- [ ] Every original `[REQUEST-uuid]` prefix is preserved in its prompt
- [ ] The original instruction is repeated at the end of each prompt (after `--- END CONTEXT ---`)
- [ ] `s02_long_context.py` loads the padded file without modification (or is updated to load the new filename — one or the other, documented in a code comment)
- [ ] `pad_long_context_dataset.py` runs cleanly with `python3 pad_long_context_dataset.py` from `benchmark/v2/`
- [ ] No new production dependencies added to `requirements.txt`
- [ ] Validation function prints "All 10 prompts validated. Dataset ready for S-02."

---

## Self-audit before done (2 rounds)

**Round 1:**
- Q1: Are all 10 prompts complete and within token range?
- Q2: Are there any `TODO`, `FIXME`, `stub`, or placeholder strings in the padding content?
- Q3: Does the padded file load correctly when imported by `s02_long_context.py`?
- Q4: Is the padding semantically relevant (not lorem ipsum or repeated garbage)?

Fix any issues found. Then run Round 2 with the same questions. Only report done when both rounds are clean.
