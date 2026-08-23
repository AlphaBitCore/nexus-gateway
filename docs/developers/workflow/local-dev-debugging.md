# Local dev debugging

Operational reference for running and debugging the stack on a workstation. The
**Environment variables** and **Test / skill env files** sections below carry
binding force — they are the canonical contracts those rules point at.

## Running the stack locally

Bootstrap the infrastructure dependencies once with `./scripts/dev-start.sh`: it
brings up PostgreSQL, Valkey, and NATS, runs the migrations, and seeds the
database (`--force-reset` wipes all local data, including `traffic_event`). Then
run each service in the foreground:

```bash
cd packages/<svc> && go run ./cmd/<svc>/ -config <svc>.dev.yaml
```

Local ports: Hub `3060`, Control Plane `3001`, AI Gateway `3050`, Compliance
Proxy `3128`, and the Control Plane UI on `3000` (`npm run dev:control-plane-ui`).

## Service logs

Logging is configured per service (`LogConfig`):

- `level` — `trace` | `debug` | `info` | `warn` | `error` (default `info`); the
  `LOG_LEVEL` environment variable overrides it.
- `format` — `json` | `text` (default `json`).
- `file` — optional path to tee logs to (also via the `LOG_FILE` env var);
  otherwise logs go to stdout.
- `stackOnError` — attach a goroutine stack on error-level logs (env
  `LOG_STACK_ON_ERROR`).

Because local services run in the foreground, their logs stream to the terminal
on stdout as JSON by default; set `LOG_FILE` to also capture them to a file. The
log level is additionally hot-reloadable through the Hub shadow (the `log_level`
config key), so verbosity can be raised on a running service without a restart.

## Restarting a service

A local service is just a foreground process. Restart it by stopping the process
(Ctrl-C, or kill it) and re-running its `go run` command. `./scripts/dev-start.sh`
refreshes the infrastructure dependencies and seed when those need to be reset.

## Admin API helper

`tests/lib/auth.sh` provides the Control Plane admin-API helpers, all driven from
the loaded environment:

- `cp_login` — drives the OAuth + PKCE flow once and caches the access token
  (idempotent): `/oauth/authorize` → `/authserver/password` → `/oauth/token`. It
  reads `NEXUS_CP_URL`, `NEXUS_ADMIN_EMAIL`, `NEXUS_ADMIN_PASSWORD`,
  `NEXUS_OAUTH_CLIENT_ID` (default `cp-ui`), and `NEXUS_OAUTH_REDIRECT_URI`.
- `cp_curl <path> [args…]` — calls `<path>` with the cached bearer token (GET, or
  any method via `-X`).
- `cp_curl_code <path>` — returns just the HTTP status code.
- `cp_curl_full <path>` — returns the body plus a `---HTTP_STATUS---` line and the
  code.
- `cp_token` — returns the cached token, logging in first if needed.

## AI Gateway body-level debug logging

When the AI Gateway runs at `LOG_LEVEL=debug` (or `trace`), the dispatcher wraps
the upstream response body in a `debugBody` reader. On close it emits a single
`upstream stream body` record at DEBUG level — a snapshot of what the provider
actually sent over the wire — with these fields:

- `format` — the wire format being read.
- `bytes_captured` — how many bytes were accumulated.
- `capped` — whether the capture hit its byte limit (the `body` then carries a
  `(truncated)` suffix).
- `body` — the captured bytes (or `(empty — no bytes read from stream body)`).

This is gated behind a DEBUG-enabled check and never runs in production.

## Environment variables

Local development loads secrets and configuration from a repo-root `.env` file
through `bootenv` (`packages/shared/core/bootenv/`). The contract:

- Application code reads every secret via `os.Getenv` only; the `.env` file is a
  convenience loader, not something the code reads directly.
- Loading is **non-overload**: a variable already present in the process
  environment wins over the `.env` value. That lets both a shared `.env` and an
  explicit `MY_VAR=x ./svc` invocation work, with the explicit value taking
  precedence.
