# Quickstart

`./scripts/dev-start.sh` takes a fresh clone to a working local stack in approximately 3 minutes on a machine where Docker is already running and the four prerequisites are installed. The script is idempotent — safe to re-run without wiping existing data. This page walks through every step, explains what each one does, and shows how to verify that each service is up before moving on.

---

## Step-by-step bring-up

1. Clone the repository: `git clone https://github.com/AlphaBitCore/nexus-gateway.git && cd nexus-gateway`. On a fresh clone `go.work` and `go.work.sum` are already present at the repo root; do not copy subdirectories into a separate location or the Go workspace resolution breaks.

2. Run the bootstrap script: `./scripts/dev-start.sh`. The script performs every setup action in the correct order and waits for each infrastructure service to be healthy before proceeding. Allow 2–5 minutes on the first run (npm install + first `go run` compile dominate the time). On a warm machine the same command completes in under 30 seconds.

3. When the script finishes it prints the per-service start commands and then launches the Control Plane UI dev server (Vite) in the foreground. Open a second terminal for each Go service.

4. Start Nexus Hub first (the other services register with it on boot): `cd packages/nexus-hub && go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml`. The first `go run` compiles the binary — allow 10–30 s on a cold machine. Subsequent restarts are sub-second.

5. Start the Control Plane: `cd packages/control-plane && go run ./cmd/control-plane/ -config control-plane.dev.yaml`.

6. Start the AI Gateway: `cd packages/ai-gateway && go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml`.

7. Start the Compliance Proxy: `cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml`.

8. If the Control Plane UI did not start automatically (because you ran with `--no-dev`), start it now: `npm run dev:control-plane-ui`.

The `-config <svc>.dev.yaml` flag is **required** for every Go service. Each binary defaults to `<svc>.config.yaml`, which is the production-shape template and intentionally omits dev-only fields such as `hub.id`. Without the flag the service exits immediately with `validate config: hub.id is required`.

## What the bootstrap script does

The script performs nine actions in sequence, each with a guard that surfaces the exact failure if anything goes wrong:

1. Checks that Node 20+, Go 1.25+, Docker, and OpenSSL are present on the path.
2. Creates the repo-root `.env` from `.env.example`, substituting every `CHANGE_ME_*` placeholder with safe dev defaults: `INTERNAL_SERVICE_TOKEN=dev-service-token`, `ADMIN_KEY_HMAC_SECRET=nexus-gateway-default-hmac-secret`, `CREDENTIAL_ENCRYPTION_KEY` = 32 random bytes via `openssl rand -hex 32` (or a fixed 64-hex fallback if OpenSSL is absent). All Go services load `.env` automatically at boot via `packages/shared/core/bootenv` — no `source .env` required.
3. Starts Docker services: `docker compose up -d`, then waits for PostgreSQL (`pg_isready`), Valkey (`valkey-cli ping`), and NATS (HTTP `/healthz` from inside the container) to become healthy.
4. Runs `npm install` at the repo root (npm workspaces install all packages).
5. Creates `tools/db-migrate/.env` from its own `.env.example` and propagates `CREDENTIAL_ENCRYPTION_KEY` from the repo-root `.env` into it so `prisma db seed` can re-encrypt seed provider credentials with the same key the runtime uses.
6. Applies the Prisma schema: `npx prisma db push` (additive — existing rows survive by default).
7. Seeds the database: `npx prisma db seed` (idempotent; creates the seeded admin accounts, routing rules, and virtual key).
8. Generates the Compliance Proxy dev CA: `packages/compliance-proxy/dev-certs/{ca.crt,ca.key}` using `openssl ecparam` + `openssl req`. The TLS-bump cert issuer needs these files; without them the Compliance Proxy aborts on boot.
9. Prints the per-service start commands, then starts the Control Plane UI dev server.

## Port reference

| Service | Port |
|---|---|
| Control Plane UI (Vite) | `:3000` |
| Control Plane | `:3001` |
| Compliance Proxy — HTTPS proxy port | `:3128` |
| Compliance Proxy — runtime API | `:3040` |
| AI Gateway | `:3050` |
| Nexus Hub | `:3060` |
| PostgreSQL (Docker) | `:55532` |
| Valkey (Docker) | `:6437` |
| NATS (Docker) | `:4222` |

## Verifying each service is up

After starting all services, run the health-check probes:

```bash
curl -fsS http://localhost:3060/healthz && echo "Hub OK"
curl -fsS http://localhost:3001/healthz && echo "CP OK"
curl -fsS http://localhost:3050/healthz && echo "AI Gateway OK"
curl -fsS http://localhost:3040/healthz && echo "Compliance Proxy runtime API OK"
curl -fsS http://localhost:3000/         && echo "UI OK"
```

All five should return `OK` or an HTTP 200 response. If a service port is not listening, check its log file (see the section below).

For a deeper check that the four Go services registered with Hub, query the thing registry:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway \
  -c "SELECT type, status FROM thing WHERE status='online' ORDER BY type;"
```

Expect rows for `ai-gateway`, `compliance-proxy`, `control-plane`, and `nexus-hub`.

## Log file locations

Each Go service tees structured logs to disk when started with its `*.dev.yaml`:

| Service | Log file |
|---|---|
| Nexus Hub | `packages/nexus-hub/logs/nexus-hub.log` |
| Control Plane | `packages/control-plane/logs/control-plane.log` |
| AI Gateway | `packages/ai-gateway/logs/ai-gateway.log` |
| Compliance Proxy | `packages/compliance-proxy/logs/compliance-proxy.log` |

Override the path without editing YAML: `LOG_FILE=/path/to/file go run ./cmd/<svc>/ ...`

For debug-level body inspection in the AI Gateway (upstream request/response JSON, raw SSE bytes), set `log.level: "debug"` in `packages/ai-gateway/ai-gateway.dev.yaml` or pass `LOG_LEVEL=debug` in the environment.

## Bootstrap flags

| Flag | Effect |
|---|---|
| _(none)_ | Bootstrap + start Control Plane UI. Existing DB data preserved. |
| `--no-dev` | Bootstrap only. Prints per-service commands and exits. |
| `--force-reset` | **Destructive.** Wipes all Postgres/Valkey/NATS volumes and the entire `nexus_gateway` database, then re-applies schema + seed. Use only when the schema has diverged beyond what `db push` can reconcile. |

---

## Canonical docs

- [`dev-start.sh`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/dev-start.sh) — the bootstrap script, fully annotated
- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — service lifecycle, log paths, body-level debug fields, admin API helper

**Adjacent wiki pages**: [Prerequisites](Prerequisites) · [First Admin Login](First-Admin-Login) · [Troubleshooting First Run](Troubleshooting-First-Run) · [Your First AI Request](Your-First-AI-Request)
