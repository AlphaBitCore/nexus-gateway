---
doc: configuration-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Configuration Architecture

> **Status**: Live spec. Migration PR-0 through PR-9 have landed; the drift-reconciler watch set now includes `cache`, `agent_settings`, `killswitch` (compliance-proxy + agent), and `gateway_passthrough` — see `packages/control-plane/cmd/control-plane/wiring/reconcile.go:58-117`. See `configuration-architecture-migration.md` §"Per-PR landed status" for the authoritative per-PR table with code-anchored evidence. The 4-layer model + R1-R5 invariants + per-key catalog below describe the current behavior of the system.
> **Owner**: nexus
> **Last reviewed**: 2026-05-21
> **Prior art**: Replaces the ad-hoc layering across yaml, env, and `thing_config_template`. Resolved 12+ silent breakages and 9 dead-code paths identified by the 2026-05-19 full-system config audit.

---

## 1. Goals & Non-Goals

### Goals

1. **Single source of truth (SoT) per concept.** Every configuration concept lives in exactly one storage layer. No double-writes, no silent merges.
2. **Authority-aligned layering.** Each layer is owned by exactly one role (developer / SRE / admin). The UI a value is edited from matches the layer it lives in.
3. **Hub-pushed runtime tuning** for true admin policy; **yaml/env** for everything else.
4. **Strict typed schemas** for `thing_config_template` state JSON, enforced at startup audit and at write time.
5. **Symmetric Thing model.** All five services (Hub, CP, AI Gateway, Compliance Proxy, Agent) are Things; Hub self-registers via PostgreSQL `LISTEN/NOTIFY` because it cannot call its own WebSocket.

### Non-Goals

* Backwards compatibility for legacy config_key names. Pre-GA per `CLAUDE.md`; renames happen in-place with a single coordinated migration.
* Support for a "Settings" mega-page that bundles every knob. Admin UI surfaces are organised by domain (Providers, Hooks, Cache, …); the `device-defaults` shape is the exception, not the rule.
* Refactoring of `system_metadata` table semantics. We continue using it as the keyed JSON SoT for Type B keys.

---

## 2. The Five Storage Layers

Configuration data lives in exactly one of these five stores. Layers are listed top-down by **who can change the value** (authority) and **when** (boot vs. runtime).

| # | Store | Owner | Mutability | Used For |
|---|---|---|---|---|
| 1 | **Code defaults** | Developer (compile-time) | Release-only | Hardcoded sane initial values (HTTP keep-alive, retry counts, buffer sizes). |
| 2 | **yaml** (`packages/<svc>/<svc>.{config,dev,prod.yaml.example}.yaml`) | SRE (declarative, git-committed) | Restart-only | Service shape — ports, retention days, CORS allowlist, S3 bucket, forward-header rules, upstream timeouts, IP access-control. |
| 3 | **env** (`.env` for dev, `systemd EnvironmentFile=` for prod) | SRE (injected) | Restart-only | Secrets + environment-specific URLs (DATABASE_URL, REDIS_URL, NEXUS_HUB_URL, INTERNAL_SERVICE_TOKEN). |
| 4 | **`thing_config_template`** + **`thing_config_override`** (postgres) | Admin via Web UI | Runtime hot-push | Operational policies — virtual keys, routing rules, kill switch, AI Guard, payload capture, agent settings. |
| 5 | **`system_metadata`** (postgres key-value) | Admin via Web UI | Runtime + invalidation | Singleton domain settings whose data shape doesn't fit a template state. Used in conjunction with `thing_config_template` Type B invalidation channels. |

### Precedence

```
Boot-time:     L3 (env)  >  L2 (yaml)  >  L1 (code default)
Run-time:      L4 (template / override push) overrides matching fields at L1-L3 for keys it owns
               L5 (system_metadata) provides the data body that Type B template invalidations point to
```

### Five Invariants (binding, no waiver without architecture-doc PR)

| # | Rule | Enforcement |
|---|---|---|
| **R1** | A concept lives in **exactly one** of L1-L5. No double-write across layers. | Startup audit + schema registry at `packages/shared/schemas/configkey/`. |
| **R2** | Secrets are L3 only. Never appear in yaml or template state. | Existing `CLAUDE.md` "Secrets are env-only" binding + lint. |
| **R3** | A template (L4) key must have an admin UI **or** a publisher in CP/Hub. Orphan rows are forbidden. | Per-key Verdict column in §7 catalog; CI lint at startup. |
| **R4** | A template (L4) key must have a registered receiver in the target service. Orphan publishers are forbidden. | Startup audit cross-references `cfgloader.Register*` against `ValidByThingType`. |
| **R5** | yaml (L2) fallback values must be sufficient for cold-start. L4 push is an enhancement, not a precondition. | Service boot must succeed with no Hub connection; smoke-tested via `dev-start.sh`. |

---

## 3. Type A vs Type B Template Keys

The wire protocol does **not** distinguish Type A from Type B (`hubMessage` envelope is identical — see `packages/shared/transport/thingclient/client.go:226-239`). The distinction is purely a **receiver-side convention**.

### Type A — Config Blob

The state JSON **is** the configuration.

- Publisher (CP) calls `hub.NotifyConfigChange(thingType, key, state)` with full state body.
- Hub upserts `thing_config_template` row with `state = body` and bumps `desiredVer`.
- WebSocket sends a change-signal envelope `{type:"config_changed", configKey, desiredVer}` to clients — the **state body itself is not pushed** over WS. Per the binding pull-only model (CLAUDE.md "Current state" + `thing-model.md:39`), Things PULL the state from Hub on receipt of the signal.
- Receiver fetches the latest state via the Hub Cat-A/B pull endpoint, then `json.Unmarshal` into a typed Go struct and applies it.

