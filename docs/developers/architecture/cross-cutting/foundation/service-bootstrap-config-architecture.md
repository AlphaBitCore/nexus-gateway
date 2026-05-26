# Service bootstrap config architecture

## §1 — Scope

A05 is the loader-level companion to A03 (`configuration-architecture.md`). A03 defines **which layer** a field belongs in (yaml / env / template / system_metadata) and the invariants that govern each layer. A05 defines **how** the yaml + env layers actually reach a running process at boot: which functions are called in what order, how the `--config` flag picks the yaml file, how env variables overlay yaml values, and how validation gates the boot.

The four backend services (`nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy`) follow a single uniform boot pattern. The agent is structurally different — desktop binary, no repo around at runtime, OS-level service manager (launchd / systemd / SCM) provides env — so its loader has different precedence and required-field rules. Both are covered here.

## §2 — The shared `main()` pattern (4 server services)

Every server service's `main()` runs the same five steps in the same order:

```
1. bootenv.LoadFromRepoRoot(slog.Default())
   → load <repo-root>/.env into the process env (no-op in prod;
     dev convenience only). See A03 §4 + R3.

2. configPath := flag.String("config", "<svc>.config.yaml", ...)
   → --config flag with the service's own default filename.

3. cfg, err := config.Load(*configPath)
   → returns the parsed + overlaid + (sometimes) validated config.
     The internal structure of Load() varies per service — see §3.

4. logger, err := logging.NewLogger(cfg.Log)
   → bring up slog with the loaded log config.

5. dbPool, err := wiring.InitDB(ctx, cfg, logger)
   ...
   → bring up DB pool, Redis, NATS, HTTP server, etc.
```

Source: `packages/<svc>/cmd/<svc>/main.go` for each of the four services. All four import `packages/shared/core/bootenv` for step 1, all four use the `flag.String("config", "<svc>.config.yaml", ...)` convention for step 2, and each has its own config package for step 3 (`internal/config` for Hub and AI Gateway; `cmd/<svc>/config` for Control Plane and Compliance Proxy).

## §3 — `Load(path)` — canonical 4-step pattern across all 4 services

All four backend services share the same `Load(path)` shape:

```
Load(path):
  cfg = defaults()
  data, err = os.ReadFile(path)
  if err and not IsNotExist → return wrapped read error
  if file present          → yaml.Unmarshal(data, cfg)
  applyEnvOverrides(cfg)
  if err := validate(cfg); err != nil → return wrapped validate error
  return cfg
```

The function name (`Load`), helper names (`defaults`, `applyEnvOverrides`, `validate`), and step order are identical across `nexus-hub`, `control-plane`, `ai-gateway`, and `compliance-proxy`. The yaml step is optional (missing-file is tolerated as "defaults + env carry the config"); every other step always runs.

What can vary between services:

- **The set of defaults.** Each `defaults()` seeds the tunables that make sense for that service (e.g. Hub seeds `Server.Port=3060`, scheduler intervals, retention windows; CP-proxy intentionally seeds only `Log.Level=info` because Listener/CA paths have no safe default).
- **The set of env overrides** (catalogued in §5).
- **The required-field set** that `validate()` enforces (catalogued in §6 — the cross-service core plus per-service additions).
- **Post-env, pre-validate transforms.** AI Gateway runs one extra step between `applyEnvOverrides` and `validate`: it field-merges `Routing.DefaultRetryPolicy` against the platform default and clamps `MaxAttemptsPerTarget` to `[1,5]`. The transform sits inside `Load` next to the canonical steps; the canonical steps themselves are unchanged.

What is fixed across all four:

- **Function name `Load(path string) (*Config, error)`** + helper names `defaults() *Config` / `applyEnvOverrides(cfg *Config)` / `validate(cfg *Config) error` (all package-private). The validate helper is always a free function on `*Config`, never a method.
- **Step order.** Env overlays yaml; yaml does not get a second chance to override env. Validate runs last, after env, against the final merged struct.
- **Missing-file tolerance.** `os.IsNotExist` short-circuits the yaml step; any other read error is wrapped and returned.
- **Error wrapping.** `read config`, `parse config`, `validate config` prefixes let operators tell apart I/O, parse, and policy failures at a glance.

