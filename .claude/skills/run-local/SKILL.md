---
name: run-local
description: >
  Bring the full Nexus Gateway local stack up from a clean clone — PostgreSQL +
  Valkey + NATS via docker-compose, four Go services (Hub / Control Plane /
  AI Gateway / Compliance Proxy), and the Control Plane UI (Vite). Encodes
  every gotcha a first-time OSS contributor hits so the model can drive the
  full bring-up without asking the user mid-way. Trigger keywords: run local,
  start local stack, local bring-up, local dev start, first-run, OSS first
  run, bootstrap stack, /run-local.
user-invocable: true
---

# Run Local

Goal: take a fresh clone (or a dirty workspace) of Nexus Gateway and end with **5 services listening** on the documented ports, ready for an admin login at `http://localhost:3000`. Every step has a guard so the model can self-heal rather than hand back to the user.

This skill is the user-facing complement to `scripts/dev-start.sh`: the script handles the Docker / DB / .env bootstrap; this skill drives the rest (start the four Go services with the right flags, verify each one, surface the few errors that need user input).

## Goal first, plan second (binding pattern)

Before any tool call, restate to the user in one line: "Goal: bring up the 5 local services. Plan: bootstrap → start Hub → CP → AI GW → Proxy → UI, verify each port, smoke health." If the user adds a constraint mid-stream (different branch / dirty DB / wants to keep current data), re-anchor the goal and continue.

## Required ports (free these before starting)

| Port | Service | Listen evidence on macOS lsof |
|---|---|---|
| 3000 | Control Plane UI (Vite) | `node … TCP *:hbci (LISTEN)` |
| 3001 | Control Plane (Echo) | `control-p … TCP *:redwood-broker (LISTEN)` |
| 3040 | Compliance Proxy | `complianc … TCP localhost:tomato-springs (LISTEN)` |
| 3050 | AI Gateway | `ai-gatewa … TCP *:gds_db (LISTEN)` |
| 3060 | Nexus Hub | `nexus-hub … TCP *:interserver (LISTEN)` |
| 55532 | PostgreSQL (Docker) | `com.docker … LISTEN` |
| 6437 | Valkey (Docker) | `com.docker … LISTEN` |
| 4222 / 8222 | NATS (Docker) | `com.docker … LISTEN` |

macOS lsof prints IANA service names (`hbci`, `redwood-broker`, …) instead of port numbers — they are the same port, no aliasing.

## Step 0 — Parallel-instance check (must run first)

A common first-run failure: the user has another local checkout running services that share this Docker Postgres. Symptoms: `metric_ops_raw` table fills with rows referencing thing IDs that don't exist (FK violation on `prisma db push`).

```bash
ps aux | grep -E "nexus-hub|control-plane|ai-gateway|compliance-proxy" | grep -v grep
lsof -i :3001 -i :3040 -i :3050 -i :3060 2>/dev/null | grep LISTEN
```

If any process from a sibling checkout (e.g. `nexus-gateway-refactor`) is listed, surface this to the user and ask whether to kill it:

```bash
kill <PID1> <PID2> ...      # SIGTERM first
sleep 2
kill -9 <PID>               # only the stubborn ones (debuggers)
```

Never kill processes under another directory without the user's go-ahead — they may be intentional debug sessions.

## Step 1 — One-shot bootstrap

```bash
./scripts/dev-start.sh --no-dev
```

`dev-start.sh` does everything Docker / DB / `.env` related. It is idempotent — safe to re-run. With `--no-dev` it stops after seeding so we can start the Go services explicitly (skill-driven), which gives us per-service log files and clean kill semantics.

The script:

- Checks Node 20+, Go 1.25+, Docker, OpenSSL.
- Creates the repo-root `.env` from `.env.example` with safe dev defaults (substituting every `CHANGE_ME_*`). `INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY` (`openssl rand -hex 32`), `COMPLIANCE_PROXY_API_TOKEN`, `AI_GATEWAY_API_TOKEN`.
- Brings up `docker compose` (PostgreSQL + Valkey + NATS) and waits for each to be ready (`pg_isready`, `valkey-cli ping`, `/healthz` on NATS).
- Runs `npm install`.
- Creates `tools/db-migrate/.env` from its example AND propagates `CREDENTIAL_ENCRYPTION_KEY` from the repo-root `.env` (seed.ts otherwise aborts with "must be a 64-char hex string").
- Runs `npx prisma db push` then `npx prisma db seed`.
- Generates `packages/compliance-proxy/dev-certs/{ca.crt,ca.key}` if missing (TLS-bump cert issuer needs them).
- Prints the per-service `go run … -config <svc>.dev.yaml` commands.

If the script fails, surface the EXACT error line to the user and propose a fix; do not silently retry the same failure.

### Reset semantics

If the user wants a clean slate ("reset", "destroy state", "redo from scratch"):

```bash
docker compose down -v     # -v wipes the named volumes (Postgres + NATS data)
rm -f .env tools/db-migrate/.env packages/compliance-proxy/dev-certs/ca.{crt,key}
./scripts/dev-start.sh --no-dev
```

`docker compose down -v` is binding — without `-v` the volumes survive and the next `db push` runs against stale data. The user has stated explicitly: "reset 的时候 docker 要把 volume 也清掉".

## Step 2 — Start the four Go services in background

Each service tees its log to `/tmp/nexus-dev-logs/<svc>.log` so the skill can read it back without grepping the foreground. The `-config <svc>.dev.yaml` flag is mandatory — the default flag value points at `<svc>.config.yaml`, which is the deployment-shape template and lacks dev-only fields like `hub.id`.

