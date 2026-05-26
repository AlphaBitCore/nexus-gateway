# Local dev debugging — services, logs, body inspection, admin API

Operational reference for working with the local stack. Bindings (kill/restart authority, body-level debugging tables, admin API helper) are extracted from CLAUDE.md so the charter stays focused on policy; the operational rules below still apply with the same force.

## Service log files

When debugging Go services, read **on-disk logs** as well as the Debug Console. With `*.dev.yaml`, `shared/logging` tees to `log.file` under each service's **process cwd** (VS Code `launch.json` sets `cwd` to `packages/<service>`). Typical paths:

| Service | Log file (relative to repo) |
|---------|-----------------------------|
| Nexus Hub | `packages/nexus-hub/logs/nexus-hub.log` |
| Control Plane | `packages/control-plane/logs/control-plane.log` |
| AI Gateway | `packages/ai-gateway/logs/ai-gateway.log` |
| Compliance Proxy | `packages/compliance-proxy/logs/compliance-proxy.log` |
| Agent (`run`) | `packages/agent/logs/agent.log` |

Override without editing YAML: `LOG_FILE` (path) and `LOG_STACK_ON_ERROR` (`true`/`1`/`yes`). See `packages/shared/core/logging` and each package's `*.dev.yaml` `log` / `logging` section.

## Test / skill env files — `tests/.env.<target>` (binding)

Tests and prod-* skills read their configuration from `tests/.env.<target>` where target ∈ {`local`, `dev`, `prod`}. This is **distinct** from the Go-service `.env` at repo root (Go services boot env vs. test/skill runtime env — same concept, different consumers).

| Loader | Used by |
|---|---|
| `tests/lib/loadenv.sh <target>` | bash test scripts + skill markdown command examples |
| `tests/lib/loadenv.py` (`loadenv.load(target)`) | Python smoke (`tests/scripts/smoke-gateway.py`) |
| `tests/integration-go/helpers.LoadEnv()` | Go scenarios + integration tests |
| `tests/lib/env.sh` | back-compat shim → `loadenv.sh` (existing bash scripts that source `env.sh` keep working) |

Target selection: explicit arg / CLI flag > `NEXUS_TEST_TARGET` env > default `local` (TTY only — non-TTY runs must set `NEXUS_TEST_TARGET` explicitly). All loaders honor non-overload semantics (process env wins over file).

Safety: target=local fails closed if any `NEXUS_*_URL` isn't loopback; target=prod fails closed if `NEXUS_CP_URL` IS loopback. `tests/lib/auth.sh` derives its token cache path from `NEXUS_TEST_TARGET` so prod and local tokens don't cross-pollute. Prod-* skills (`prod-login` / `prod-deploy` / `prod-debug`) source `loadenv.sh prod` first, then call `cp_login` / `cp_curl` etc.

Adding a new env var consumed by tests/skills means: (1) document it in every `tests/.env.<target>.example` you ship, (2) consumers read it via the loader contract; never bypass with `os.Getenv` for values already in the env file.

## Environment variables — repo-root `.env` (binding)

Every service `main.go` calls `bootenv.LoadFromRepoRoot()` first thing in `run()`, which walks up from cwd looking for the `.git/` marker and loads `<repo-root>/.env` if present. This means:

- **Local dev**: copy `.env.example` → `.env` at repo root, edit the placeholder values, restart the service. No `source .env`, no `export VAR=...` — `bootenv` picks it up automatically from `packages/<svc>/` cwd.
- **Production**: there's no `.env` file in prod. Inject env vars via `systemd EnvironmentFile=/etc/nexus-gateway/env`, K8s Secret → env, or `docker --env-file`. `bootenv` silently skips when `.env` is absent.
- **Override precedence**: existing process env vars win over `.env` values (`godotenv` non-overload). `MY_VAR=x ./svc` still works for one-off overrides without editing `.env`.
- **Secrets are env-only**: per CLAUDE.md "Secrets are env-only" binding, every secret (`INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`, `COMPLIANCE_PROXY_API_TOKEN`, `AI_GATEWAY_API_TOKEN`, …) lives in `.env` (dev) or systemd EnvironmentFile (prod) — never in any `*.dev.yaml` / `*.prod.yaml.example`. See `.env.example` for the full variable contract + `[MUST MATCH]` cross-service constraints.

