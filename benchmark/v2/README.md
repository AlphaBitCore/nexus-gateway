# Nexus Gateway Benchmark v2

Production-grade Python benchmarking framework for LLM API gateways.  
Compares **Nexus Gateway**, **LiteLLM**, and **Bifrost** with full SSE streaming support, TTFT measurement, and methodologically valid fair comparison.

> **This supersedes Minimal Benchmark v1 (2026-05-19).**  
> v1 was invalid: Nexus had caching ON (44% hit rate), load ran from a MacBook, no warmup. See methodology note below.

---

## Quick Start

```bash
cd benchmark/v2
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

# Set env vars
export NEXUS_API_KEY="your-key"
export LITELLM_API_KEY="your-key"
export BIFROST_API_KEY="your-key"
export NEXUS_BASE_URL="http://<aws-ip>:3050"
export LITELLM_BASE_URL="http://<aws-ip>:4000"
export BIFROST_BASE_URL="http://<aws-ip>:8080"

# Run the full suite
./run_full_suite.sh ./results/run-001
```

---

## Running Individual Scenarios

```bash
# S-01: Short chat fair comparison — Nexus
python cli.py run --scenario s01 --gateway nexus --mode cache-disabled

# S-02: Long context
python cli.py run --scenario s02 --gateway litellm --mode cache-disabled

# S-03: Streaming stress test
python cli.py run --scenario s03 --gateway bifrost --mode cache-disabled

# S-04: Concurrency sweep (VU levels 1,5,10,20,50,100)
python cli.py run --scenario s04 --gateway nexus --mode cache-disabled

# S-05: 30-minute soak test
python cli.py run --scenario s05 --gateway nexus --mode cache-disabled

# S-06: Consistency / flakiness (100 identical requests, 1 VU)
python cli.py run --scenario s06 --gateway litellm --mode cache-disabled

# S-07: Gateway overhead isolation (max_tokens=1)
python cli.py run --scenario s07 --gateway nexus --mode cache-disabled

# S-08: Nexus cache feature test (NOT a head-to-head)
python cli.py run --scenario s08 --gateway nexus --mode cache-enabled

# S-09: PII / compliance enforcement (Nexus only)
python cli.py run --scenario s09 --gateway nexus

# S-10: Pre-flight config parity check only
python cli.py validate-config --mode cache-disabled

# S-11: Provider failover simulation (Nexus only)
python cli.py run --scenario s11 --gateway nexus
```

## Running the Full Suite

```bash
# All comparison scenarios, all gateways, cache DISABLED
python cli.py run-suite \
  --mode cache-disabled \
  --gateways nexus,litellm,bifrost \
  --scenarios s01,s02,s03,s04,s06 \
  --output ./results/run-001/

# Or use the shell script (includes cache feature test + report generation)
./run_full_suite.sh ./results/run-001/
```

## Generating Reports

```bash
# Markdown comparison report from existing results
python cli.py report --results-dir ./results/run-001/ --format markdown

# Terminal tables
python cli.py report --results-dir ./results/run-001/ --format terminal
```

---

## Scenarios Reference

| ID | Name | VUs | Duration | Cache Mode | Gateways |
|----|------|-----|----------|------------|----------|
| S-01 | Short Chat | 20 | 5 min | Disabled | All 3 |
| S-02 | Long Context | 10 | 5 min | Disabled | All 3 |
| S-03 | Streaming Stress | 30 | 5 min | Disabled | All 3 |
| S-04 | Concurrency Sweep | 1→100 | 2 min/level | Disabled | All 3 |
| S-05 | Soak / Stability | 20 | 30 min | Disabled | All 3 |
| S-06 | Flakiness / Consistency | 1 | 100 requests | Disabled | All 3 |
| S-07 | Overhead Isolation | 5 | 2 min | Disabled | All 3 |
| S-08 | Cache Feature | 10 | 2 min × 3 | **Enabled** | Nexus only |
| S-09 | PII Compliance | 1 | 100 requests | N/A | Nexus only |
| S-10 | Config Parity | — | Pre-flight | — | All 3 |
| S-11 | Provider Failover | 5 | 90 sec | Disabled | Nexus only |

---

## Metrics Captured

For each scenario + gateway:

```
Total Requests       HTTP 4xx / 5xx       Stream Broken count
Successful           Connection Timeouts  Cache Hit Rate
Failed               Stream Timeouts      Cache Hit TTFT p95

TTFT avg / p50 / p95 / p99 / stddev
E2E  avg / p50 / p95 / p99
Throughput (RPS)
HTTP Failure Rate %
Stream Broken Rate %
TTFT Gain p95 (cache miss − cache hit)
```

---

## Interpreting Results