Examples: `killswitch`, `log_level`, `cache`, `gateway_passthrough`, `agent_settings`, `onboarding`. `payload_capture` is Type A on agent (state carries `{enabled: bool}`) but Type B elsewhere; `ai_guard`, `observability`, `diag_mode` are exclusively Type B (see §7).

### Type B — Invalidation Trigger

The state JSON is **always** null or `{}`. The real data lives in a dedicated SQL table.

- Publisher (CP) writes the canonical data to a SQL table (`Provider`, `VirtualKey`, `Hook`, `ComplianceExemptionGrant`, `system_metadata['<key>.config']`, …) and calls `hub.InvalidateConfig(thingType, key)`.
- Hub bumps the template's `version` (state stays null/`{}`) and pushes `{type:"config_changed", configKey, state:null|{}, desiredVer}`.
- Receiver **ignores** the bytes; either (a) reloads its in-process cache from a Hub Cat-B HTTP pull endpoint, or (b) reads `system_metadata` directly.

Examples: `providers`, `models`, `credentials`, `virtual_keys`, `routing_rules`, `quota_policies`, `quota_overrides`, `organizations`, `interception_domains`, `hooks`, `exemptions`, `payload_capture` (receiver-side), `streaming_compliance`, `observability`, `credential_reliability`.

### Hybrid — Type B with payload

`virtual_keys` carries a structured invalidation payload `{op:"invalidate", ids:[...]}` for targeted hash-based purge. State is meaningful but signal-shaped, not authoritative config. Documented as Type B with a payload spec (see §7).

### Hub self-shadow (special case)

`nexus-hub` cannot call its own WebSocket endpoints. Instead, Hub self-registers as a Thing (`packages/nexus-hub/internal/self/reg/selfreg.go`) and consumes its own `thing.desired` via PostgreSQL `LISTEN config_changed` (`packages/nexus-hub/internal/self/shadow/manager.go`). The keys (`nexus-hub.log_level`, `nexus-hub.observability`) are otherwise identical to other Type A keys.

---

## 4. Override Mechanism

Per-instance overrides are stored in `thing_config_override` and **merged server-side** before push.

```
CP API:     PUT /api/admin/nodes/:id/overrides/:configKey
            → packages/control-plane/internal/infrastructure/infra/thing_overrides.go:50,335-383
Hub API:    PUT /api/hub/things/:id/overrides/:configKey
            → packages/nexus-hub/internal/fleet/handler/hubapi/hub_api_overrides.go:147-202
Merge:      packages/nexus-hub/internal/fleet/manager/override.go:340-412 (recomputeDesiredTx)
            templates ⊕ overrides → thing.desired (atomic, in tx)
            then bumps desired_ver and RePushConfigKey via WebSocket
```

**Implications**:
1. Receivers never see the override layer separately — they receive the merged `thing.desired` state.
2. An override row is meaningless without its template row. Hub returns `ErrTemplateMissing` (override.go:98-104) if you try to create an override for a key with no template.
3. **Therefore, when downgrading a key to yaml-only (e.g., `access_control`), the template row AND any existing overrides must be deleted atomically.**

Admin UI surface: `/infrastructure/overrides` (generic editor) + per-node detail page tabs.

---

## 5. The Drift Reconciler

Server-side mechanism that detects and re-pushes when `desired_ver != reported_ver`.

- **Detector**: `packages/nexus-hub/internal/jobs/defs/drift/drift.go:82-110` — scheduled job, finds drifted Things, calls `RePushConfig`.
- **Re-push**: `packages/nexus-hub/internal/fleet/manager/drift.go:29-103` — per-key delta via WebSocket if connected, else MQ HubSignal.
- **Retry**: Redis tracks `nexus:drift:retry:<thingID>`; 3 attempts within 5m before status flips to `drift`.
- **Hub excluded**: Hub uses LISTEN/NOTIFY for its own row; drift converges synchronously on apply, so no retry loop is needed.

CP-side **app-level reconciler** (`packages/control-plane/internal/platform/configreconcile/reconcile.go`, wired in `packages/control-plane/cmd/control-plane/wiring/reconcile.go:58-117`) watches a hard-coded list of high-value keys for content drift (not just version drift), running every 60s:
- `cache` (ai-gateway) — emergency-grade Category-A cache config blob.
- `agent_settings` (agent) — heartbeat / runtime tuning.
- `killswitch` (compliance-proxy + agent) — emergency brake; survives WS reconnect mid-toggle.
- `gateway_passthrough` (ai-gateway) — emergency passthrough toggle, same class as `killswitch`.

The dead `gateway_settings` watch entry was removed in PR-0. `ai_guard` is not watched at the content-drift layer; it falls back to the version-drift detector.

---

## 6. Naming Conventions

Every config concept has a single canonical name that transforms deterministically across all four representations:

| Representation | Convention | Example |
|---|---|---|
| **yaml field** | camelCase | `forwardHeaders`, `nexusHubUrl`, `accessControl` |
| **env variable** | SCREAMING_SNAKE_CASE | `NEXUS_HUB_URL`, `INTERNAL_SERVICE_TOKEN` |
| **configKey** (template) | snake_case, **no suffix** | `cache`, `ai_guard`, `gateway_passthrough`, `hooks`, `exemptions` |
| **Go struct field** | PascalCase | `Registry.NexusHubURL`, `AccessControl.SourceIPAllowlist` |

### env namespacing rules