```bash
mkdir -p /tmp/nexus-dev-logs

cd packages/nexus-hub
nohup go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml \
  > /tmp/nexus-dev-logs/hub.log 2>&1 &
HUB_PID=$!

cd ../control-plane
nohup go run ./cmd/control-plane/ -config control-plane.dev.yaml \
  > /tmp/nexus-dev-logs/cp.log 2>&1 &
CP_PID=$!

cd ../ai-gateway
nohup go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml \
  > /tmp/nexus-dev-logs/aigw.log 2>&1 &
AIGW_PID=$!

cd ../compliance-proxy
nohup go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml \
  > /tmp/nexus-dev-logs/proxy.log 2>&1 &
PROXY_PID=$!

cd ../..
sleep 15        # first `go run` compiles + warms — give it space
```

The first invocation of each `go run` compiles the binary; expect 10-30 s per service on a cold machine. After that, restarts are sub-second.

### Verification

```bash
lsof -i :3060 -i :3001 -i :3050 -i :3040 2>/dev/null | grep LISTEN
```

Expect 4 LISTEN rows. If a port is missing, check the log:

```bash
grep -iE "level=(error|fatal)|panic" /tmp/nexus-dev-logs/<svc>.log | tail -20
```

Common failures the skill should recognise and act on without asking:

| Log line | Cause | Fix |
|---|---|---|
| `validate config: hub.id is required` | Missing `-config nexus-hub.dev.yaml` (used default deployment yaml). | Add the flag. |
| `cert issuer: cert: read CA cert ./dev-certs/ca.crt: open … no such file` | dev CA not generated. | Run `(cd packages/compliance-proxy && make dev-certs)`. |
| `hubclient: INTERNAL_SERVICE_TOKEN is not set` | Repo-root `.env` missing. | Re-run `./scripts/dev-start.sh --no-dev` (it idempotently bootstraps `.env`). |
| `seed: CREDENTIAL_ENCRYPTION_KEY must be a 64-char hex string` | `tools/db-migrate/.env` lacks the key. | Re-run dev-start.sh — it now mirrors the value from repo-root `.env`. |

### Thing-registry cross-check (deeper smoke than `lsof`)

After ~10 s the four binaries register as Things on the Hub:

```bash
docker exec nexus-postgres psql -U postgres -d nexus_gateway \
  -c "SELECT type, status FROM thing WHERE status='online' GROUP BY type, status ORDER BY type"
```

Expect rows for `ai-gateway`, `compliance-proxy`, `control-plane`, `nexus-hub`. (The Hub itself sometimes shows as Thing rows for older sessions; one current `online` row is enough.)

## Step 3 — Start the Control Plane UI

```bash
nohup npm run dev:control-plane-ui > /tmp/nexus-dev-logs/ui.log 2>&1 &
UI_PID=$!
sleep 15
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:3000/
# Expect 200
```

Vite prints a `VITE vN.M.K ready in <ms>` line on success; if the log instead carries `EADDRINUSE` or `Cannot find module`, surface the error.

## Step 4 — Smoke the login surface (optional but high-value)

The Control Plane uses OAuth + PKCE. The simplest sanity check is to load the UI's root path (already done above) and confirm the seeded super-admin credentials are mentioned by the seed.

If you want a curl-level confirmation that the auth server is responsive:

```bash
# 400 here is correct — the body lacks authctx; the endpoint exists and is reachable.
curl -s -X POST http://localhost:3001/authserver/password \
  -H "Content-Type: application/json" \
  -d '{"authctx":"","email":"admin@nexus.ai","password":"admin123"}' \
  -o /dev/null -w "%{http_code}\n"
```

For a real end-to-end login, source the local test env contract:

```bash
cp tests/.env.local.example tests/.env.local
source tests/lib/loadenv.sh local
source tests/lib/auth.sh
cp_login                                       # cached at /tmp/nexus_test_token_local
cp_curl /api/admin/me                          # confirms session
```

## Stopping the stack cleanly

```bash
# Foreground PIDs the skill stored above (Hub last so dependents have time to deregister):
kill $UI_PID $CP_PID $PROXY_PID $AIGW_PID $HUB_PID 2>/dev/null

# If anything is stubborn (debug servers, runaway test loops):
lsof -i :3000 -i :3001 -i :3040 -i :3050 -i :3060 2>/dev/null | awk '/LISTEN/ {print $2}' | sort -u | xargs -r kill -9

# Docker stays up — leave it for fast restarts. To wipe everything:
docker compose down -v
```

## What this skill does NOT do

- It does NOT run `git pull` / `git checkout`. The branch state is the user's responsibility.
- It does NOT mutate `.env` files beyond what `dev-start.sh` already does. Manual secret overrides (e.g. a real provider API key for `examples/01-hello-world/`) stay in the user's hands.
- It does NOT run the smoke / scenario test suites. Those are separate skills (`/smoke-gateway`, `/test-all`).
- It does NOT install Docker / Go / Node. Prerequisite checks fail loud — the user installs them.

## Sub-agent dispatch policy (when relevant)

If the bring-up surfaces a mass refactor (e.g. removing a deprecated config field that ripples across 30 test files), delegate via the Agent tool with these guardrails repeated at the TOP of the prompt:

- **Never `git stash`** (any form) — sweeps up parallel-session work, recovery not guaranteed.
- **Never `git add -A` / `git add .`** — use explicit pathspecs.
- **Never commit** — only edit.
- Report back: list of files touched, tests deleted (with names), verification command outputs.

These are CLAUDE.md bindings; the skill restates them so the sub-agent doesn't need to discover them.