- **TTFT p95 < 1500ms** — threshold for S-01 (short chat)
- **HTTP failure < 1%** — any gateway above this has reliability issues
- **Stream broken < 0.5%** — LiteLLM v1 showed 28.63%; S-06 characterizes this at low concurrency
- **TTFT stddev** — high stddev relative to avg indicates instability or jitter
- **Concurrency sweep (S-04)** — watch for the "knee" where p95 starts climbing steeply; that's the saturation point
- **Soak (S-05)** — p95 increasing >20% over 30 min signals a resource leak or degradation

---

## Why This Methodology Is Valid

### What was wrong with v1
1. Nexus had semantic caching ON → 44.16% cache hit rate → 4ms TTFT p95 (physically impossible without cache serving)
2. Load generator ran on a MacBook, not co-located with the AWS gateways → uncontrolled network jitter
3. No warmup phase → cold-start artifacts included in results
4. No config parity validation → no proof gateways were configured equivalently

### What v2 does differently
- **Caching explicitly OFF** in all comparison scenarios (Mode A). Config parity validated before every run via S-10.
- **Load generator runs from the same AWS environment** as the gateways. No laptop-to-AWS hops.
- **Warmup phase** runs for 30–60s before every timed window. Warmup data is excluded from metrics.
- **Full SSE parsing** with `httpx_sse` — TTFT measured at first content token, E2E at `[DONE]` signal, stream-broken when connection drops before `[DONE]`.
- **numpy percentiles** — no averages-only reporting.
- **Nexus cache tested separately** (S-08) as a product feature, clearly labeled, not included in head-to-head tables.
- **Environment captured per run** — hostname, OS, git commit, config fingerprint, Python version.

---

## Directory Structure

```
benchmark/v2/
├── cli.py                          # CLI entry point (typer)
├── run_full_suite.sh               # One-command full suite runner
├── requirements.txt
├── README.md
├── config/
│   ├── global.yaml                 # Global defaults
│   ├── nexus.yaml                  # Nexus Gateway config
│   ├── litellm.yaml                # LiteLLM config
│   └── bifrost.yaml                # Bifrost config
├── datasets/
│   ├── short_chat_v2.json          # 55 unique short-chat prompts
│   ├── long_context_v2.json        # 10 UUID-prefixed long prompts
│   ├── streaming_v2.json           # 15 long-form streaming prompts
│   ├── cache_exact_v2.json         # 5 exact-match cache test prompts
│   ├── cache_prefix_v2.json        # 10 prefix-cache test entries
│   └── compliance_pii_v2.json      # 50 clean + 50 fake-PII prompts
├── gateway_adapters/
│   ├── base.py                     # Abstract BaseGatewayAdapter
│   ├── nexus.py                    # Nexus adapter
│   ├── litellm.py                  # LiteLLM adapter
│   └── bifrost.py                  # Bifrost adapter
├── engine/
│   ├── models.py                   # Pydantic v2 config models
│   ├── metrics.py                  # ScenarioMetrics + RequestRecord
│   ├── runner.py                   # Async SSE execution engine
│   └── config_validator.py        # Pre-flight parity checker
├── scenarios/
│   ├── base_scenario.py            # Shared helpers
│   ├── s01_short_chat.py
│   ├── s02_long_context.py
│   ├── s03_streaming_stress.py
│   ├── s04_concurrency_sweep.py
│   ├── s05_soak_test.py
│   ├── s06_flakiness_consistency.py
│   ├── s07_overhead_isolation.py
│   ├── s08_cache_feature.py
│   ├── s09_compliance_pii.py
│   ├── s10_config_parity.py
│   └── s11_provider_failover.py
└── reporting/
    ├── environment_capture.py
    ├── terminal.py                 # Rich terminal tables
    ├── json_report.py
    ├── csv_report.py
    └── markdown_report.py          # Publishable comparison tables
```

---

## Adding a New Gateway

1. Create `gateway_adapters/mygw.py` subclassing `BaseGatewayAdapter`
2. Implement `_extra_body()` and `from_config()` 
3. Add `config/mygw.yaml`
4. Add to `GATEWAY_MAP` in `cli.py`

That's it — all scenarios and reporting work automatically.

---

## Suggested v3 Improvements

- **Grafana/Prometheus integration** — emit metrics as Prometheus gauge/histogram via `prometheus_client`; visualize in Grafana with pre-built dashboard
- **Automated regression detection** — compare p95 across runs stored in SQLite; alert if regression > 10%
- **Container-based deployment** — `docker-compose.yml` that spins up all three gateways + the mock LLM backend on a single EC2 instance for fully reproducible test isolation
- **GitHub Actions CI** — `.github/workflows/benchmark.yml` that runs S-01 + S-06 on every PR to catch gateway regressions before merge
- **Mock LLM backend** — FastAPI server returning fixed responses in <1ms, enabling pure gateway-overhead isolation without consuming real API quota
- **Token throughput metric** — tokens/second across the stream, not just latency
- **Provider cost reporting** — attach estimated USD cost per 1000 requests at observed token counts