## Service lifecycle — agent may restart services autonomously

When the user is iterating on Go code or asks to verify behaviour end-to-end, the agent **is permitted to kill and relaunch the local Go services** (Nexus Hub, Control Plane, AI Gateway, Compliance Proxy, Agent) without per-restart confirmation, so that code updates are actually loaded into the running binary before testing. This applies only to **local dev processes** owned by this workspace; never touch a process the agent did not start or cannot identify as a workspace service.

Operating rules:

- **Identify before killing.** Use `lsof -nP -iTCP:<port> -sTCP:LISTEN` (3060 hub, 3001 CP, 3050 AI Gateway, 3040 compliance proxy) or `ps -ef | grep <service>` and confirm the binary path is under `packages/<service>/`. If a debugger (`dlv`, `__debug_bin*`, VS Code/GoLand) is attached, **prefer telling the user** instead of killing — debugger processes hold breakpoints and unsaved state.
- **Graceful first.** Send `SIGTERM` (`kill <pid>`); only escalate to `kill -9` if the process did not exit within ~3 s.
- **Restart with the same config.** Reuse the package's `*.dev.yaml` (e.g. `cd packages/ai-gateway && go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml`). Run in the background so the agent keeps control of the shell, and tail the corresponding log file (see table above) to verify the service came up healthy before claiming the test is done.
- **Out of scope without explicit user request.** Do **not** restart the Control Plane UI dev server (Vite HMR is usually fine), Postgres, Redis, NATS, or any Docker container; do **not** run `docker compose down`, `docker volume rm`, or anything that drops local state.
- **Verification before completion still applies.** After restart, re-run the failing scenario (e.g. the curl that reproduced the bug) and confirm the new behaviour from logs + response before reporting success.

## AI Gateway body-level debugging (provider request / response inspection)

When debugging issues in `packages/ai-gateway/` that involve model request/response body transformation or processing (e.g. wrong token counts, empty streaming responses, format translation bugs), enable DEBUG-level logging and read the structured log fields that the gateway already emits at `slog.LevelDebug`:

| Log message | Fields | When emitted |
|---|---|---|
| `"upstream request body"` | `format`, `url`, `body` (first 8 KB) | Before `Transport.Do()` — shows the exact JSON sent to the provider |
| `"upstream response headers"` | `format`, `status`, `stream`, `content_type`, `content_length`, `body_nil` | After `Transport.Do()` — confirms provider status and headers |
| `"upstream stream body"` | `format`, `bytes_captured`, `body` (first 8 KB) | On stream body close — shows raw SSE bytes received from provider |
| `"outbound http"` | `url`, `status`, `req_bytes`, `resp_bytes`, `duration_ms` | After response body closed — end-to-end byte counts |

**To activate**: ensure `log.level: "debug"` in the service's `*.dev.yaml`; the fields are already wired in `spec_adapter.go` (request body, response headers, stream body) and `shared/httpclient/logging.go` (outbound http). No code changes are needed for standard body inspection.

**When to add temporary debug logs**: if the above are insufficient (e.g. you need mid-stream chunk values or internal state), add a temporary `slog.LevelDebug` log at the relevant point — e.g. inside `geminiStreamSession.Next()`, `chunkSSEReader.Read()`, or the LivePipeline loop. Remove them before committing; they are for local investigation only.

## API debugging (Control Plane)

The Control Plane API (port 3001) uses OAuth + PKCE bearer tokens — the cookie-based `/api/admin/auth/login` endpoint is gone. Use the helper at `tests/lib/auth.sh`:

```bash
# Source helpers via the target-aware loader (defaults to local).
source tests/lib/loadenv.sh        # reads tests/.env.local (or .example)
source tests/lib/auth.sh

cp_login                                                       # idempotent; caches token at /tmp/nexus_token_local
cp_curl /api/admin/analytics/cost?groupBy=device               # any GET path
cp_curl -X POST /api/admin/routing-rules -d @rule.json         # POST / PUT / DELETE via -X

# Query the database directly via Docker:
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "SELECT ..."
```

Seed login credentials: `admin@nexus.ai / admin123` (super_admin), `alice@nexus.ai / admin123`, `carol@nexus.ai / compliance123`, `bob@nexus.ai / provider123`, `diana@nexus.ai / viewer123`.