| Category | Rule | Example |
|---|---|---|
| Shared infrastructure URL | bare name (no service prefix) | `DATABASE_URL`, `REDIS_URL`, `NATS_URL`, `NEXUS_HUB_URL`, `AI_GATEWAY_URL`, `COMPLIANCE_PROXY_URL`, **`AUTH_SERVER_URL`**, **`AUTH_SERVER_JWKS_URL`** |
| [MUST MATCH] shared values | bare name | `INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`, `COMPLIANCE_PROXY_API_TOKEN`, **`AUTH_SERVER_ISSUER`** |
| Service-private port | `<SVC>_PORT` | `NEXUS_HUB_PORT`, `AI_GATEWAY_PORT`, `COMPLIANCE_PROXY_PORT`, `CONTROL_PLANE_PORT` |
| Service-private API token | `<SVC>_API_TOKEN` | `AI_GATEWAY_API_TOKEN`, `COMPLIANCE_PROXY_API_TOKEN` |
| Service-private operational knob | `<SVC>_<KNOB>` | `NEXUS_HUB_ID`, `NEXUS_HUB_ADVERTISE_ADDR`, `NEXUS_HUB_SCHEDULER_ENABLED`, `NEXUS_HUB_AGENTCA_DIR` |
| Cross-cutting | bare name | `LOG_LEVEL`, `LOG_FORMAT`, `OTEL_ENDPOINT`, `MQ_DRIVER` |

**Service-prefix rule of thumb**: prefix when the variable describes the service's *own* private state (its port, its ID, its private API token). NO prefix when the variable describes a shared environment-level entity that any service can connect to (the Hub, the database, the auth server). The value of `AUTH_SERVER_URL` is "the auth server's URL", not "Hub's view of CP" — same shape as `NEXUS_HUB_URL` ("the Hub's URL", consumed by any service that needs Hub).

### Renames executed in this migration

Status column: **LANDED** = renamed name present in `tools/db-migrate/seed/data/seed-baseline.sql` and the live `packages/shared/schemas/configkey/configkey.go` constants. **PENDING** = decision outstanding.

**configKey renames (L4 template layer):**

