# Handoff: Benchmark v2 — API Keys Setup

**Date**: 2026-06-14  
**Session goal**: Save context and transition to API keys configuration for benchmark v2.

---

## What Was Built This Session

### Benchmark v2 (`benchmark/v2/`)

A full Python-based benchmarking framework replacing the invalid k6 v1 benchmark. Complete file inventory:

| Layer | Files | Status |
|---|---|---|
| Config (YAML + Pydantic) | `config/nexus.yaml`, `litellm.yaml`, `bifrost.yaml`, `global.yaml`, `engine/models.py` | Done |
| Core engine | `engine/runner.py` (async SSE + httpx), `engine/metrics.py` (numpy), `engine/config_validator.py` | Done |
| Gateway adapters | `gateway_adapters/base.py`, `nexus.py`, `litellm.py`, `bifrost.py` | Done |
| Scenarios | `scenarios/s01`–`s11` — all 11 independently runnable | Done |
| Datasets | 55 short prompts, 10 long, 15 streaming, 50+50 PII, cache exact/prefix | Done |
| Reporting | `reporting/terminal.py` (Rich), `json_report.py`, `csv_report.py`, `markdown_report.py` | Done |
| CLI | `cli.py` (typer), `run_full_suite.sh` | Done |
| README | Full setup + methodology + v3 roadmap | Done |

### Local Setup Files (from last task)

- `benchmark/v2/.env.local` — env vars with localhost URLs pre-filled; Nexus keys marked as BLOCKERS
- `benchmark/v2/engine/models.py` — fixed `${VAR}` env resolution bug; auto-loads `.env.local`
- `benchmark/v2/config/global.yaml` — model standardized to `gpt-4o-mini`
- `benchmark/v2/LOCAL_SETUP.md` — full local setup doc
- `.gitignore` — `.env.local` excluded

---

## Why v1 Was Invalid (do not cite v1 numbers)

1. Nexus had caching enabled (44.16% hit rate); Bifrost/LiteLLM had 0%. TTFT p95 of 4ms = cache hit, not model call.
2. Load generator ran from a MacBook Pro (local), not from within AWS. Uncontrolled network jitter.
3. No warmup phase documented.
4. No config parity validation.

---

## Current State of `.env.local`

```
NEXUS_BASE_URL=http://localhost:3050
NEXUS_API_KEY=BLOCKER_NEEDS_LOCAL_VIRTUAL_KEY
NEXUS_ADMIN_API_KEY=BLOCKER_NEEDS_ADMIN_KEY

LITELLM_BASE_URL=http://localhost:4000
LITELLM_API_KEY=dev-test-key-litellm

BIFROST_BASE_URL=http://localhost:8080
BIFROST_API_KEY=dev-test-key-bifrost

OPENAI_API_KEY=BLOCKER_NEEDS_REAL_KEY

BENCHMARK_MODEL=gpt-4o-mini
CACHE_MODE=disabled
```

---

## Hard Blockers for Next Task

| Blocker | What's needed | How to resolve |
|---|---|---|
| `NEXUS_API_KEY` | A virtual key from the local Nexus instance | Start local Nexus stack → CP UI → Gateway → Virtual Keys → New Key |
| `NEXUS_ADMIN_API_KEY` | An admin API key from local Nexus | CP UI → Settings → API Keys |
| `OPENAI_API_KEY` | Real OpenAI key to make actual model calls | Get from OpenAI dashboard or ask James |
| LiteLLM running locally | LiteLLM is not in this repo | Start separately: `litellm --model gpt-4o-mini --port 4000` |
| Bifrost running locally | Bifrost is not in this repo | Start separately per Bifrost docs on port 8080 |

---

## Next Task: API Keys Setup

### Goal

Get all three gateways running locally with valid dev/test keys so `python cli.py validate-config` passes and `./run_full_suite.sh` can execute S-01 (short chat, cache disabled).

### Steps

1. **Start local Nexus stack** — `./scripts/dev-start.sh` from repo root, or `cd packages/control-plane && go run ./cmd/control-plane/` etc.
2. **Create Nexus virtual key** — CP UI at `http://localhost:3000` → Gateway → Virtual Keys → New Key → copy value → paste into `.env.local` as `NEXUS_API_KEY`.
3. **Create Nexus admin key** — CP UI → Settings → API Keys → copy value → paste into `.env.local` as `NEXUS_ADMIN_API_KEY`.
4. **Start LiteLLM** — `pip install litellm && litellm --model gpt-4o-mini --port 4000` (needs `OPENAI_API_KEY` in env).
5. **Start Bifrost** — follow Bifrost docs; point to port 8080.
6. **Set OpenAI key** — add `OPENAI_API_KEY=sk-...` to `.env.local`.
7. **Run health checks** — see `benchmark/v2/LOCAL_SETUP.md` for exact curl commands.
8. **Validate config parity** — `cd benchmark/v2 && python cli.py validate-config --mode cache-disabled`.
9. **Run S-01** — `python cli.py run --scenario s01 --gateway nexus --mode cache-disabled`.

### Key Files to Reference

- `benchmark/v2/LOCAL_SETUP.md` — all health check commands and URLs
- `benchmark/v2/.env.local` — env vars (git-ignored)
- `benchmark/v2/config/nexus.yaml` — gateway config shape
- `packages/control-plane/` — where to find admin key creation endpoints
- `scripts/dev-start.sh` — local stack bootstrap

---

## Onboarding Context (Kaushik — new FTE, started 2026-06-14)

- Kaushik starts Monday. First deliverable: scan repo, document API key/config mismatches in a markdown file.
- Daily dev sync: **6:30 PM PST** (except Saturday) — Chinese dev team.
- Weekly 1:1 with Kash: **9 AM PST Mondays**.
- Primary Slack: iTech Hub channel.
- James is in Europe; Kash is steering Nexus gateway work.
- Contacts for Kaushik: Kao Mao, GK.
- Tools to set up: Granola (use `.edu` email for free tier), Claude Cowork, Slack (iTech Hub).

---

## Memory Anchors

- `[[project_parallel_worktree_sessions]]` — each Claude Code session needs its own worktree
- `[[benchmark_v2_local_setup]]` — benchmark/v2 is the canonical framework; v1 is invalid
- `[[feedback_cache_mandatory_all_ingress]]` — cache mode must be explicit and matched across all gateways in comparative tests