- The loader is silent when no `.env` exists — production has none (its
  environment comes from systemd / Kubernetes), so the boot log stays clean;
  parse errors of an `.env` that does exist are surfaced.

Every variable is documented in the repo-root `.env.example`. **Secrets are
env-only**: no secret field (auth tokens, HMAC keys, credential-encryption keys,
internal-service tokens, DB passwords) belongs in committed YAML — YAML carries
only non-secret shape and tunings. Cross-service shared secrets are tagged
`[MUST MATCH]` in `.env.example`; drift between consumers of a `[MUST MATCH]`
value is the most common source of inter-service 403s. In production these
variables are delivered via a systemd `EnvironmentFile=` or a Kubernetes Secret.

## Test / skill env files

Tests and the `prod-*` skills read their configuration from `tests/.env.<target>`,
where `target` is one of `local`, `dev`, `stg`, or `prod`, loaded by
`tests/lib/loadenv.sh` (and its Python twin `tests/lib/loadenv.py`, which mirrors
the same semantics — change both together or the two halves of the harness drift):

- **Layering.** The loader parses `.env.<target>.example` first, then
  `.env.<target>` on top, with the same non-overload semantics — a value already
  in the process environment is preserved, and the files only fill gaps. It fails
  with a copy-from-example hint if neither file exists.
- **Target selection.** The target is the first argument if given, else
  `$NEXUS_TEST_TARGET`, else `local` on an interactive TTY. A non-interactive
  context (CI, cron) must set `NEXUS_TEST_TARGET` explicitly — it will not silently
  default.
- **Fail-closed safety guards.** With `target=local`, every `NEXUS_*_URL` must
  reference `localhost` / `127.0.0.1`; a `.env.local` accidentally pointing at
  production fails fast. With `target=prod`, `NEXUS_CP_URL` must not be a loopback
  address, catching a freshly-copied `.env.prod` left with localhost defaults.
  This keeps a state-mutating test or skill from running against the wrong target.
  `dev` and `stg` carry no URL guard — they are remote by definition and there is
  no wrong-direction mistake to catch.
- **When to use `stg`.** Any suite that needs a real completion — notably
  `tests/e2e-python/sdk_compat/` and `tests/e2e-node/` — needs a target whose
  provider credentials actually work. Local qualifies once its `Credential` rows
  decrypt and the keys are valid; `stg` is the fallback when they do not, or when
  you need a provider local has no key for. A `502 PROVIDER_UNAVAILABLE` is always
  an environment problem, never a compat one — diagnose it with the Control Plane
  credential probe (`POST /api/admin/credentials/{id}/probe`) rather than reading it
  as a test failure. Two failure modes to know apart: `credentials: decryption
  failed: cipher: message authentication failed` means `CREDENTIAL_ENCRYPTION_KEY`
  no longer matches the key those rows were written with (restore the old key, add
  it to `CREDENTIAL_KEY_MAP`, or re-enter the keys in the Control Plane so they are
  re-encrypted); a provider `401` means the stored key itself is wrong or revoked.
- **Adding a new variable.** Add it to the committed `.env.<target>.example` with a
  placeholder or safe default — that file is what documents the variable — and put
  the real value in the gitignored `.env.<target>`. The example-then-user layering
  means a variable documented in the example is always present.

## References

- `scripts/dev-start.sh` — local stack bootstrap
- `packages/shared/core/bootenv/` — repo-root `.env` loader
- `.env.example` — the documented variable catalog and `[MUST MATCH]` tags
- `tests/lib/loadenv.sh` — the `tests/.env.<target>` loader and safety guards
- `tests/lib/auth.sh` — the `cp_login` / `cp_curl` admin-API helpers
- `packages/ai-gateway/internal/config/` — the service logging configuration
- `packages/ai-gateway/internal/providers/dispatch/spec_adapter.go` — the debug body capture