| From | To | Reason | Status |
|---|---|---|---|
| `aiguard_config` | `ai_guard` | Drop `_config` suffix; snake_case canonical | LANDED |
| `cache_config` | `cache` | Drop `_config` suffix | LANDED |
| `hook_config` | `hooks` | Drop `_config` suffix; plural for the collection | LANDED |
| `gateway_passthrough_config` | `gateway_passthrough` | Drop `_config` suffix | LANDED |
| `active_exemptions` | `exemptions` | Align with agent's existing `exemptions`; the "active" qualifier doesn't add information | LANDED |
| `agentUpdateTarget` (const) | `agent_update_target` | snake_case (currently camelCase outlier) | PENDING — decision deferred (open question #2 in §12) |
| `siem.config` (const) | `siem` | Drop dotted/suffix form | LANDED (`configkey.SIEM`) |
| `streaming_compliance.config` (const) | `streaming_compliance` | Align with template row name | LANDED |
| `payload_capture.config` (const) | `payload_capture` | Same | LANDED |
| `gateway.credential_reliability.config` (const) | `credential_reliability` | Drop dotted prefix (service implied by Thing type) | LANDED |

**yaml renames (L2 layer):**

| Service | From | To | Reason |
|---|---|---|---|
| compliance-proxy | `logging:` | `log:` | Align with other 3 services |
| compliance-proxy | `redis.address` | `redis.addrs` | Universal Redis schema (§9); also fix `address` vs `addr` spelling drift |
| compliance-proxy | `audit.database.dsn` | `database.url` (top-level) | DSN vs URL drift; move out of audit nesting |
| ai-gateway | `server.readTimeoutSec` (int) | `server.readTimeout` (time.Duration) | Type uniformity |
| control-plane | `server.shutdownTimeout` (int) | `server.shutdownTimeout` (time.Duration) | Same |
| control-plane | `database.maxOpenConns/maxIdleConns/connMaxLifetime` (database/sql style) | `database.maxConns/minConns/maxConnLifetime` (pgxpool style) | Adopt pgxpool naming uniformly (matches Hub) |
| control-plane | `bff.nexusHubUrl` | `registry.nexusHubUrl` | Move to top level — service registers as a Thing, parallels ai-gateway / compliance-proxy |
| nexus-hub | `auth.cpURL` | `authServer.url` | OAuth-standard, deployment-agnostic naming |
| nexus-hub | `auth.cpJWKSURL` | `authServer.jwksURL` | Same |
| nexus-hub | `auth.cpIssuer` | `authServer.issuer` | Same; CP-side mirror is `authServer.issuer` (already exists) |
| control-plane | `auth.bootstrapKey` | (DELETE — dead config) | Zero code references |

**env renames (L3 layer) — full sweep applying the rule "service prefix for service-private knobs; no prefix for entity-shaped infrastructure URLs and shared values":**

| From | To | Reason |
|---|---|---|
| `CONTROL_PLANE_URL` | `NEXUS_HUB_URL` | Value is Hub URL, not CP URL (misnomer since Hub split) |
| `PORT` (CP) | `CONTROL_PLANE_PORT` | Service-private port — add prefix for symmetry |
| `NEXUS_HUB_CP_URL` | `AUTH_SERVER_URL` | Entity-shaped (`<entity>_URL`); drop deployment-specific "CP" and service prefix "NEXUS_HUB_" |
| `NEXUS_HUB_CP_JWKS_URL` | `AUTH_SERVER_JWKS_URL` | Same |
| `NEXUS_HUB_CP_ISSUER` | `AUTH_SERVER_ISSUER` | Same; **becomes [MUST MATCH CP ↔ Hub]** |
| `NEXUS_HUB_AGENTCA_DIR` | `AGENT_CA_DIR` | "agent CA" is shared PKI entity; CP already uses unprefixed name — merge Hub onto it |
| `NEXUS_HUB_AGENTCA_CERT_FILE` | `AGENT_CA_CERT_FILE` | Same |
| `NEXUS_HUB_AGENTCA_KEY_FILE` | `AGENT_CA_KEY_FILE` | Same |
| `CACHE_ENABLED` | `AI_GATEWAY_CACHE_ENABLED` | ai-gateway-private operational knob — add prefix to avoid `.env.example` ambiguity |
| `CACHE_TTL` | `AI_GATEWAY_CACHE_TTL` | Same |
| `CACHE_PREFIX` | `AI_GATEWAY_CACHE_PREFIX` | Same |
| `CORS_ENABLED` | `AI_GATEWAY_CORS_ENABLED` | Same |
| `CORS_ALLOWED_ORIGINS` | `AI_GATEWAY_CORS_ALLOWED_ORIGINS` | Same |
| `CRYPTO_PRODUCTION` | `CONTROL_PLANE_CRYPTO_PRODUCTION` | CP-private feature flag |
| `AI_GATEWAY_BASE_URL` | `AI_GATEWAY_URL` | Consolidate the simulator handler's alias onto the canonical URL name |
| `REDIS_URL` | (deleted — replaced by `REDIS_MODE` + `REDIS_ADDRS` + …) | Universal Redis schema (§9) |
| `REDIS_ADDR` | (deleted — same) | Same; also fixes `addr` vs `address` yaml drift |

**Auth-server fields — renamed to OAuth-standard, deployment-agnostic naming**:

The "cp" prefix in both yaml and env hard-coded a deployment assumption — that CP happens to be running the auth server today. We remove that assumption: these fields describe a single OAuth-standard "authorization server" entity, regardless of which process runs it. CP already uses `AUTH_SERVER_*` env vars for the issuer side; Hub adopts the same prefix for the verifier side, creating a natural `[MUST MATCH CP ↔ Hub]` env relationship.

| Old (Hub yaml) | Old (env) | New (Hub yaml) | New (env) |
|---|---|---|---|
| `auth.cpURL` | `NEXUS_HUB_CP_URL` | `authServer.url` | **`AUTH_SERVER_URL`** |
| `auth.cpJWKSURL` | `NEXUS_HUB_CP_JWKS_URL` | `authServer.jwksURL` | **`AUTH_SERVER_JWKS_URL`** |
| `auth.cpIssuer` | `NEXUS_HUB_CP_ISSUER` | `authServer.issuer` | **`AUTH_SERVER_ISSUER`** [MUST MATCH CP] |

`AUTH_SERVER_ISSUER` becomes a [MUST MATCH] env variable: CP writes it as the `iss` claim in JWTs it issues; Hub uses it as the expected `iss` value when verifying those JWTs. The two values must equal for verification to succeed — the same pattern as `INTERNAL_SERVICE_TOKEN`.

`AUTH_SERVER_KEYSTORE_DIR` (CP-only — private key storage) stays as-is. It's not shared with Hub.

yaml field symmetry:
- CP: `authServer.{issuer, keystoreDir}` — issuer-side (signs JWTs, owns keystore)
- Hub: `authServer.{url, jwksURL, issuer}` — verifier-side (verifies JWTs, holds discovery URLs)

`auth.internalServiceToken` (Hub) remains under `auth:` — that one truly is about inter-service token authentication, not OAuth.

---

## 6.5 BINDING — Rename Sweep Discipline

> **Every rename (yaml field / env variable / configKey / Go struct field) is touching at minimum FOUR layers simultaneously. Half-completing a rename leaves the system in an inconsistent state where one half reads the old name and the other half reads the new name — silent breakage by construction. Every rename MUST sweep all four layers in the same PR.**

### The Four-Layer Sweep Checklist

For EACH rename, before committing the PR, verify all of these layers have been updated:

| # | Layer | What to grep for | Example |
|---|---|---|---|
| 1 | **Go source code** | string literal of OLD name | `grep -rE '"CACHE_ENABLED"\|"cache_config"' packages/ --include="*.go" \| grep -v "_test.go"` |
| 2 | **Go tests** | same — yes update tests too | `grep -rE '"CACHE_ENABLED"' packages/ --include="*_test.go"` |
| 3 | **yaml files** | yaml field/key | `grep -rE 'CACHE_ENABLED:\|cache_config:' packages/ --include="*.yaml"` |
| 4 | **`.env.example` + `tests/.env.<target>.example`** | env var declaration | `grep -E '^CACHE_ENABLED=' .env.example tests/.env.*.example` |
| 5 | **DB / seed-baseline.sql** | configKey literal in INSERT/UPDATE | `grep -E "'cache_config'" tools/db-migrate/seed/data/*.sql` |
| 6 | **DB migration scripts** (if any new migration creates rows) | configKey literal | `grep -rE "'cache_config'" tools/db-migrate/migrations/` |
| 7 | **admin UI source** (React/TS) | configKey or env name in `.tsx`/`.ts` | `grep -rE "'cache_config'\|cache_config" packages/control-plane-ui/src/ --include="*.tsx" --include="*.ts"` |
| 8 | **admin UI i18n locales** | translation key references | `grep -rE 'cache_config' packages/control-plane-ui/{public,src}/i18n/` packages/control-plane-ui/{public,src}/locales/` |
| 9 | **prod systemd EnvironmentFile** | env declaration (SSH-only check) | `ssh $HOST 'sudo grep -E "^CACHE_ENABLED=" /etc/nexus-gateway/env'` |
| 10 | **prod DB rows** (if configKey rename) | template + override rows | `ssh $HOST 'psql -c "SELECT * FROM thing_config_template WHERE config_key = '"'"'cache_config'"'"';"'` |
| 11 | **`docs/developers/architecture/*.md`** | any doc referencing old name | `grep -rE 'cache_config\|CACHE_ENABLED' docs/` |
| 12 | **`.claude/skills/*.md`** | skill doc references | `grep -rE 'cache_config\|CACHE_ENABLED' .claude/skills/` |
| 13 | **`CLAUDE.md`** + `.cursor/rules/*.mdc` | rule references | `grep -rE 'cache_config\|CACHE_ENABLED' CLAUDE.md .cursor/rules/` |
| 14 | **Test fixtures + scripts** | `tests/scripts/*` and `tests/fixtures/*` | `grep -rE 'cache_config\|CACHE_ENABLED' tests/` |

### After the sweep — final verification

Run a single negative grep that should return ZERO matches:

```bash
# For each renamed identifier, after sweep:
OLD="cache_config"   # or CACHE_ENABLED, etc.
git grep -E "\b${OLD}\b" -- ':!docs/developers/architecture/configuration-architecture*.md'
# The architecture docs are allowed to mention the old name (in rename tables).
# Everywhere else: ZERO matches.
```

If anything remains, the rename is incomplete — DO NOT merge the PR.

### What goes wrong if you skip this

- **Skipped layer 1 (Go source)**: service boots reading old env name, gets empty value, falls back to yaml default OR crashes at startup audit.
- **Skipped layer 4 (.env.example)**: next operator who reads the example file uses the wrong name; their service silently misbehaves.
- **Skipped layer 5 (seed)**: fresh `npx prisma db seed` produces a DB with the old configKey; service receives push for unknown key.
- **Skipped layer 7 (UI source)**: admin UI calls a 404 API endpoint OR sends mismatched configKey in WS history queries.
- **Skipped layer 8 (i18n)**: 缺少 translation falls back to key string → UI shows raw `pages:cache.title` text.
- **Skipped layer 9 (prod EnvironmentFile)**: deployed binary reads new name, doesn't find it, falls back to yaml — prod loses the env-driven override (most painful for secrets).
- **Skipped layer 10 (prod DB)**: existing prod template rows with old configKey become orphans; new pushes write new key; admin UI shows two of "the same thing" with conflicting state.

### Per-rename pre-flight script

For each rename, before committing, run:

```bash
#!/usr/bin/env bash
# Save as scripts/check-rename.sh
OLD="$1"
NEW="$2"

echo "=== Layer 1-2: Go source (excluding rename tables in docs) ==="
git grep -E "\b${OLD}\b" -- '*.go' | grep -v "_test.go"
git grep -E "\b${OLD}\b" -- '*_test.go'

echo "=== Layer 3: yaml ==="
git grep -E "\b${OLD}\b" -- '*.yaml' '*.yml'

echo "=== Layer 4: env examples ==="
git grep -E "\b${OLD}\b" -- '.env.example' 'tests/.env.*.example'

echo "=== Layer 5-6: SQL + migrations ==="
git grep -E "\b${OLD}\b" -- 'tools/db-migrate/'

echo "=== Layer 7-8: UI source + locales ==="
git grep -E "\b${OLD}\b" -- 'packages/control-plane-ui/src/' 'packages/control-plane-ui/public/' 'packages/agent/' '*.tsx' '*.ts' '*.json'

echo "=== Layer 11-13: docs + skills + rules ==="
git grep -E "\b${OLD}\b" -- 'docs/' '.claude/skills/' '.cursor/rules/' 'CLAUDE.md' ':!docs/developers/architecture/configuration-architecture*.md'

echo "=== Layer 14: test fixtures + scripts ==="
git grep -E "\b${OLD}\b" -- 'tests/'

echo "=== EXPECTED: zero matches above (excluding rename tables in arch docs) ==="
```

Run for every rename:

```bash
./scripts/check-rename.sh CACHE_ENABLED AI_GATEWAY_CACHE_ENABLED
./scripts/check-rename.sh NEXUS_HUB_CP_URL AUTH_SERVER_URL
./scripts/check-rename.sh NEXUS_HUB_AGENTCA_DIR AGENT_CA_DIR
./scripts/check-rename.sh PORT CONTROL_PLANE_PORT   # careful: PORT is short, may match unrelated code — review manually
./scripts/check-rename.sh aiguard_config ai_guard
./scripts/check-rename.sh cache_config cache
./scripts/check-rename.sh hook_config hooks
./scripts/check-rename.sh gateway_passthrough_config gateway_passthrough
./scripts/check-rename.sh active_exemptions exemptions
./scripts/check-rename.sh agentUpdateTarget agent_update_target
# ... etc, one invocation per rename in §6.
```

For short or generic names (`PORT`, `cache`, `hooks`), use word-boundary matching carefully and review results by hand — false positives are likely.

### Where false positives are OK

Two acceptable kinds of remaining references:

1. **`docs/developers/architecture/configuration-architecture*.md`** — these docs explicitly document the rename in "From / To" tables.
2. **`CHANGELOG.md` / git commit messages** — historical record.

All other matches must be cleaned up.

---

## 7. Complete Per-Key Catalog

Live catalog of keys present in `packages/shared/schemas/configkey/configkey.go` + `ValidByThingType` (`validation.go`). The 2026-05-19 audit identified 48 prod rows; renames + deletes + downgrades + new keys (E61 / E72) have landed since, leaving the current set below. Verdict column reflects current behavior in code, not pending work.

Verdict legend: **KEEP_A** (working Type A — receiver consumes the state blob directly), **KEEP_B** (working Type B — receiver ignores state bytes, reloads from SQL or `system_metadata`).

### 7.1 ai-gateway (19 keys)

| configKey | Type | Notes |
|---|---|---|
| `log_level` | A | Working A. Per-Thing slog level. Prod override path valid. |
| `observability` | B | Receiver re-reads `system_metadata['observability.config']`; template state is `null`. |
| `cache` | A | True Type A — receiver consumes blob (`packages/shared/storage/cacheconfig`). Drift reconciler watches it. UI: `/ai-gateway/prompt-cache`. |
| `ai_guard` | B | CP calls `InvalidateConfig`; receiver reloads. UI: `/compliance/ai-guard`. |
| `gateway_passthrough` | A | True Type A. Fail-closed cold-start. Emergency-grade. UI: `/ai-gateway/passthrough`. |
| `payload_capture` | B | Receiver re-reads `system_metadata['payload_capture.config']`. Template state is `null`. |
| `credential_reliability` | B | Real data in `system_metadata['gateway.credential_reliability.config']`. UI: per-credential tab. |
| `providers` | B | Clean B. UI: `/ai-gateway/providers`. |
| `models` | B | Triad with providers/credentials. UI: under `/ai-gateway/providers/:id`. |
| `credentials` | B | Data in `Credential` table. UI: `/ai-gateway/credentials`. |
| `routing_rules` | B | Most active B. UI: `/ai-gateway/routing`. |
| `virtual_keys` | B (with payload) | Structured invalidation `{op:"invalidate", ids:[...]}` for targeted hash purge. |
| `quota_policies` | B | Triad sibling. UI: `/ai-gateway/quota-policies`. |
| `quota_overrides` | B | Triad sibling. UI: `/ai-gateway/quota-overrides`. |
| `organizations` | B | Triad with quota_*. PolicyCache reload. UI: `/iam/organizations`. |
| `hooks` | B | Receiver reloads from `Hook` table. UI: `/compliance/hooks`. |
| `response_cache.time_sensitive_patterns` | A (E61-S2) | Cluster-wide list of co-occurrence patterns pushed from Hub to every ai-gateway Thing. When any rule fires on an incoming prompt, both L1 and L2 cache tiers skip lookup AND write. Receiver swaps `freshness.Detector` atomically on each push. See `response-cache-architecture.md` §4.1. |
| `semantic_cache.config` | A (E61-S3) | Fleet-wide L1 embedding singleton config blob (embedding provider/model/dimension/fingerprint, Redis index name, enabled). On fingerprint change, receiver emits the `semantic-cache-reindex` job. See `response-cache-architecture.md` §3.5. |
| `response_cache.extract_config` | A (E72) | Fleet-wide L1 extract (exact-match) cache singleton config (`enabled`, `ttlSeconds`, `applyFreshnessRules`). Receiver (`registerAGExtractCacheConfig`) hot-swaps `cache.Cache` via `atomic.Pointer` so admins can disable cache or stop freshness rules from firing without a service restart. |

### 7.2 compliance-proxy (9 keys)

| configKey | Type | Notes |
|---|---|---|
| `log_level` | A | Working A. Override path valid. |
| `observability` | B | Receiver re-reads `system_metadata['observability.config']`. Template state `null`. |
| `killswitch` | A | True Type A. Shape `interception.Killswitch{Enabled bool}`. UI: `/infrastructure/kill-switch`. |
| `onboarding` | A | Per-instance Type A toggle. UI: `/infrastructure/proxy-rollout`. |
| `payload_capture` | B | Receiver re-reads `system_metadata['payload_capture.config']`. Template state `null`. |
| `streaming_compliance` | B | Receiver registered in compliance-proxy `configdispatch` (PR-6). Reloads policy snapshot consumed by the data plane. |
| `interception_domains` | B | Canonical Type B; reloads `InterceptionDomain` SQL. UI: `/compliance/interception-domains`. |
| `hooks` | B | Clean B; reloads `Hook` table. |
| `exemptions` | A (projected snapshot) | Cannot become Type B — compliance-proxy has no DB access. CP projects from `ComplianceExemptionGrant` table and pushes. Atomic swap. UI: `/compliance/exemptions`. |

`access_control` was downgraded to yaml (`accessControl:`); `domain_allowlist` and `compliance_streaming` were deleted as dead code (PR-0).

### 7.3 agent (10 keys)

| configKey | Type | Notes |
|---|---|---|
| `agent_settings` | A | Cat A blob; UI: `/devices/device-defaults`. E60 added `attestationEnabled` (bool, fleet-wide opt-in for agent traffic attestation) and reserves `complianceProxyUrl` (lenient-parse so older agents tolerate absence). |
| `diag_mode` | B | Canonical nudge-only pattern. Real data in `thing.metadata.diagModeUntil` + `thing_diag_mode_window`. UI: `/infrastructure/diag-mode`. |
| `exemptions` | B (catbagent-pulled) | Cat B Hub-pulled via the catbagent loader; backed by `ComplianceExemptionGrant` projection. UI: `/compliance/exemptions` (shared with compliance-proxy). |
| `hooks` | B | Catbagent-pulled. |
| `interception_domains` | B | Catbagent-pulled. |
| `payload_capture` | B | Catbagent-pulled. |
| `streaming_compliance` | B | Catbagent-pulled. |
| `killswitch` | A | **Safety-critical** — only fleet-wide way to disengage TLS bumping. Schema `interception.Killswitch{Enabled bool}` aligned with compliance-proxy. UI: `/infrastructure/kill-switch` (shared with compliance-proxy). |
| `installed_rule_packs` | B (catbagent-pulled) | Agent's installed rule pack snapshot invalidation. |
| `user_context` | B (catbagent-pulled) | Agent's per-device user context snapshot invalidation. |

`auth`, `log_level`, `observability`, `policy_rules`, `timing_intervals` were deleted as dead code (PR-0). `agentUpdateTarget` rename + auto-updater build-out is open question #2 in §12.

### 7.4 control-plane (2 keys)

| configKey | Type | Notes |
|---|---|---|
| `log_level` | A | Working Type A; override path valid. |
| `observability` | B | Receiver re-reads `system_metadata['observability.config']`. Template state `null`. |

### 7.5 nexus-hub (2 keys)

| configKey | Type | Notes |
|---|---|---|
| `log_level` | A | Hub self-registers via `selfreg.New`; selfshadow consumes via PG `LISTEN/NOTIFY`. |
| `observability` | B | Same fan-out + re-read path as the other services. CP fan-out includes nexus-hub. |

### 7.6 Roll-up (post-PR-0..PR-9)

Live key counts mirror `ValidByThingType` in `packages/shared/schemas/configkey/validation.go`:

| Thing type | Key count | Source |
|---|---|---|
| `nexus-hub` | 2 | `LogLevel`, `Observability` |
| `control-plane` | 2 | `LogLevel`, `Observability` |
| `ai-gateway` | 19 | see §7.1 |
| `compliance-proxy` | 9 | see §7.2 |
| `agent` | 10 | see §7.3 (`agent_settings`, `diag_mode`, `exemptions`, `hooks`, `interception_domains`, `payload_capture`, `streaming_compliance`, `killswitch`, `installed_rule_packs`, `user_context`) |
| **Total live keys** | **42** | sum of above (some keys appear under multiple Thing types) |

Updates relative to the 2026-05-19 baseline (48 rows):
- DELETED: `domain_allowlist`, `compliance_streaming`, ai-gw `streaming_compliance`, agent `auth` / `log_level` / `observability` / `policy_rules` / `timing_intervals`.
- DOWNGRADED to yaml: `forward_headers_config`, `upstream_timeouts`, `access_control`.
- RENAMED (key value): `aiguard_config → ai_guard`, `cache_config → cache`, `hook_config → hooks`, `gateway_passthrough_config → gateway_passthrough`, `active_exemptions → exemptions`.
- ADDED (post-baseline): ai-gateway `response_cache.time_sensitive_patterns`, `semantic_cache.config`, `response_cache.extract_config` (E61 / E72); agent `installed_rule_packs`, `user_context` (catbagent Cat B).

---

## 8. Schema Registry

Every key (Type A and Type B) is exported as a constant in `packages/shared/schemas/configkey/`. Startup audit (`configkey.AuditTemplateRows`) cross-references DB rows against `ValidByThingType`; unknown `(type, key)` tuples emit a WARN.

The audit returns 2 values — `([]OrphanRow, error)` — matching the live signature in `validation.go`:

```go
// packages/shared/schemas/configkey/validation.go
func AuditTemplateRows(ctx context.Context, db DBScanner) ([]OrphanRow, error) {
    rows, err := db.Query(ctx, "SELECT type, config_key FROM thing_config_template")
    if err != nil {
        return nil, fmt.Errorf("query thing_config_template: %w", err)
    }
    defer rows.Close()

    var orphans []OrphanRow
    for rows.Next() {
        var t, k string
        if err := rows.Scan(&t, &k); err != nil {
            return nil, fmt.Errorf("scan template row: %w", err)
        }
        if !isValid(t, k) {
            orphans = append(orphans, OrphanRow{Type: t, Key: k})
        }
    }
    return orphans, nil
}
```

### Live constants (`configkey.go`)

```go
package configkey

// Type A — state IS the configuration.
const (
    LogLevel                            = "log_level"
    Killswitch                          = "killswitch"           // {Enabled bool}
    AIGuard                             = "ai_guard"
    Cache                               = "cache"
    GatewayPassthrough                  = "gateway_passthrough"
    AgentSettings                       = "agent_settings"
    DiagMode                            = "diag_mode"
    Onboarding                          = "onboarding"
    PayloadCapture                      = "payload_capture"      // A on agent; Type B elsewhere (receiver re-reads system_metadata)
    Observability                       = "observability"        // Type B everywhere (receiver re-reads system_metadata)
    ResponseCacheTimeSensitivePatterns  = "response_cache.time_sensitive_patterns"  // E61-S2
    SemanticCacheConfig                 = "semantic_cache.config"                    // E61-S3
    ResponseCacheExtractConfig          = "response_cache.extract_config"            // E72
)

// Type B — invalidation trigger (state stays null/{}).
const (
    Providers             = "providers"
    Models                = "models"
    Credentials           = "credentials"
    RoutingRules          = "routing_rules"
    VirtualKeys           = "virtual_keys"            // B with structured payload {op, ids}
    QuotaPolicies         = "quota_policies"
    QuotaOverrides        = "quota_overrides"
    Organizations         = "organizations"
    InterceptionDomains   = "interception_domains"
    Hooks                 = "hooks"
    Exemptions            = "exemptions"
    StreamingCompliance   = "streaming_compliance"
    CredentialReliability = "credential_reliability"
    SIEM                  = "siem"
    InstalledRulePacks    = "installed_rule_packs"    // agent catbagent
    UserContext           = "user_context"            // agent catbagent
)
```

### Live `ValidByThingType` (`validation.go`)

```go
var ValidByThingType = map[string][]string{
    "nexus-hub":     {LogLevel, Observability},
    "control-plane": {LogLevel, Observability},
    "ai-gateway": {
        LogLevel, Observability, Cache, AIGuard, GatewayPassthrough,
        PayloadCapture, CredentialReliability,
        Providers, Models, Credentials, RoutingRules, VirtualKeys,
        QuotaPolicies, QuotaOverrides, Organizations, Hooks,
        ResponseCacheTimeSensitivePatterns, SemanticCacheConfig,
        ResponseCacheExtractConfig,
    },
    "compliance-proxy": {
        LogLevel, Observability, Killswitch, Onboarding,
        PayloadCapture, StreamingCompliance,
        InterceptionDomains, Hooks, Exemptions,
    },
    "agent": {
        AgentSettings, DiagMode, Exemptions, Hooks,
        InterceptionDomains, PayloadCapture, StreamingCompliance,
        Killswitch,
        InstalledRulePacks, UserContext,
    },
}
```

There are no `AccessControl` or `ComplianceStreaming` constants — `access_control` was downgraded to yaml, and the canonical streaming key is `StreamingCompliance` (a single key, applies to compliance-proxy and agent). The `TypedRegistry` (for unmarshalling Type A state into structs) is wired alongside `ValidByThingType`; see `packages/shared/schemas/configkey/typed.go` for the live mapping.

---

## 9. Redis — Universal Schema (Forward-Looking)

Support `standalone | sentinel | cluster` modes + ACL (Redis 6+) + TLS (incl. mTLS) in a single shape.

### yaml (4 services share identical shape)

```yaml
redis:
  mode: standalone        # standalone | sentinel | cluster
  addrs: ["localhost:6379"]
  username: ""             # ACL; old-Redis password-only flow leaves blank
  password: ""             # injected via env REDIS_PASSWORD; never literal in yaml
  db: 0                    # ignored in cluster mode

  sentinel:
    masterName: ""
    username: ""
    password: ""

  cluster:
    maxRedirects: 8
    routeRandomly: false
    readOnly: false        # read distribution to replicas

  tls:
    enabled: false
    insecureSkipVerify: false
    caFile: ""
    certFile: ""           # mTLS client cert
    keyFile: ""
    serverName: ""         # SNI

  poolSize: 10
  minIdleConns: 0
  maxRetries: 3
  dialTimeout: 5s
  readTimeout: 3s
  writeTimeout: 3s
  poolTimeout: 4s
```

### env mapping (every yaml field has a parallel env)

`REDIS_MODE`, `REDIS_ADDRS` (comma-separated), `REDIS_USERNAME`, `REDIS_PASSWORD`, `REDIS_DB`, `REDIS_SENTINEL_MASTER_NAME`, `REDIS_SENTINEL_USERNAME`, `REDIS_SENTINEL_PASSWORD`, `REDIS_CLUSTER_MAX_REDIRECTS`, `REDIS_CLUSTER_ROUTE_RANDOMLY`, `REDIS_CLUSTER_READ_ONLY`, `REDIS_TLS_*` (6 fields), `REDIS_POOL_SIZE`, `REDIS_MIN_IDLE_CONNS`, `REDIS_MAX_RETRIES`, `REDIS_DIAL_TIMEOUT`, `REDIS_READ_TIMEOUT`, `REDIS_WRITE_TIMEOUT`, `REDIS_POOL_TIMEOUT`.

### Implementation

New package `packages/shared/storage/redisfactory/` exposes:

```go
func New(yamlCfg Config, logger *slog.Logger) (redis.UniversalClient, error)
```

Internally constructs `redis.Options` / `redis.FailoverOptions` / `redis.ClusterOptions` from `mode` discriminator. Startup validation:
- `mode=sentinel` requires non-empty `sentinel.masterName`.
- `mode=cluster` requires `len(addrs) >= 1`.
- mTLS: `certFile` set ⇔ `keyFile` set.

### Deprecations

- env `REDIS_URL`, `REDIS_ADDR` — **deleted** (URL form can't cleanly express ACL + multi-addr).
- yaml `redis.url`, `redis.addr`, `redis.address` — **deleted** (three historical spellings).

---

## 10. Postgres / NATS (Forward-Looking, Conservative)

### Postgres

Keep single `DATABASE_URL`. Pool fields added to yaml uniformly:

```yaml
database:
  url: ""                        # env DATABASE_URL
  maxConns: 50                   # pgxpool naming
  minConns: 10
  maxConnLifetime: 30m
  maxConnIdleTime: 5m
  healthCheckPeriod: 1m
```

Drop CP's old `maxOpenConns/maxIdleConns/connMaxLifetime` (database/sql terminology). Defer read-replica support until a real need arises.

### NATS

Keep single `NATS_URL` (NATS clients support comma-separated multi-broker URLs natively). Defer credentials-file support until needed.

---

## 11. Acceptance Criteria

After all PRs land:

- [ ] `tools/db-migrate/seed/data/seed-baseline.sql` has ≤ 39 `thing_config_template` rows.
- [ ] All configKey literals come from `packages/shared/schemas/configkey/`.
- [ ] `grep -rE 'os.Getenv\("CONTROL_PLANE_URL"\)' packages/` returns zero.
- [ ] `grep -rE 'yaml:"logging"' packages/` returns zero.
- [ ] `grep -rE 'redis.url|redis.addr|redis.address' packages/*.yaml*` returns zero.
- [ ] All 4 `*.prod.yaml.example` have empty `database.url` / `redis.password` placeholder (no `CHANGE_ME_*`).
- [ ] `.env.example` documents every env var read by the 4 services + agent.
- [ ] Hub startup audit logs zero (type, key) pairs not in `ValidByThingType`.
- [ ] Hub startup audit logs zero registered receivers not in `ValidByThingType`.
- [ ] `prod-deploy/SKILL.md` Step 5.5 (env preflight) added.
- [ ] `prod-debug/SKILL.md` "env-var drift" failure pattern added.
- [ ] `CLAUDE.md` cites this doc under "Mandatory rules" as binding.
- [ ] `npm run check:all` green.
- [ ] `tests/scripts/smoke-gateway.py --all-ingress` green against migrated prod.

---

## 12. Open Decisions

Status column reflects what landed; see `configuration-architecture-migration.md` Decision log for the per-PR record.

| # | Question | Status |
|---|---|---|
| 1 | Replacement value for `compliance-proxy.access_control.sourceIpAllowlist` | RESOLVED — downgraded to yaml `accessControl:`; per-deployment value set by SRE. |
| 2 | `agentUpdateTarget`: delete or build out | **PENDING** — constant + read handler still present; no seed row + no admin write path. Will be resolved by a future agent self-update epic. |
| 3 | `agent.killswitch`: delete or fix | RESOLVED — must fix. Safety-critical per CLAUDE.md NE fail-open binding. Wire schema `interception.Killswitch{Enabled bool}` aligned across agent + compliance-proxy. |
| 4 | `agent.observability`: delete or fix | RESOLVED — deleted (PR-0). OTEL config remains yaml-only + standard OTEL env vars. |
| 5 | `agent.log_level`: delete or wire | RESOLVED — deleted (PR-0). log_level is env/yaml-tier, not admin-tier. |
| 6 | `agent.timing_intervals`: delete or wire | RESOLVED — deleted (PR-0); fields consolidated into `agent_settings` applier. |

---

## 13. Migration Plan — See `configuration-architecture-migration.md`

The PR sequence (PR-0 through PR-9), per-PR SQL scripts, code-touch lists, prod deployment ordering, and rollback procedures are documented in the sibling file `configuration-architecture-migration.md`.
