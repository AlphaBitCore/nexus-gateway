# Configuration architecture

## §1 — Scope

Every config field in Nexus Gateway lives in exactly one of four layers. This document defines the four layers, the five invariants (R1-R5) that govern which layer a field belongs in and how it is read / written / renamed, the 14-layer rename sweep discipline that keeps a rename from leaving a half-renamed field somewhere in the system, and a per-key catalog (§7) listing every configKey and `system_metadata` key currently in use.

CLAUDE.md's mandatory rule ("Configuration changes go through `configuration-architecture.md`") treats any PR that adds / removes / renames a yaml field, env variable, `thing_config_template` configKey, `system_metadata` key, or publisher / receiver wiring without conforming to this document as a binding violation that requires explicit user waiver.

Relationship to other foundation docs:
- **A01 (`thing-model.md`)** defines the L3 data model: the `thing` table, the `thing_config_template` / `thing_config_override` cascade, Type A vs Type B configKey semantics, the override blacklist, and the canonical Thing / shadow / desired / reported terminology.
- **A02 (`thing-config-sync-architecture.md`)** defines the L3 sync flow: how an admin write becomes a Thing's applied state through `manager.Manager.UpdateConfig` + `SetOverride`, NATS `nexus.hub.signal` cross-Hub fanout, `configloader.Loader` Thing-side apply, and `selfshadow.Manager` for Hub-self.

A03 sits at a different angle: it is the **layer model** — where each field belongs, what enforces it, and how to add or rename one safely.

## §2 — The four layers

```
L1: yaml (per-service)              L3: thing_config_template + thing_config_override
    packages/<svc>/<svc>.config.yaml    Hub-managed fleet config (Cat A blob + Cat B trigger)
    packages/<svc>/<svc>.dev.yaml       (data model: A01 §4; sync flow: A02)
    boot-time shape, non-secret

L2: env vars (process)              L4: system_metadata
    .env.example contract               Singleton runtime config (key/value rows)
    secrets + cross-service tokens      reloaded by polling DB; no push channel
    bootenv loader at startup
```

### Decision: which layer does a new field belong in?

| Question | If yes → |
|---|---|
| Does this field hold a secret (auth token, HMAC key, credential-encryption key, DB password)? | **L2 env** (R2 binding) |
| Does this field need to change without restarting the service AND admins set it from the Control Plane UI AND it varies per ThingType (fleet) or per Thing (override)? | **L3 template / override** |
| Does this field need to change without restarting the service AND it is a single global value (no per-Thing variation, no per-type variation)? | **L4 system_metadata** |
| Else (service shape — port, timeout, log level, feature flag, allowlist, etc.) | **L1 yaml** |

## §3 — L1: yaml (per-service boot shape)

Each of the five services (`nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy`, `agent`) owns a pair of yaml files at `packages/<svc>/<svc>.config.yaml` (prod shape) and `packages/<svc>/<svc>.dev.yaml` (local-dev overrides). The service's `main()` loads one of them at boot via the loader described in `service-bootstrap-config-architecture.md` (A05).

**What belongs here:** service shape that is constant for the life of the process — listen port, timeout windows, advertise host, log level, feature flags, allowlists, Redis mode / addresses, database URL, NATS connection string. Changing any of these requires a service restart by design.

**What is forbidden here:** any secret. Per CLAUDE.md mandatory rule "Secrets are env-only", a yaml file MUST NOT contain `password`, `secret`, `token`, or `key` fields holding secret values. Adding such a field requires explicit user approval in chat. (Yaml may reference an env var name as a value — e.g. the credential-encryption key name to look up at runtime — but the secret itself stays in env.)

**What is enforced:** `scripts/check-no-yaml-secrets.mjs` scans staged yaml at pre-commit time and rejects matches for the forbidden field-name patterns. The `npm run check:no-yaml-secrets:strict` flag runs the same scan repo-wide.

**Own-address fields + the peer-URL rule (binding).** A service configures its OWN addresses and never a peer's:

