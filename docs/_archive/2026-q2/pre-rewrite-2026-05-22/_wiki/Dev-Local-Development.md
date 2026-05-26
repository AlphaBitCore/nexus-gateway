# Dev Local Development

*Audience: contributors running the local stack for daily development.*

The local stack brings up PostgreSQL, Valkey (Redis-compatible), and NATS JetStream in Docker, then runs the four Go services and the React UI as local processes. A one-command bootstrap covers the Docker layer; services are started individually so each can be relaunched after a code change without disturbing the rest. This page covers the bootstrap, service log paths, kill/restart rules, the admin API helper, and body-level debugging for the AI Gateway.

---

## Bootstrap and service startup

Run the bootstrap once per clone — it starts the Docker containers, installs npm dependencies, and runs Prisma migrations:

```bash
./scripts/dev-start.sh
```

After bootstrap, start each Go service individually:

```bash
cd packages/nexus-hub        && go run ./cmd/nexus-hub/        -config nexus-hub.dev.yaml
cd packages/control-plane    && go run ./cmd/control-plane/    -config control-plane.dev.yaml
cd packages/ai-gateway       && go run ./cmd/ai-gateway/       -config ai-gateway.dev.yaml
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml
```

Start the admin UI in a separate terminal:

```bash
npm run dev:control-plane-ui     # serves on http://localhost:3000
```

Ports: Hub `:3060`, Control Plane `:3001`, AI Gateway `:3050`, Compliance Proxy `:3040`, UI `:3000`.

The `-config <service>.dev.yaml` flag is mandatory. Without it the service looks for its production YAML path, which does not exist locally.

### Environment variables

Every service `main.go` calls `bootenv.LoadFromRepoRoot()` on startup, which walks up from cwd to find `.git/` and loads `<repo-root>/.env` if present. Copy `.env.example` to `.env` at the repo root and fill in the placeholder values — no `export VAR=...` is needed.

All secrets (credential encryption key, internal service token, HMAC secret) live in `.env` for local dev and in systemd `EnvironmentFile` for production. Secrets never appear in any `*.dev.yaml` or YAML file committed to the repo.

Cross-service secrets are marked `[MUST MATCH]` in `.env.example`. Drift between consumers is the most common source of inter-service 403s on a fresh setup.

---

## Service log files

Go services tee structured logs to disk when using `*.dev.yaml`. Log files land under the service's process cwd:

| Service | Log file |
|---|---|
| Nexus Hub | `packages/nexus-hub/logs/nexus-hub.log` |
| Control Plane | `packages/control-plane/logs/control-plane.log` |
| AI Gateway | `packages/ai-gateway/logs/ai-gateway.log` |
| Compliance Proxy | `packages/compliance-proxy/logs/compliance-proxy.log` |
| Agent | `packages/agent/logs/agent.log` |

Override without editing YAML: `LOG_FILE` (path) and `LOG_STACK_ON_ERROR` (`true`/`1`/`yes`). The logging implementation is in `packages/shared/core/logging`.

When debugging a specific service, `tail -f` its log file in parallel with the IDE Debug Console. The log file is more reliable for structured fields (JSON) than raw terminal output.

---

## Kill and restart services

After editing Go code, the binary must be rebuilt and the service restarted to load the new code. The agent is authorized to kill and relaunch local Go services without per-restart confirmation during an iteration session.

Rules for safe kill/restart:

1. **Identify before killing.** Use `lsof -nP -iTCP:<port> -sTCP:LISTEN` (Hub `:3060`, Control Plane `:3001`, AI Gateway `:3050`, Compliance Proxy `:3040`) or `ps -ef | grep <service>`. Confirm the binary path is under `packages/<service>/`. If a debugger (`dlv`, VS Code, GoLand) is attached, prefer informing the user instead of killing.
2. **Graceful first.** Send `SIGTERM` (`kill <pid>`); escalate to `kill -9` only if the process did not exit within ~3s.
3. **Restart with the same config.** Use the same `*.dev.yaml` flag as the original start.
4. **Out of scope without explicit user request.** Do not restart the UI dev server (Vite HMR handles code changes), Postgres, Valkey, NATS, or any Docker container.

---

## Admin API helper

The Control Plane API uses OAuth + PKCE bearer tokens. The `tests/lib/auth.sh` helper manages login and token caching:

```bash
# Load the test environment and auth helpers (defaults to local target).
source tests/lib/loadenv.sh
source tests/lib/auth.sh

cp_login                                                        # idempotent; caches token at /tmp/nexus_token_local
cp_curl /api/admin/analytics/cost?groupBy=device               # any GET path
cp_curl -X POST /api/admin/routing-rules -d @rule.json         # POST / PUT / DELETE via -X
cp_curl_code /api/admin/providers                              # returns HTTP status code only
cp_curl_full /api/admin/ready                                  # returns headers + body
```

Database queries directly via Docker:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "SELECT id, source, model FROM traffic_event LIMIT 5;"
```

Seed credentials: `admin@nexus.ai / admin123` (super_admin), plus `alice@nexus.ai`, `carol@nexus.ai`, `bob@nexus.ai`, `diana@nexus.ai` for testing role-scoped access.

### Test/skill env files

Tests and prod-* skills read configuration from `tests/.env.<target>` where target ∈ {`local`, `dev`, `prod`}. The loader (`tests/lib/loadenv.sh <target>`) sets all `NEXUS_*` variables. Target selection: explicit arg > `NEXUS_TEST_TARGET` env > default `local` (TTY only — non-TTY runs must set `NEXUS_TEST_TARGET` explicitly).

Safety guards are fail-closed: `local` target fails if any `NEXUS_*_URL` resolves outside loopback; `prod` target fails if `NEXUS_CP_URL` is loopback. These guards prevent a stale env file from accidentally targeting production.

---

## AI Gateway body-level debugging

When debugging body transformation issues in `packages/ai-gateway/` (wrong token counts, empty streaming responses, format translation bugs), enable `log.level: "debug"` in `ai-gateway.dev.yaml`. The AI Gateway already emits these structured fields at DEBUG level:

| Log message | Key fields | When emitted |
|---|---|---|
| `"upstream request body"` | `format`, `url`, `body` (first 8 KB) | Before the provider call — shows the exact JSON sent |
| `"upstream response headers"` | `format`, `status`, `stream`, `content_type` | After the provider responds — confirms status and headers |
| `"upstream stream body"` | `format`, `bytes_captured`, `body` (first 8 KB) | On stream close — shows raw SSE bytes received |
| `"outbound http"` | `url`, `status`, `req_bytes`, `resp_bytes`, `duration_ms` | After response body closed — end-to-end byte counts |

No code changes are needed for standard body inspection — these log points are already wired in `spec_adapter.go` and `shared/httpclient/logging.go`.

---

## Canonical docs

- [`docs/developers/workflow/local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — authoritative operational reference with binding rules for service lifecycle, env contract, and body-level debugging
- [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — Go and TypeScript conventions, tooling table

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev Repo Structure](Dev-Repo-Structure) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Quickstart](Quickstart) · [Troubleshooting First Run](Troubleshooting-First-Run)
