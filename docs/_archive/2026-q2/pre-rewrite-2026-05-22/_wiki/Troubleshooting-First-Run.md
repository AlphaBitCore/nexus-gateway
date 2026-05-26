# Troubleshooting First Run

This page covers the ten most common failures during a first local bring-up, each with the exact symptom, root cause, and fix. Sources are `scripts/dev-start.sh`, `docs/developers/workflow/local-dev-debugging.md`, and the `/run-local` skill's documented gotcha list. For failures not listed here, read the per-service log file (see [Quickstart](Quickstart) → "Log file locations").

---

## Failure 1 — Docker is not running

**Symptom:**

```
Error response from daemon: dial unix /var/run/docker.sock: connect: connection refused
```

or `docker: command not found`.

**Cause:** Docker Desktop is installed but not started, or Docker is not installed at all.

**Fix:** Start Docker Desktop (macOS/Windows) or the Docker daemon on Linux (`sudo systemctl start docker`). Verify with `docker info` — it should print the server version without an error. Then re-run `./scripts/dev-start.sh`.

---

## Failure 2 — Port conflict

**Symptom:** a service fails to bind on startup, producing a message like:

```
listen tcp :3050: bind: address already in use
```

or the `dev-start.sh` script exits with a compose error referencing a port.

**Cause:** another process (a previous run of the same service, a different project, or a system service) is already listening on one of the required ports (`:3000`, `:3001`, `:3040`, `:3050`, `:3060`, `:55532`, `:6437`, `:4222`).

**Fix:** find and free the conflicting process:

```bash
lsof -nP -iTCP:3050 -sTCP:LISTEN   # replace 3050 with the conflicting port
kill <PID>
```

For Docker infra ports, check whether an old container is still running: `docker ps`. If so, `docker compose down` then re-run the bootstrap.

---

## Failure 3 — Missing `-config <svc>.dev.yaml` flag

**Symptom:** the service process starts but exits immediately with:

```
validate config: hub.id is required
```

or a similar "required field missing" message.

**Cause:** each binary defaults to `<svc>.config.yaml`, which is the production-shape template and intentionally omits dev-only fields like `hub.id`. The `-config <svc>.dev.yaml` flag is required on every `go run` invocation in dev.

**Fix:** add the flag. Example for the Hub:

```bash
cd packages/nexus-hub && go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml
```

---

## Failure 4 — `go.work` missing from build context

**Symptom:** Go tries to download the repo's own modules from GitHub during build:

```
go: downloading github.com/ai-nexus-platform/nexus-gateway/packages/shared v0.0.0-...
```

Changes made locally are not reflected in the running binary.

**Cause:** `GOWORK=off` is set in the shell, or the build was triggered from outside the repo root (e.g. a Docker build that only copied a single `packages/<svc>/` directory).

**Fix:** unset `GOWORK` (`unset GOWORK`) and ensure `go.work` and `go.work.sum` are present at the repo root. Run `go env GOWORK` — it should print the absolute path to `go.work`. Never set `GOWORK=off` in a dev session.

---

## Failure 5 — `GOWORK=off` pulling stale GitHub snapshots

**Symptom:** tests or builds succeed but produce unexpected behavior because they compiled against an old version of `packages/shared/`. The issue is silent — no error, just wrong behavior.

**Cause:** this is the same root cause as Failure 4 but without an obvious error. With `GOWORK=off` each Go module resolves its `require` directives against GitHub snapshots pinned to the pseudo-version in `go.mod`, not the local working tree.

**Fix:** `unset GOWORK`. Confirm with `go env GOWORK` as above. The canonical sanity probe from the README is `GOWORK=off go build ./cmd/<svc>/` — that command should fail or pull remote, which is the expected failure mode. If it succeeds locally it means the pseudo-version on GitHub already matches, but local changes are still bypassed.

---

## Failure 6 — Prisma migrate refused on a dirty DB

**Symptom:**

```
Error: P3009 migrate found failed migrations in the target database
```

or

```
Error: P1001 Can't reach database server at `localhost:55532`
```