## §4 — yaml file selection (`--config` flag)

The `--config` flag's default value is the service's `<svc>.config.yaml` (e.g. `nexus-hub.config.yaml`). The default value is the filename only — the loader passes it straight to `os.ReadFile`, which resolves relative to the process's current working directory.

The loader has **no concept of "dev mode" vs "prod mode"**. The local-dev convention is: invoke the binary with `--config <svc>.dev.yaml` (e.g. `go run ./cmd/nexus-hub -config nexus-hub.dev.yaml`). The repo holds both files at `packages/<svc>/<svc>.config.yaml` (prod shape) and `packages/<svc>/<svc>.dev.yaml` (local-dev overrides). The "dev" vs "prod" distinction is purely a file naming convention; it never reaches the Go code.

Prod deployments pass a path appropriate to the deploy host. The repo carries no systemd unit for the four backend services (those are provisioned by the deploy pipeline rather than committed templates), so the canonical prod path varies per deployment — see `prod-deploy` skill for the live one. The agent does ship a committed systemd unit at `packages/agent/platform/linux/installer/systemd/nexus-agent.service`; its `ExecStart` passes `-config /etc/nexus-agent/agent.yaml` (note the agent-specific filename — see §7).

## §5 — env override (hand-enumerated `os.Getenv` blocks)

Every service expresses its env-override surface as a series of explicit `os.Getenv` blocks inside a free function named `applyEnvOverrides(cfg *Config)`. The shape is uniform:

```go
if v := os.Getenv("VAR_NAME"); v != "" {
    cfg.Field = parse(v)
}
```

**Only env variables explicitly enumerated in code can override yaml.** Reflection or struct-tag-driven override is not used anywhere. To make a new field env-overridable, add an `os.Getenv` block inside the service's `applyEnvOverrides` in the same PR.

Cross-service env vars (shared shape, identical name in every service that has the field):

| Env var | Effect |
|---|---|
| `DATABASE_URL` | `cfg.Database.URL` |
| `INTERNAL_SERVICE_TOKEN` | `cfg.Auth.InternalServiceToken` (env-only, never yaml — see A03 R2) |
| `NEXUS_HUB_URL` | `cfg.Registry.NexusHubURL` (non-Hub services only) |
| `MQ_DRIVER` | `cfg.MQ.Driver` |
| `NATS_URL` | `cfg.MQ.NATS.URL` |
| `LOG_LEVEL` / `LOG_FORMAT` | `cfg.Log.{Level,Format}` |
| `<SERVICE>_PUBLIC_URL` | `cfg.PublicURL` — one per service: `NEXUS_HUB_PUBLIC_URL`, `CONTROL_PLANE_PUBLIC_URL`, `AI_GATEWAY_PUBLIC_URL`, `COMPLIANCE_PROXY_PUBLIC_URL` |

