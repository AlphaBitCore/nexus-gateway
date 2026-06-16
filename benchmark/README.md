# Gateway Benchmark Harness

A dependency-light async harness for a **fair, reproducible** performance
comparison of OpenAI-compatible AI gateways — **Nexus**, **Bifrost**, and
**LiteLLM** — on the same hardware, one gateway at a time.

It measures per request: **TTFT** (time to first token), **end-to-end latency**,
HTTP status, **stream interruption**, request errors, and **cache hit**
(from response headers). It aggregates p50/p95/p99, RPS, and failure/error/cache
rates, and writes `results.json`, `results.csv`, and `summary.md` per run.

> Layout note: this harness lives at the top level of `benchmark/`
> (`run.py`, `preflight.py`, `configs/`, `datasets/`, `harness/`). Two earlier
> harnesses also exist in this folder — `benchmark/src/` (+ `run_benchmark.py`)
> and `benchmark/v2/`. They are independent; this README documents the
> top-level harness only.

---

## Install

```bash
python3 -m venv .venv && source .venv/bin/activate     # optional
pip install -r benchmark/requirements.txt              # httpx + PyYAML
```
Python 3.10+ recommended.

## Environment variables (API keys)

Keys are **never** stored in the config files — each config names an env var:

| Gateway | Env var          | Local value (this repo) |
|---------|------------------|-------------------------|
| Nexus   | `NEXUS_API_KEY`  | a Nexus virtual key     |
| Bifrost | `BIFROST_API_KEY`| `local-dev`             |
| LiteLLM | `LITELLM_API_KEY`| `sk-local-dev`          |

Load them (the local gateways already have these in `benchmark/v2/.env.local`):
```bash
set -a; source benchmark/v2/.env.local; set +a
# or export individually:
export LITELLM_API_KEY=sk-local-dev
export BIFROST_API_KEY=local-dev
export NEXUS_API_KEY=...   # your Nexus virtual key
```

## Configs

`benchmark/configs/{nexus,bifrost,litellm}.yaml`. Each captures: `gateway_name`,
`version`, `base_url`, `api_key_env`, `model`, `provider`, `cache_mode`,
`request_timeout`, `max_tokens`, `stream`. Edit `base_url`/`model` to match your
deployment (defaults target the local Docker gateways: Nexus `:3050`,
LiteLLM `:4000`, Bifrost `:8080`).

## Preflight (config parity)

Before a fair comparison, check the configs agree:
```bash
python benchmark/preflight.py
```
Prints a parity table for `model` (provider prefix normalised), `stream`,
`cache_mode`, `request_timeout`, `max_tokens` and **WARNs** on any divergence.

## Run

```bash
python benchmark/run.py --gateway <nexus|bifrost|litellm|all> \
                        --scenario <name> [--vus N] [--duration S] \
                        [--output-dir DIR] [--warmup N] [--dry-run]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `--gateway`    | (required) | target gateway, or `all` |
| `--scenario`   | (required) | see scenarios below |
| `--vus`        | scenario default | override virtual-user count |
| `--duration`   | scenario default | override duration (seconds) |
| `--output-dir` | `benchmark/results/<scenario>-<ts>` | base output dir |
| `--warmup`     | `3` | warmup requests excluded from metrics |
| `--dry-run`    | off | print config + dataset info, make no requests |

### Scenarios

| Name | Load | Stream | Cache | Dataset | Notes |
|------|------|--------|-------|---------|-------|
| `smoke`             | 1 VU, 5 reqs | ✓ | off | short_chat | prints PASS/FAIL per gateway |
| `w01`               | 20 VUs, 3 min | ✓ | off | short_chat | unique prompt IDs + per-req nonce |
| `w02`               | 10 VUs, 3 min | ✓ | off | long_context | ~16k-token prompts, VU/iter markers |
| `w03a`              | 20 VUs, 3 min | ✓ | off | short_chat | explicit cache-disabled fair run |
| `w03b`              | 10 VUs, 2 min | ✓ | **on** | (built-in) | **Nexus only**: exact/prefix/mixed cache |
| `w04`               | 30 VUs, 3 min | ✓ | off | streaming_stress | long-form streaming stress |
| `concurrency-sweep` | w01 @ [1,5,10,20,50,100] | ✓ | off | short_chat | one CSV row per VU level |

### Examples

```bash
# Smoke test one gateway (PASS/FAIL + output files)
python benchmark/run.py --gateway nexus --scenario smoke

# Fair short-chat comparison across all three (cache disabled)
python benchmark/run.py --gateway all --scenario w03a

# Long-context, 10 VUs, custom 60s
python benchmark/run.py --gateway litellm --scenario w02 --duration 60

# Nexus cache feature characterisation
python benchmark/run.py --gateway nexus --scenario w03b

# Streaming stress
python benchmark/run.py --gateway bifrost --scenario w04

# Concurrency sweep (CSV row per level)
python benchmark/run.py --gateway nexus --scenario concurrency-sweep

# See exactly what would run, without making requests
python benchmark/run.py --gateway all --scenario w01 --dry-run
```

## Outputs

Per run, under the output dir (`<base>/<gateway>/<scenario>/`):

* `results.json` — metadata + aggregate stats + raw per-request records
* `results.csv`  — one row per request (sweep: one row per VU level)
* `summary.md`   — markdown table of aggregate stats

…and a terminal table is printed at the end of each run. All files are written
atomically (temp file + `os.replace`).

### Metrics

Per request: `ttft`, `e2e`, `status_code`, `stream_broken`, `request_error`,
`cache_hit` (+ `cache_detected`, `cache_class`, `completion_tokens`).
Aggregate: p50/p95/p99 for TTFT and E2E, total requests, RPS, HTTP failure %,
request error %, stream interruption %, cache hit % (over requests where a cache
header was observed).

### How metrics are defined

* **TTFT** — time from request send to the first `data:` SSE chunk (streaming),
  or to the full response (non-streaming).
* **E2E** — request send to response fully consumed.
* **stream_broken** — a 200 stream that ends before `data: [DONE]`.
* **cache_hit** — parsed from cache headers (`x-cache`, `x-nexus-cache`,
  `cache-status`, …); a request only counts toward cache-hit rate if such a
  header was present.
* **Fair comparison** — cache-disabled scenarios append a unique nonce to every
  prompt so no accidental cache reuse can occur, regardless of gateway settings.
