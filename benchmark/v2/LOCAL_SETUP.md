# Benchmark v2 — Local Development Setup

This guide explains how to run the benchmark suite locally without AWS or production credentials.

---

## Required Env Vars

These are read from `benchmark/v2/.env.local` (auto-loaded at startup) or from exported shell vars (shell vars take precedence).

| Variable | Where it comes from | Dev value / placeholder |
|---|---|---|
| `NEXUS_BASE_URL` | Nexus AI Gateway listen address | `http://localhost:3050` (default in config) |
| `NEXUS_ADMIN_URL` | Nexus Control Plane listen address | `http://localhost:3001` (default in config) |
| `NEXUS_API_KEY` | Virtual Key created via CP UI or admin API | **BLOCKER — see "Getting Keys Locally" below** |
| `NEXUS_ADMIN_API_KEY` | Admin API Key from CP UI (Settings -> API Keys) | **BLOCKER — see "Getting Keys Locally" below** |
| `LITELLM_BASE_URL` | LiteLLM listen address | `http://localhost:4000` (default in config) |
| `LITELLM_API_KEY` | LiteLLM master key or any accepted key | Set to `LITELLM_MASTER_KEY` value from your LiteLLM config |
| `BIFROST_BASE_URL` | Bifrost listen address | `http://localhost:8080` (default in config) |
| `BIFROST_API_KEY` | Bifrost static API key | Any non-empty string when Bifrost auth is disabled |

`NEXUS_BASE_URL`, `NEXUS_ADMIN_URL`, `LITELLM_BASE_URL`, and `BIFROST_BASE_URL` have built-in defaults in the YAML configs and do not need to be set for standard local dev.

---

## Getting Keys Locally

### NEXUS_API_KEY (Virtual Key)

The seed (`npx prisma db seed`) inserts Virtual Key rows but only stores their **hashed** values — the plaintext is never in the repo. You must create a new key after the Nexus stack is running.

**Option A — CP UI:**
1. Open `http://localhost:3000` and log in (`admin@nexus.ai` / `admin123` on a seeded local DB).
2. Navigate to Gateway -> Virtual Keys -> New Key.
3. Copy the plaintext `nvk_...` key shown once on creation.
4. Paste it as `NEXUS_API_KEY` in `benchmark/v2/.env.local`.

**Option B — Admin API (requires `NEXUS_ADMIN_API_KEY` first):**
```bash
curl -s -X POST http://localhost:3001/api/admin/virtual-keys \
  -H "Authorization: Bearer <your-nxk-admin-key>" \
  -H "Content-Type: application/json" \
  -d '{"name":"benchmark-dev","type":"application"}'
```
The response body contains the plaintext key.

### NEXUS_ADMIN_API_KEY (Admin API Key)

The seed inserts a `local-dev-super-admin` key (`prefix: nxk_01234567`) but again only its hash — the plaintext was never committed.

**Generate a fresh key:**
1. Log in to CP UI (`http://localhost:3000`).
2. Navigate to Settings -> API Keys -> Generate New Key.
3. Copy the plaintext `nxk_...` key (shown once).
4. Paste it as `NEXUS_ADMIN_API_KEY` in `benchmark/v2/.env.local`.

Alternatively, use the `cp_login` / `cp_curl` helpers from `tests/lib/auth.sh`:
```bash
source tests/lib/auth.sh
cp_login   # prompts for NEXUS_ADMIN_EMAIL / NEXUS_ADMIN_PASSWORD
cp_curl POST /api/user/api-keys '{"name":"benchmark-admin"}'
```

### LITELLM_API_KEY

Use whatever value you passed as `--master_key` (or `LITELLM_MASTER_KEY` env var) when starting LiteLLM. If you started LiteLLM without auth, set this to any non-empty string — the header will be sent but not validated.

### BIFROST_API_KEY

Bifrost accepts a static key from its own config. For a local Bifrost instance started with auth disabled, any non-empty string works. Check your Bifrost startup config for the expected value.

---

## Service URLs and Health Checks

| Service | Default URL | Health endpoint | Curl check |
|---|---|---|---|
| Nexus AI Gateway | `http://localhost:3050` | `GET /healthz` | `curl -sf http://localhost:3050/healthz` |
| Nexus Control Plane | `http://localhost:3001` | `GET /healthz` (or `GET /api/health`) | `curl -sf http://localhost:3001/healthz` |
| LiteLLM | `http://localhost:4000` | `GET /health` | `curl -sf http://localhost:4000/health` |
| Bifrost | `http://localhost:8080` | `GET /health` | `curl -sf http://localhost:8080/health` |

Verify all three gateway health checks pass before running the benchmark.

---

## Starting Services Locally

### Step 1 — Infrastructure (Docker)

The repo's `docker-compose.yml` starts Postgres, Valkey (Redis-compatible), and NATS. It does **not** start any Go service or the benchmark gateways.

```bash
cd /path/to/nexus-gateway
docker compose up -d postgres valkey nats
```

Wait until all three containers are healthy:
```bash
docker compose ps
```

### Step 2 — Database

