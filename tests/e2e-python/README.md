# tests/e2e-python

Phase 4 (AI-judge) and Phase 5 (protocol compatibility) tests for the Nexus
Gateway test program. See `docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md` for the full plan.

## Setup

```bash
# Install uv if you don't have it: curl -LsSf https://astral.sh/uv/install.sh | sh
cd tests/e2e-python
uv sync                  # creates .venv and installs pinned deps
```

The tests source configuration from `tests/.env.test` (the same file the
shell smoke scripts use). At minimum you need:

```
NEXUS_TEST_VK=nvk_...                 # Nexus VK with access to Kimi 128k
NEXUS_AI_GW_URL=http://localhost:3050 # Local AI Gateway
NEXUS_JUDGE_MODEL=moonshot-v1-128k    # Default oracle model
```

If `NEXUS_TEST_VK` is missing or still set to `nvk_REPLACE_ME`, the tests
auto-skip with a clear message.

## Layout

| Directory | Purpose | Phase |
|-----------|---------|-------|
| `ai_judge/` | Tests that consult Kimi 128k via Nexus VK as an oracle | 4 |
| `protocol/` | Drop-in compatibility tests for `openai` / `anthropic` SDKs | 5 |
| `conftest.py` | Shared fixtures: `nexus_env`, `nexus_judge`, `nexus_db` | — |

## Running

```bash
cd tests/e2e-python

# All AI-judge tests.
uv run pytest ai_judge/

# A specific judge case with verbose output (you'll see Kimi's reasoning).
uv run pytest -v ai_judge/test_pii_detection.py

# Skip judge tests (Phase 5 protocol-compat only).
uv run pytest -m "not ai_judge"
```

## Verification discipline (master plan §6)

Every AI-judge test MUST verify reality through at least one of:
- HTTP response assertion against the gateway
- DB cross-check on `traffic_event`
- Prometheus counter delta
- AI-judge verdict (the oracle path)

`test_pii_detection.py` is the canonical example: it does HTTP assertion +
DB cross-check FIRST (cheap, deterministic), then asks the judge whether
the gateway's behaviour was *appropriate* given the prompt. This separation
keeps the cheap checks fast and stops a flaky judge from masking a
genuine 200/403 regression.

## Cost note

Each AI-judge call is one Kimi 128k completion through our own VK
(dogfooding). At ~500 tokens per judge call, the full Phase 4 suite is
under $0.10 per run on the current Moonshot pricing. Budget is fine for
local dev; revisit if Phase 4 grows past ~50 cases.