- `publicURL` (top-level, all four server services; required) — the externally reachable base URL, reported to the Thing Registry as `staticInfo.publicUrl`. Env override `<SVC>_PUBLIC_URL`.
- `privateURL` (top-level, all four server services; **optional**) — the internal service-to-service base URL, reported as `staticInfo.privateUrl`. Env override `<SVC>_PRIVATE_URL` (`NEXUS_HUB_PRIVATE_URL`, `CONTROL_PLANE_PRIVATE_URL`, `AI_GATEWAY_PRIVATE_URL`, `COMPLIANCE_PROXY_PRIVATE_URL`). **Default = auto-derived from the bind interface** via `packages/shared/core/metrics/platform/staticinfo.go` `EffectivePrivateURL`: a specific bind host (e.g. the appliance's `127.0.0.1` behind nginx) is advertised verbatim; a wildcard/empty bind advertises `http://<primary-outbound-IPv4>:<service-port>` — nobody sets it in the common case; set it only for split-horizon or non-default topologies. The compliance-proxy derives the port from its RuntimeAPI listen address (the surface peers call).
- **Peer service URLs are never local config.** A service that needs another Nexus service's URL resolves it at runtime from that peer's Hub-reported Thing via `packages/shared/transport/peerurl` (`GET /api/internal/things/service-url/:thing_type` — contract in `service-call-framework.md` §6.5). The former peer-URL fields — compliance-proxy `compliance.aiGatewayUrl`, control-plane `bff.aiGatewayUrl` / `bff.complianceProxyUrl` / `bff.complianceProxyRuntimeUrl` (env `AI_GATEWAY_URL` / `COMPLIANCE_PROXY_URL` / `COMPLIANCE_PROXY_RUNTIME_URL`) — are **deleted**; do not reintroduce this class of field.
- Special case: compliance-proxy `onboarding.cpUIBaseURL` stays as an **optional override** for the 407-page display link; when unset it defaults to the Hub-resolved Control Plane `publicUrl`.
- Still config (not peer URLs): the bootstrap `registry.nexusHubUrl` (needed before any Thing can be read), infrastructure URLs (DB / NATS / Redis), and external IdP URLs.

## §4 — L2: env vars (.env.example + bootenv)

The repo-root `.env.example` is the contract: it documents every environment variable any of the four Go services consumes at runtime, including a one-line explanation per variable.

**Loader:** `packages/shared/core/bootenv/bootenv.go` `LoadFromRepoRoot`. It walks up from the service's cwd looking for the `.git` marker (file or directory — `git worktree add` creates `.git` as a file), then loads `<root>/.env` via `godotenv.Load`. The non-overload variant is used deliberately: **process env vars always win** over `.env` file values. This makes systemd `EnvironmentFile=`, Docker `--env-file`, Kubernetes `envFrom`, and ad-hoc `MY_VAR=x ./svc` invocations always override the file.

**Application contract:** code reads secrets via `os.Getenv` only. The `.env` file is a boot-time convenience for local dev; it is never read on the request path and there is no `.env` in prod (env vars come from systemd EnvironmentFile or K8s Secret).

**`[MUST MATCH]` tag:** values tagged `[MUST MATCH]` in `.env.example` must be identical across the services listed in the tag. Cross-service drift on these is the most common source of inter-service 403s — a `[MUST MATCH all 4 services]` value like `INTERNAL_SERVICE_TOKEN` is what Hub's WebSocket registration + `/api/internal/things` auth checks against and the agent's bearer header; the CP→Hub config-write (`/api/hub`) + admin-alerts surface uses the separate `[MUST MATCH CP ↔ Hub]` `HUB_CONFIG_TOKEN` (SEC-W2-02 — split so a data-plane service's service-token leak cannot inject fleet config). Mismatch on any pair instantly breaks that pair.

**Categories of values in `.env.example`** (each value documented in the file itself; this doc lists categories, not values):
- Inter-service auth (`INTERNAL_SERVICE_TOKEN` shared by all 4; `HUB_CONFIG_TOKEN` CP ↔ Hub config-write authority; `ADMIN_KEY_HMAC_SECRET`) — required, multi-service shared.
- Credential encryption (`CREDENTIAL_ENCRYPTION_KEY`, optional multi-key rotation map) — CP ↔ AI Gateway shared.
- Secret custody (SEC-W2-03): the `secretCustody` **yaml** block on CP + AI Gateway (`provider: noop|command`, `command` argv, `timeoutSec`) is **non-secret config** (argv only, no secret — same shape as compliance-proxy's `ca.kms`), so it lives in yaml, not env. It governs how the crown-jewel env secrets above are resolved at boot: `noop` reads them raw; `command` treats each as a base64 wrapped blob unwrapped once via the `packages/shared/core/kms` envelope-custody provider. The [MUST MATCH] contract stays on the unwrapped plaintext.
- DB / Redis / NATS connection strings + credentials.
- Per-service tuning that requires env (e.g. log level overrides, debug toggles, optional integrations).
- Test harness (`tests/.env.<target>` for local/dev/prod targets — see `local-dev-debugging.md`).

**Renaming an env var** is a 14-layer sweep (§6.5). The env-var name lives in `.env.example`, in every service that calls `os.Getenv`, in systemd EnvironmentFile on prod EC2, in K8s Secret manifests if any, and in test fixtures.

## §5 — L3: thing_config_template + thing_config_override

This layer is the fleet-managed config: admin writes a value in the Control Plane UI; Hub propagates it to every Thing of the affected type (template) or to a single Thing (override). The data model — `thing_config_template` PK `(type, config_key)`, `thing_config_override` PK `(thing_id, config_key)`, the cascade `override > template`, the Type A vs Type B configKey semantics, the override blacklist (`credentials`, `virtual_keys`) — is the subject of A01 §4 + §5. The end-to-end write → push → apply → report flow is A02. This section restates only the L3-specific rules an L3 PR must follow.

**configKey registration is single-source-of-truth:** every configKey lives as a Go const in `packages/shared/schemas/configkey/configkey.go`. Adding a new configKey requires updating, **in the same PR**:
- The const declaration in `configkey.go`.
- The `ValidByThingType` map in `packages/shared/schemas/configkey/validation.go` (per-ThingType allowed-key set).
- The `TypedRegistry` map in `packages/shared/schemas/configkey/typed.go` for Type A keys (currently `json.RawMessage` placeholders; typed structs land per-key as receivers adopt them).
- The §7 catalog row in this doc.
- A seed row in `tools/db-migrate/seed/fixtures/thing_config_template.json` for the `thing_config_template` default — without this row, `AuditTemplateRows` (`packages/shared/schemas/configkey/validation.go`) logs a WARN at Hub startup but does not block boot.

**Override write-side blacklist:** `packages/shared/schemas/configtypes/policy/override_policy.go` `nonOverridableConfigKeys` lists the configKeys CP must reject when the admin attempts an override write. Currently `credentials` and `virtual_keys`. Adding entries is a deliberate policy change and requires SDD + spec updates in the same PR; the blacklist is unexported on purpose so external packages can only consult it via `IsOverridable` / `IsBlacklisted` / `BlacklistedKeys`.

## §6 — L4: system_metadata

`system_metadata` is a singleton key/value table — PK is `key String @id`, value is `Json`, audit columns track who wrote and when. The Hub schema lives at `tools/db-migrate/schema/admin.prisma` (model `SystemMetadata`).

**What belongs here:** single-instance global runtime config that admins change in the UI but that has no per-ThingType or per-Thing variation. The receiver pattern is consistent: write happens via an admin handler; readers periodically `SELECT value FROM system_metadata WHERE key = $1` and apply the result. There is no push channel — receivers poll DB or reload on demand.

**Known keys** (enumerated by greppable usage at `system_metadata WHERE key = ...` SQL sites + string-literal `*.config` / `*.settings` patterns):

| `key` | Purpose | Primary reader |
|---|---|---|
| `siem.config` | SIEM bridge sink + auth header config | `packages/nexus-hub/internal/observability/siem/bridge.go` |
| `payload_capture.config` | Payload-capture toggles + caps (overrides `thing_config_template.payload_capture` for service-side receivers) | `packages/nexus-hub/internal/compliance/catbagent/payload_capture.go` + AI Gateway / Compliance Proxy receivers |
| `streaming_compliance.config` | Streaming-compliance policy snapshot | `packages/nexus-hub/internal/compliance/catbagent/streaming_compliance.go` |
| `observability.config` | Fleet observability toggles (re-read by every receiver — see A01 §5 note) | Every service's configdispatch receiver |
| `gateway.settings` | AI Gateway runtime settings (cache toggles, behaviour flags) | AI Gateway dispatch |
| `agent.settings` | Agent runtime settings | Hub catbagent loader path |
| `semantic_cache.config` | Semantic cache singleton config (fleet-wide L1 embedding) | AI Gateway dispatch |
| `gateway.credential_reliability.config` | Credential reliability thresholds | AI Gateway dispatch + Hub credential-health rollup job |
| `propagation_ledger:<thingType>:<configKey>` | Control-Plane-internal durable backstop for Category-B pushes: per-key `{intended, acked}` versions. Bumped on each security-sensitive `InvalidateConfigE`, acked on confirmed push; the CP reconcile loop re-pushes any key where `acked < intended`. Namespaced family (one row per `(type, key)`), not a singleton; never read by data-plane Things. | `packages/control-plane/internal/platform/hub/ledger.go` (CP reconcile arm) |

**Coexistence with L3:** some keys live in both `thing_config_template` and `system_metadata` by design — `payload_capture` is the classic example. The `thing_config_template.payload_capture` row is the change-signal channel (pushed via shadow); the `system_metadata['payload_capture.config']` row is the authoritative value the receivers re-read. The split exists because the receiver-side state lives in CP-owned business tables and the `thing_config_template` push would otherwise carry an empty / stale state. A01 §5 carries the full Type A / Type B coexistence note.

`streaming_compliance` has the identical shape (`system_metadata['streaming_compliance.config']` is
its authoritative row) and is the reason this paragraph now carries a warning: **a Type-B receiver
that applies the pushed payload instead of re-reading will reset the admin's configuration to the
built-in defaults on every push, and nothing will report it.** The compliance proxy did exactly that.
It loaded the admin's `chunked_async` at boot and its own invalidation handler replaced it with the
default `passthrough` 70 ms later, after which every SSE stream was relayed uninspected — `passthrough`
neither accumulates nor can reject. The failure is silent by construction: the push succeeds, the
propagation ledger acks, and the only visible difference is a mode string in a log line that the
service's boot log level was filtering.

The rule, stated as a rule: **on a Type-B trigger, ignore the payload and re-read.** The reference
implementation is `registerPayloadCapture` in the compliance proxy's `configdispatch`, which does not
look at `raw` at all. A receiver that needs to tolerate both shapes must treat an empty payload
(absent, `""`, `null`, `{}` — note `null` is four bytes, so a `len() == 0` check does not catch what
the Hub sends) as "no state supplied", never as "use defaults".

## §6.5 — Rename sweep discipline (binding, 14 layers)

Half-completing a rename leaves the system in an inconsistent state where half the code reads the old name and half reads the new — silent prod breakage by construction. To prevent this, every PR that renames a yaml field, env variable, `thing_config_template` configKey, or `system_metadata` key MUST run `scripts/check-rename.sh OLD NEW` and confirm all 14 layers are clean before merge.

The 14 layers (enforced verbatim by `scripts/check-rename.sh`):

| L | Layer | Scope |
|---|---|---|
| 1 | Go source (production) | `*.go` excluding `*_test.go` and `vendor/` |
| 2 | Go tests | `*_test.go` |
| 3 | yaml configs | `*.yaml`, `*.yml` |
| 4 | env example files | `.env.example`, `tests/.env.*.example` |
| 5 | seed fixtures | `tools/db-migrate/seed/` |
| 6 | DB schema | `tools/db-migrate/schema/`, `tools/db-migrate/schema-extras.sql` |
| 7 | admin UI source | `packages/control-plane-ui/src/`, `packages/agent/`, `*.tsx`, `*.ts` |
| 8 | UI i18n locales | `packages/control-plane-ui/public/`, `packages/control-plane-ui/src/i18n/`, `packages/ui-shared/src/i18n/`, `*.json` |
| 9 | prod EnvironmentFile | SSH to prod EC2, `cat /etc/systemd/system/nexus-*.service.d/env.conf` |
| 10 | prod DB rows | SSH to prod EC2, `psql` against `thing_config_template` + `thing_config_override` |
| 11 | docs | `docs/` |
| 12 | skills | `.claude/skills/` |
| 13 | CLAUDE.md + cursor rules | `CLAUDE.md`, `.cursor/rules/` |
| 14 | test fixtures + scripts | `tests/` |

**Modes:**
- `scripts/check-rename.sh OLD NEW` — single-rename audit.
- `scripts/check-rename.sh --plan` — runs every rename in `scripts/check-rename.manifest.tsv`.
- `scripts/check-rename.sh --manifest <file>` — custom TSV.
- `scripts/check-rename.sh --skip-prod OLD NEW` — skips L9 + L10 when SSH is not available.

**Allowlist** (matches that don't count as breakage): the script itself, `CHANGELOG.md`, and this document — those documentation locations are where the rename history is explicitly recorded by design.

**`--plan` mode** scans `scripts/check-rename.manifest.tsv`, a tab-separated `old<TAB>new` file listing the renames committed in the current migration. The manifest is intended to be hand-maintained as the rename queue and emptied (or pruned) on PR merge. Pure deletes are written as `OLD<TAB>(deleted)` and still scan for leftover OLD references.

## §6.6 — R1-R5: invariants the layer model rests on

These are the five rules every config PR has to obey. Each cites the exact check or code path that backs it:

**R1 — Single layer of authority per field.** A given config field lives in exactly one of L1 / L2 / L3 / L4. No double-write. Two layers writing the same field disagrees on truth the moment one diverges; the receiver can read either. The decision table in §2 picks the layer; once picked, every other layer's representation must point at it (e.g. yaml may reference an env var name; CP UI may surface a configKey label, but the value lives only in L3).

**R2 — Secrets are env-only.** Every secret (auth token, HMAC key, credential-encryption key/passphrase/salt, internal-service token, DB password) lives in L2 and only in L2. No secret field in any committed yaml. Enforced by `scripts/check-no-yaml-secrets.mjs` at pre-commit + `npm run check:no-yaml-secrets:strict` in CI. Adding a yaml field named `secret`/`password`/`token`/`key` requires explicit user approval in chat. Source: CLAUDE.md mandatory rule "Secrets are env-only".

**R3 — Process env always wins over .env file.** `packages/shared/core/bootenv/bootenv.go` uses `godotenv.Load` (non-overload). This makes systemd `EnvironmentFile=`, Docker `--env-file`, Kubernetes `envFrom`, and one-off shell overrides always trump the file's defaults. Application code reads via `os.Getenv` only and never inspects `.env` directly.

**R4 — Push channels are versioned and idempotent.** L3 writes propagate through `thing_config_template.version` (monotonic per-(type, key), bumped per admin write) and `thing.desired_ver` (per-type monotonic for template writes; per-Thing `+= 1` for override writes). Thing-side apply uses the `desiredVer > reportedVer` predicate (A01 §7), `Force=true` is the explicit bypass for admin re-sync replays (A02 §10). Equal-version applies are skipped — so a stale message cannot re-apply over a fresher one.

**R5 — Renames sweep all 14 layers in the same PR.** §6.5 is binding. `scripts/check-rename.sh` must report `ALL 14 LAYERS CLEAN` for every rename before the PR merges. The script is allowed to skip L9 + L10 only when `--skip-prod` is passed (no SSH); in that case the PR description must call out that the deployer will run the prod scan post-merge before traffic resumes.

## §7 — Per-key catalog

### L3 configKeys (`thing_config_template` + `thing_config_override`)

Type A = `state` is the config payload (callback applies directly). Type B = `state` is empty/null; the version bump is a "go reload" signal and the actual data lives in a dedicated DB table or `system_metadata` key (see §6).

| configKey | Type | Allowed ThingTypes | Wire `state` shape |
|---|---|---|---|
| `log_level` | A | nexus-hub, control-plane, ai-gateway, compliance-proxy | `{level: string}` |
| `killswitch` | A | compliance-proxy, agent | `{engaged: bool}` (interception.Killswitch) |
| `ai_guard` | A | ai-gateway | AI Guard backend config blob |
| `cache` | A | ai-gateway | AI Gateway prompt/response-cache config blob — `{adapters, providers}` only. There is **no** global tier: the former `global.cache_master_kill_switch` and `global.normaliser_enabled` fields are retired (emergency cache-off = the fleet disable-all on `/ai-gateway/cache` or Emergency Passthrough's `bypassCache`; the upstream rewrite is demand-driven off the adapter rule + provider marker-inject settings in this same blob) |
| `gateway_passthrough` | A | ai-gateway | Emergency passthrough toggle (3-tier global/adapter/provider) |
| `agent_settings` | A | agent | Agent runtime settings (quit-allowed, shutdown warning, auto-update, traffic upload level, theme, QUIC fallback bundles, bypass bundles, attestation). Heartbeat/drain intervals are NOT shadow-pushed — CP strips them on PUT; they are local-yaml-only |
| `diag_mode` | A | agent | `{until: string}` (RFC3339) — per-thing override; admin writes it via the Hub override API with `expires_at`=until, the agent raises its local log level to debug until the window ends |
| `onboarding` | A | compliance-proxy | Compliance-proxy onboarding state |
| `payload_capture` | A (agent) / B (ai-gateway, compliance-proxy) | ai-gateway, compliance-proxy, agent | agent: `{enabled: bool}`; server: null (receivers re-read from `system_metadata['payload_capture.config']`) |
| `observability` | B (everywhere) | nexus-hub, control-plane, ai-gateway, compliance-proxy | null (receivers re-read from `system_metadata['observability.config']`) |
| `response_cache.time_sensitive_patterns` | A | ai-gateway | Cluster-wide freshness rule list |
| `semantic_cache.config` | A | ai-gateway | Fleet-wide L1 embedding singleton config (`vary_by` / `enabled` / `threshold`), hot-swapped into the in-process `SemanticConfigCache` independently of semantic-index lifecycle so the fleet config applies even when the index is not yet ready |
| `response_cache.extract_config` | A | ai-gateway | L1 extract cache fleet config (atomic.Pointer hot-swap) |
| `providers` | B | ai-gateway | null — receiver reloads provider snapshot |
| `models` | B | ai-gateway | null — receiver reloads model catalog |
| `credentials` | B | ai-gateway | null — receiver reloads credential snapshot. **Override-blacklisted** |
| `routing_rules` | B | ai-gateway | null — receiver reloads routing-rule snapshot |
| `virtual_keys` | B | ai-gateway | `{op:"invalidate", ids:[...]}` — scoped eviction. **Override-blacklisted** |
| `quota_policies` | B | ai-gateway | null — receiver reloads quota policy snapshot |
| `quota_overrides` | B | ai-gateway | null — receiver reloads quota override snapshot |
| `organizations` | B | ai-gateway | null — receiver reloads org snapshot |
| `interception_domains` | B | compliance-proxy, agent | null — receiver reloads domain interception list |
| `hooks` | B | ai-gateway, compliance-proxy, agent | null — receiver reloads hooks snapshot |
| `exemptions` | B | compliance-proxy, agent | null — receiver reloads exemption list |
| `streaming_compliance` | B | compliance-proxy, agent | null — receiver reloads streaming compliance policy |
| `credential_reliability` | B | ai-gateway | null — receiver reloads credential reliability thresholds |
| `siem` | B (declared) | none — see note | constant declared in `packages/shared/schemas/configkey/configkey.go` but absent from every entry in `ValidByThingType`; admin cannot write a `thing_config_template` row with this key today (`AuditTemplateRows` would log it as orphan). The live SIEM operational config flows through `system_metadata['siem.config']` (Layer 4) |
| `installed_rule_packs` | B | agent | null — agent pulls full rule pack snapshot |
| `user_context` | B | agent | null — agent pulls per-device user context |

The single source of truth for the configKey set is `packages/shared/schemas/configkey/configkey.go` + `validation.go`. This table reflects what those files declare today. Adding a row here is one of the four required steps when introducing a new configKey (see §5).

### L4 `system_metadata` keys

See §6 for the table — `siem.config`, `payload_capture.config`, `streaming_compliance.config`, `observability.config`, `gateway.settings`, `agent.settings`, `semantic_cache.config`, `gateway.credential_reliability.config`, and the CP-internal `propagation_ledger:<thingType>:<configKey>` family. Also `agent.config.version`, `audit_chain.acked_orphans`, `device.auth.mode` and `hub.spill_upload_secret`.

**Who claims a key.** The table has no schema and no owner: a row is a key and a JSON blob written by whichever service reached for it. `packages/shared/schemas/configkey/systemmetadata.go` is the registry of the keys some code path reads or writes, plus the prefix families built at runtime. The Control Plane runs `AuditUnknownKeys` once at boot and logs a WARN naming every key nobody claims.

WARN and not fatal, on purpose: an unclaimed row breaks nothing, and refusing to start over one turns bookkeeping into an outage. What it needs is a person to look. The failure it exists to prevent is a key outliving the code that read it — `audit.payload` did for three months, and it was worse than dead weight, because its value was the OPPOSITE of the setting actually in force and its timestamp kept moving, so an operator asking "is body capture on?" by the obvious name read `false` and stopped investigating.

The registry is checked in both directions. A key present in the database and absent from the registry is what the boot WARN reports. A key registered but mentioned in no non-test Go file is what `TestSystemMetadataKeys_EveryEntryIsClaimedBySomeCode` reports — otherwise the registry itself goes stale and starts vouching for keys nobody uses, which recreates the original problem one level up.

Adding a fixed key means adding it to `SystemMetadataKeys`; a key built at runtime from a thing type or a config key belongs in `systemMetadataKeyFamilies` as a prefix rather than being enumerated, because an enumerated family either goes stale or fires on legitimate rows every boot — and a warning that cries wolf is one nobody reads.

## §8 — Workflow: adding a new config field

1. Pick the layer using the decision table in §2.
2. Apply the per-layer checklist:

   **L1 (yaml)**:
   - Add the field to the service's config struct in Go.
   - Add to both `packages/<svc>/<svc>.config.yaml` and `packages/<svc>/<svc>.dev.yaml`.
   - Confirm `scripts/check-no-yaml-secrets.mjs` passes (no `password`/`secret`/`token`/`key` field names).

   **L2 (env var)**:
   - Add to `.env.example` at repo root with a doc comment explaining the variable.
   - If the variable is shared across services, tag it `[MUST MATCH <which services>]` in the comment.
   - Update every service that consumes it: `os.Getenv("VAR_NAME")` at the call site.
   - If the variable lives in prod systemd `EnvironmentFile`, update the prod deploy procedure / template.

   **L3 (configKey)**:
   - Add the const to `packages/shared/schemas/configkey/configkey.go` (snake_case, no `_config` suffix).
   - Add to `ValidByThingType` map in `validation.go` for each ThingType that accepts it.
   - For Type A keys: add to `TypedRegistry` in `typed.go` (start with `json.RawMessage` placeholder; promote to a typed struct under `packages/shared/schemas/configtypes/` when a receiver wants typed decoding).
   - For Type B agent keys that need Hub-side aggregation: add a loader file under `packages/nexus-hub/internal/compliance/catbagent/` and wire it in `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`.
   - Add a seed row to the matching `tools/db-migrate/seed/fixtures/<table>.json` (reference) or `seed/fixtures/demo/<table>.json` (demo).
   - Add a row in this doc's §7 catalog.
   - Each service that needs to receive the key: add a registration in `packages/<svc>/cmd/<svc>/configdispatch/` (or for agent, `packages/agent/cmd/agent/configdispatch.go`) — `cfgloader.Register[V]` for typed Type A, `RegisterRaw` for raw-byte Type A, `RegisterRawPull` for agent Type B that needs HTTP pull.

   **L4 (system_metadata)**:
   - Add the admin write path (or the initial seed) — `INSERT INTO system_metadata (key, value, ...) VALUES (...)`.
   - Add the reader at every consumer.
   - Add a row in this doc's §6 table.

3. Verify: build, run any affected tests, exercise the read path against a fresh DB to confirm seed / migration is correct.

## §9 — Redis configuration

Redis is referenced from yaml (`packages/<svc>/<svc>.dev.yaml` `redis:` block) but the connection-shape choice is architectural enough to call out separately. The yaml structure is the same across all five services; only the addresses differ.

**Mode selection** (yaml `redis.mode` field):
- `standalone` — single Redis instance. Used in local dev (the docker-compose Redis on port 6437) and in single-node prod deployments.
- `sentinel` — Redis Sentinel with master + replicas + sentinels. Use `redis.sentinel.masterName` + `redis.addrs` (list of sentinel addresses).
- `cluster` — Redis Cluster sharded across nodes. Use `redis.cluster.maxRedirects`, `routeRandomly`, `readOnly`.

The factory that consumes this yaml lives at `packages/shared/storage/redisfactory/`. Its docstring explicitly references this section ("docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md §9").

**Cross-service constraint:** every service that uses Redis must agree on mode, address set, and credentials. Drift causes session loss (CP admin sessions stored in Redis), IAM cache misses, response cache misses, and quota-counter inconsistency.

**Valkey:** see the `e61-valkey-migration` runbook (`docs/operators/ops/runbooks/`) for the Redis → Valkey migration steps. The client library is wire-compatible; the migration is operational (deploy + cutover), not code.

## References

- yaml configs — `packages/<svc>/<svc>.config.yaml`, `packages/<svc>/<svc>.dev.yaml`
- env contract — `.env.example` (repo root)
- env loader — `packages/shared/core/bootenv/bootenv.go`
- configKey constants + ValidByThingType + TypedRegistry — `packages/shared/schemas/configkey/`
- Typed config payload schemas — `packages/shared/schemas/configtypes/`
- Override blacklist — `packages/shared/schemas/configtypes/policy/override_policy.go`
- `system_metadata` table — `tools/db-migrate/schema/admin.prisma`
- Rename sweep script — `scripts/check-rename.sh` + `scripts/check-rename.manifest.tsv`
- yaml-secrets guard — `scripts/check-no-yaml-secrets.mjs`
- Hub side `system_metadata` loaders — `packages/nexus-hub/internal/compliance/catbagent/`, `packages/nexus-hub/internal/observability/siem/bridge.go`
- L3 data model + cascade — `docs/developers/architecture/cross-cutting/foundation/thing-model.md` (A01)
- L3 sync flow — `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` (A02)
- yaml + bootstrap loader specifics — `docs/developers/architecture/cross-cutting/foundation/service-bootstrap-config-architecture.md` (A05)