```bash
cd tools/db-migrate
cp ../../.env.example ../../.env   # if not done yet — fill in CREDENTIAL_ENCRYPTION_KEY etc.
npx prisma migrate dev
npm run seed
```

The seed creates demo Virtual Keys, AdminApiKeys, providers, and routing rules. The key hashes are stored; you still need to generate fresh plaintext keys (see "Getting Keys Locally" above).

### Step 3 — Nexus Go Services

Open four terminal tabs:

```bash
# Hub (port 3060)
cd packages/nexus-hub && go run ./cmd/nexus-hub/

# Control Plane (port 3001)
cd packages/control-plane && go run ./cmd/control-plane/

# AI Gateway (port 3050)
cd packages/ai-gateway && go run ./cmd/ai-gateway/

# Compliance Proxy (port 3040) — only needed for S-09 compliance scenario
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/
```

Or use the bootstrap script:
```bash
./scripts/dev-start.sh
```

### Step 4 — LiteLLM (external — not in this repo)

Install and run LiteLLM separately. Minimum working config:

```bash
pip install litellm[proxy]
litellm --model openai/gpt-4o-mini --port 4000 --master_key sk-local-dev
```

Set `LITELLM_API_KEY=sk-local-dev` in `benchmark/v2/.env.local`.

### Step 5 — Bifrost (external — not in this repo)

Install and run Bifrost separately. See [Bifrost documentation](https://github.com/maximhq/bifrost) for startup instructions. The benchmark expects OpenAI-compatible `/v1/chat/completions` on port 8080.

---

## Configuring Nexus to Route gpt-4o-mini

Before the benchmark can succeed against Nexus, a routing rule for `gpt-4o-mini` must exist pointing to an OpenAI credential with a real API key. This is the single upstream dependency that requires a real OpenAI key.

**Steps:**
1. In CP UI, go to Gateway -> Credentials -> Add Credential (provider: openai, paste your OpenAI API key).
2. Go to Gateway -> Models -> Add Model (name: `gpt-4o-mini`, provider: openai).
3. Go to Gateway -> Routing Rules -> Add Rule (model: `gpt-4o-mini`, credential: the one you just created).
4. Ensure the Virtual Key you created has `gpt-4o-mini` in its allowed models list.

---

## Running the Benchmark

### Fill in `.env.local` first

```bash
cp benchmark/v2/.env.local.example benchmark/v2/.env.local   # if template is available
# Then edit benchmark/v2/.env.local with your actual key values
```

### Single scenario (fastest check):

```bash
cd benchmark/v2
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

python cli.py run --scenario s01 --gateway nexus --mode cache-disabled
```

### Full three-gateway comparison suite:

```bash
cd benchmark/v2
source .venv/bin/activate

python cli.py run-suite \
  --mode cache-disabled \
  --gateways nexus,litellm,bifrost \
  --scenarios s01,s02,s03,s04,s06 \
  --output ./results/run-local-001/
```

### One-command full suite (includes cache feature test + markdown report):

```bash
cd benchmark/v2
source .venv/bin/activate
./run_full_suite.sh ./results/run-local-001/
```

### Model used

All runs default to `gpt-4o-mini` (set in `config/global.yaml` and as the CLI default). Override per-run with `--model gpt-4o` if needed.

---

## Known Blockers

| # | Blocker | Needs |
|---|---|---|
| 1 | `NEXUS_API_KEY` (Virtual Key plaintext) | Must be generated after local Nexus stack is running. Seed stores only the hash. CP UI or admin API required. |
| 2 | `NEXUS_ADMIN_API_KEY` (Admin API Key plaintext) | Same as above — generate via CP UI -> Settings -> API Keys after local stack is up. |
| 3 | OpenAI API key in Nexus credential | A real OpenAI API key must be added to the Nexus credential store so the AI Gateway can route `gpt-4o-mini` upstream. This is a real upstream cost. |
| 4 | LiteLLM not in repo | LiteLLM must be installed and started separately. It also needs an OpenAI API key in its config to forward requests upstream. |
| 5 | Bifrost not in repo | Bifrost must be installed and started separately. Also needs an upstream provider key. |
| 6 | `NEXUS_ADMIN_URL` needed only for S-10 | The config_validator (S-10 / pre-flight parity check) calls the Nexus admin API. If `NEXUS_ADMIN_API_KEY` is not set, the parity check will fail. For comparison-only runs (S-01 through S-09 excluding S-10), this blocker can be deferred — skip S-10 with `--scenarios s01,s02,s03,s04,s06`. |

---

## Env Var Loading Order

`engine/models.py` loads `benchmark/v2/.env.local` automatically via `python-dotenv` with `override=False`. This means:

1. If you have already exported a var in your shell (`export NEXUS_API_KEY=...`), the shell value wins.
2. Otherwise the value from `.env.local` is used.
3. If neither is set, the YAML default (e.g. `http://localhost:3050` for URLs) is used.
4. If no default and the var is unset, the raw `${VAR_NAME}` string is passed — the benchmark will fail with an auth error or connection refused, not a silent misconfiguration.