Per-service additions (the full list is in each service's `applyEnvOverrides`):

| Service | Notable additions |
|---|---|
| `nexus-hub` | `NEXUS_HUB_ID`, `NEXUS_HUB_ADVERTISE_ADDR`, `NEXUS_HUB_ALLOWED_ORIGINS`, `NEXUS_HUB_PORT`, `NEXUS_HUB_SCHEDULER_*` (retention + interval knobs), `AGENT_CA_{CERT,KEY,DIR}` |
| `control-plane` | `CONTROL_PLANE_PORT`, `CONTROL_PLANE_CRYPTO_PRODUCTION`, `COMPLIANCE_PROXY_URL`, `AI_GATEWAY_URL`, `COMPLIANCE_PROXY_RUNTIME_URL`, `COMPLIANCE_PROXY_API_TOKEN`, `CREDENTIAL_ENCRYPTION_{KEY,PASSPHRASE,SALT}`, `CREDENTIAL_KEY_MAP`, `AGENT_CA_DIR`, `ADMIN_KEY_HMAC_SECRET`, `AUTH_SERVER_ISSUER`, `AUTH_SERVER_KEYSTORE_DIR`, `OTEL_*` |
| `ai-gateway` | `AI_GATEWAY_PORT`, `AI_GATEWAY_CORS_{ENABLED,ALLOWED_ORIGINS}`, `AI_GATEWAY_CACHE_{ENABLED,TTL,PREFIX}`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`, `CREDENTIAL_KEY_MAP`, `OTEL_*` |
| `compliance-proxy` | (none beyond the cross-service set) |

`REDIS_*` env vars (e.g. `REDIS_ADDRS`) are consumed by `redisfactory.LoadEnv` + `redisfactory.New` at wiring time, not by any service's `applyEnvOverrides`. The service's `validate` checks Redis presence by reading **either** `cfg.Redis.Addrs` (yaml-populated) **or** `os.Getenv("REDIS_ADDRS")` directly, mirroring the env-merge contract redisfactory uses internally.

**Relationship to `bootenv`:** the two cooperate sequentially. `bootenv.LoadFromRepoRoot` (step 1 in §2) loads `<repo-root>/.env` into the process env (with the non-overload rule from A03 R3: process env always wins over file values). `applyEnvOverrides` then reads from the process env via `os.Getenv` and overlays into `cfg`. A secret-tagged env var like `INTERNAL_SERVICE_TOKEN` typically lives in `.env` for local dev (loaded by bootenv), then read by `applyEnvOverrides` into `cfg.Auth.InternalServiceToken`. In prod, `.env` does not exist; systemd `EnvironmentFile=` or K8s Secret injects the same env var directly, and the same env-block picks it up.

## §6 — Required-field validation (`validate(cfg)` in every service)

Every service runs `validate(cfg *Config) error` at the end of `Load`. The function lives in the service's config package as a free function (not a method) so call sites read symmetrically. A failure is wrapped as `validate config: <reason>` and surfaced from `Load`, terminating boot before any handler is wired up.

The required-set is split into a **cross-service core** (every backend service enforces it) plus **per-service additions** for fields that only make sense in one service.

**Cross-service core** (Hub, CP, AI Gateway, Compliance Proxy all enforce):

| Field | Why it's required |
|---|---|
| `cfg.PublicURL` | Reported as `staticInfo` on Thing registration; admin UI uses it to render service-specific URLs (agent-setup page, integration help cards, smoke-test endpoint hints). |
| `cfg.Database.URL` | Every service is DB-bound at boot (Hub: shadow + audit; CP: admin handlers; AIG: traffic_event + VK lookups; CP-proxy: traffic_event + audit). |
| `cfg.Auth.InternalServiceToken` | Bearer on Hub WS/HTTP; shared with Hub. Mismatch → all inter-service calls 403. Env-only per A03 R2. |
| `cfg.Redis.Addrs` OR `os.Getenv("REDIS_ADDRS")` | Session store / IAM cache / rate limit / response cache / quota counters. Read both yaml + env because `redisfactory.LoadEnv` merges env at wiring time, not at config.Load. |
| `cfg.MQ.Driver` | NATS JetStream is the only supported value today. Without it the service has no MQ transport for cross-service events. |
| `cfg.MQ.NATS.URL` (when `Driver=="nats"`) | NATS connection endpoint; conditional because future MQ drivers may not need it. |

**Per-service additions:**

| Service | Additional required fields | Reason |
|---|---|---|
| `nexus-hub` | `cfg.Hub.ID` | Identifies this Hub instance in `nexus.hub.signal` cross-Hub fanout; defaults to `hub-<hostname>` via `defaults()`. |
| `control-plane` | `cfg.Registry.NexusHubURL` | CP registers as a Thing on boot. |
| `ai-gateway` | `cfg.Auth.HMACSecret`, `cfg.Auth.CredentialMasterKey`, `cfg.Registry.NexusHubURL` | HMAC hashes VK + Admin API keys before DB lookup (env-only, `ADMIN_KEY_HMAC_SECRET`). Master key decrypts Hub-pushed provider credentials (env-only, `CREDENTIAL_ENCRYPTION_KEY`). Hub URL because AIG registers as a Thing. |
| `compliance-proxy` | `cfg.Registry.NexusHubURL`, `cfg.Listener.Address`, `cfg.CA.CertPath`, `cfg.CA.KeyPath` | Hub URL for Thing registration. Listener address terminates inbound CONNECT. Sub-CA cert + key fly-issue MITM leaf certs. |

Beyond the required-set, each service's `validate` may also enforce **shape constraints** on optional fields (e.g. CP-proxy validates that `connections.idleTimeout` parses as a `time.Duration > 0` when set, and that `log.level` is one of `trace/debug/info/warn/error`). These guards reject obviously-broken yaml at boot rather than surfacing the failure on first request.

Adding to the required-set is a deliberate change. The per-service `validate` is the single place to look — if a field is not checked there, it is not enforced at boot regardless of how load-bearing it might be at runtime.

## §7 — Agent's special pattern

The agent ships as a desktop binary installed on user machines. At runtime it does not sit inside a repo, does not have a `.env` file to pick up, and gets env via the OS service manager (launchd on macOS, systemd on Linux, Service Control Manager on Windows). Its boot loader is correspondingly different.

**Differences from the server pattern:**

- **No `bootenv` call.** `packages/agent/cmd/agent/main.go` does not call `bootenv.LoadFromRepoRoot`. The agent reads env vars directly via `os.Getenv` at the call sites that need them; there is no `.env` file convenience layer.
- **Subcommand routing.** The agent's `main()` dispatches to subcommands (e.g. `run`, `enroll`); `cmdRun(args)` is the one that loads config and starts the daemon.
- **Default flag value is `agent.yaml`, not `agent.config.yaml`.** `cmd_run.go` `cmdRun` declares `fs.String("config", "agent.yaml", ...)`. The repo carries `packages/agent/agent.config.yaml` (template / reference) and `packages/agent/agent.dev.yaml` (local dev); the agent installer is responsible for writing a concrete `agent.yaml` into the install directory.
- **Loader is `LoadFromFile(path)`, not `Load(path) + applyEnvOverrides`.** Source: `packages/agent/internal/sync/schema/config.go` `LoadFromFile`. The sequence is:
  ```
  1. data, err := os.ReadFile(path)
     → missing-file IS an error (no defaults fallback). The agent
       refuses to start without a yaml.
  2. yaml.Unmarshal(data, &cfg)
  3. if cfg.HubHTTPURL == "" → error.
     → the only enforced required field at this layer.
  4. applyDefaults(&cfg)
     → fill in defaults AFTER the yaml has been unmarshalled. The
       opposite ordering from the server pattern, where defaults are
       the starting point.
  ```
- **No env-override step.** The agent has no `applyEnvOverrides`. Once the yaml is loaded and defaults are filled, the config is frozen for the duration of the process. Runtime config changes flow through the Cat A / Cat B configKey mechanism (A01 §5 + A02), not through env or yaml reloads.

The agent's pattern reflects its deployment shape: a single yaml is what the installer wrote, the user does not edit it, and everything mutable at runtime comes from Hub via the configloader path. The server pattern reflects its deployment shape: ops controls the env, yaml is a deploy artifact, both are subject to change between deploys.

## References

- Server `main()` entry points — `packages/nexus-hub/cmd/nexus-hub/main.go`, `packages/control-plane/cmd/control-plane/main.go`, `packages/ai-gateway/cmd/ai-gateway/main.go`, `packages/compliance-proxy/cmd/compliance-proxy/main.go`
- Server `config.Load(path)` implementations — `packages/nexus-hub/internal/config/config.go`, `packages/control-plane/cmd/control-plane/config/config.go`, `packages/ai-gateway/internal/config/config.go`, `packages/compliance-proxy/cmd/compliance-proxy/config/config.go`
- Env loader (.env file) — `packages/shared/core/bootenv/bootenv.go`
- Agent `main()` + subcommand — `packages/agent/cmd/agent/main.go`, `packages/agent/cmd/agent/cmd_run.go`
- Agent yaml loader — `packages/agent/internal/sync/schema/config.go`
- yaml files (per service) — `packages/<svc>/<svc>.config.yaml`, `packages/<svc>/<svc>.dev.yaml`
- env contract — `.env.example` (repo root)
- Layer model + R1-R5 invariants + 14-layer rename — `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` (A03)
- Runtime config flow (Cat A / Cat B configKeys) — `docs/developers/architecture/cross-cutting/foundation/thing-model.md` (A01) + `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` (A02)