**Cause:** a previous interrupted migration left the database in a partially-applied state, OR the Docker Postgres container is not yet ready when `prisma db push` runs.

**Fix for a partially-applied migration:** run a full reset (all local data is lost):

```bash
./scripts/dev-start.sh --force-reset
```

This runs `docker compose down -v` first, wiping the Postgres volume, then re-applies the schema from scratch.

**Fix for the "can't reach" error:** the Docker container is still starting. Re-run `./scripts/dev-start.sh` — it polls `pg_isready` with 30 retries before proceeding.

---

## Failure 7 — NATS JetStream not enabled

**Symptom:** the Hub starts but `traffic_event` rows never appear in Postgres, or the Hub log contains:

```
nats: JetStream not enabled
```

or consumer subscription errors.

**Cause:** the NATS server was started without JetStream enabled, or the container image is too old. The `docker-compose.yml` passes `--jetstream` to the NATS container; if a user has an overriding Docker Compose file or a stale local container, JetStream may be absent.

**Fix:** stop and remove the NATS container and let `docker compose up` recreate it:

```bash
docker compose stop nats && docker compose rm -f nats
docker compose up -d nats
```

Wait for NATS to be healthy, then restart the Hub.

---

## Failure 8 — Valkey vs Redis 7 confusion

**Symptom:** `docker compose up` starts a container named `nexus-redis` instead of `valkey`, or the service log contains:

```
WRONGTYPE Operation against a key holding the wrong kind of value
```

**Cause:** a leftover `nexus-redis` container from before the Valkey migration (E61, 2026-05-20) is still running on port `:6437`. The `docker-compose.yml` now defines a service named `valkey` using the `valkey/valkey-bundle:8-trixie` image. Old Redis containers are not automatically removed.

**Fix:**

```bash
docker compose down -v         # removes all named volumes + containers
docker compose up -d           # recreates with the current compose file (Valkey 8)
```

Then re-run `./scripts/dev-start.sh` to re-seed.

---

## Failure 9 — Seed not run, seeded VK and routing rules missing

**Symptom:** `curl http://localhost:3050/v1/chat/completions -H "Authorization: Bearer $VK" ...` returns `401 Unauthorized` even though the VK was copy-pasted from the DB query. Or the DB query for virtual keys returns zero rows.

**Cause:** `npx prisma db seed` was not run or failed silently. This can happen when `./scripts/dev-start.sh` was interrupted partway through, or when `CREDENTIAL_ENCRYPTION_KEY` was missing from `tools/db-migrate/.env` (causing the seed to abort).

**Fix:** re-run the bootstrap:

```bash
./scripts/dev-start.sh
```

The script is idempotent. If the seed fails with `CREDENTIAL_ENCRYPTION_KEY must be a 64-char hex string`, `tools/db-migrate/.env` is missing the key — the script propagates it automatically on re-run.

---

## Failure 10 — `.env` file missing, services fail with secret errors

**Symptom:** the AI Gateway or Control Plane exits on boot with:

```
hubclient: INTERNAL_SERVICE_TOKEN is not set
```

or:

```
credential decrypt: CREDENTIAL_ENCRYPTION_KEY is not set
```

**Cause:** the repo-root `.env` file does not exist. Go services load it via `packages/shared/core/bootenv` at startup — no `source .env` needed, but the file must be present at the repo root.

**Fix:** re-run the bootstrap script — it creates `.env` from `.env.example` on first run:

```bash
./scripts/dev-start.sh --no-dev
```

If `.env` exists but contains `CHANGE_ME_*` placeholders, the bootstrap failed partway through. Delete `.env` and re-run — the script substitutes every placeholder with safe dev defaults.

---

## Canonical docs

- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — service lifecycle, kill/restart authority, body-level debug fields
- [`dev-start.sh`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/dev-start.sh) — annotated bootstrap script; read the inline comments for the intent behind each step

**Adjacent wiki pages**: [Prerequisites](Prerequisites) · [Quickstart](Quickstart) · [First Admin Login](First-Admin-Login) · [Your First AI Request](Your-First-AI-Request)
