---
doc: configuration-architecture-migration
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Configuration Architecture — Migration Plan

> **Companion to**: `configuration-architecture.md`
> **Status**: Landed. PR-0 .. PR-9 all complete (renamed keys + deleted dead rows + downgraded yaml + Cat A→B for `exemptions` + drift reconciler expansion all visible in `seed-baseline.sql` + `configkey.go` + `governance/exemptions/handler/handler.go` + `wiring/reconcile.go:58-117`). Remaining tracked work is the per-PR acceptance ticks + the "operator decisions" log below.
> **Original scope**: 10 PRs, ~2000 LOC churn, 1 prod DB migration window with full pg_dump backup
> **Original prod impact**: 9 deletes, 8 renames, 7 shape fixes, 2 fan-out fixes, 3 yaml downgrades

## Per-PR landed status

| PR | Title | Status | Evidence |
|---|---|---|---|
| PR-0 | Dead code cleanup | **LANDED** | `seed-baseline.sql` has no `domain_allowlist` / `compliance_streaming` / agent `auth` / `log_level` / `observability` / `policy_rules` / `timing_intervals` rows; ai-gateway has no `streaming_compliance` row. |
| PR-1 | configKey constant package | **LANDED** | `packages/shared/schemas/configkey/{configkey.go,validation.go,typed.go}` exists with full constant set + `ValidByThingType`. |
| PR-2 | yaml/env structural alignment | **LANDED** | Per `service-bootstrap-config-architecture.md` §3 + §4 (post-rewrite), all 4 services use `log:` / `database.maxConns/minConns/maxConnLifetime` / `NEXUS_HUB_PORT` / unprefixed shared infrastructure URLs. |
| PR-3 | Redis universal factory | **LANDED** | `packages/shared/storage/redisfactory/` exists; `REDIS_MODE` / `REDIS_ADDRS` env shape in use. |
| PR-4 | Rename 8 configKeys + state preservation | **LANDED** | `seed-baseline.sql` carries `ai_guard`, `cache`, `hooks` (×3), `gateway_passthrough`, `exemptions`. Old names absent. |
| PR-5 | Shape fixes (observability ×4 + agent_settings + aiguard publisher) | **LANDED** | `observability` template state is `null` for all 4 Thing types in `seed-baseline.sql`; agent `killswitch` carries `{"enabled": true}`. |
| PR-6 | Fan-out fixes (nexus-hub.observability, compliance-proxy.streaming_compliance) | **LANDED** | compliance-proxy `streaming_compliance` row present in `seed-baseline.sql`; nexus-hub `observability` row present. |
| PR-7 | Downgrade 3 keys to yaml | **LANDED** | `seed-baseline.sql` has no rows for `forward_headers_config`, `upstream_timeouts`, or `access_control`. |
| PR-8 | Agent Cat A → Cat B for `exemptions` | **LANDED** | `packages/control-plane/internal/governance/exemptions/handler/handler.go` calls `h.hub.InvalidateConfig(ctx, "agent", "exemptions")`; agent side is `RegisterRawPull`. |
| PR-9 | Drift reconciler expansion + skill docs | **LANDED** | `packages/control-plane/cmd/control-plane/wiring/reconcile.go:58-117` watches `cache`, `agent_settings`, `killswitch` (compliance-proxy + agent), and `gateway_passthrough` — the four emergency-grade keys. Skill doc updates (prod-deploy / prod-debug entries listed below in §"Architecture doc references") are tracked independently. |

---

## Order of execution

Risk increases monotonically with PR number. **Stop and revisit if any PR's smoke fails before proceeding to the next.**

| PR | Title | Risk | Touches | Prod data change | Code LOC | Dep |
|---|---|---|---|---|---|---|
| **PR-0** | Dead code cleanup | 🟢 Low | seed + 4 services + agent | -9 template rows, -1 override | -400 | — |
| **PR-1** | configKey constant package | 🟢 Low | shared + 4 services | 0 | +200 / -100 | — |
| **PR-2** | yaml/env structural alignment | 🟡 Med | 4 services + EnvironmentFile | 0 (env file change) | +50 / -50 | PR-1 |
| **PR-3** | Redis universal factory | 🟡 Med | shared + 4 services + EnvironmentFile | 0 (env file change) | +400 / -150 | PR-2 |
| **PR-4** | Rename 8 configKeys + state preservation | 🟡 Med | seed + 4 services + UI + DB | 8 renamed rows | +50 / -50 | PR-1 |
| **PR-5** | Shape fixes (observability ×4 + agent_settings + aiguard publisher) | 🟢 Low | CP + receivers + seed | 5 state writes | +30 / -50 | PR-4 |
| **PR-6** | Fan-out fixes (nexus-hub.observability, compliance-proxy.streaming_compliance) | 🟢 Low | CP + compliance-proxy receiver | 0 | +30 / 0 | PR-5 |
| **PR-7** | Downgrade 3 keys to yaml | 🟠 Higher | 4 services + DB + UI | -3 template rows, -1 override | -150 | PR-6 |
| **PR-8** | Agent Cat A → Cat B for `exemptions` | 🟡 Med | nexus-hub + agent | 1 state shape | +80 / -30 | PR-7 |
| **PR-9** | Drift reconciler expansion + skill docs | 🟢 Low | reconcile.go + 2 SKILL.md | 0 | +50 | PR-8 |

---

## ⚠ BINDING — Rename Sweep Discipline (for every rename in any PR)

Every rename (configKey, yaml field, env variable, Go struct field, UI string) touches AT LEAST these 14 locations. **Half-completing a rename = silent prod breakage**. For every rename listed in this plan, the PR MUST sweep all of:

1. **Go source code** (`packages/**/*.go` excluding tests)
2. **Go tests** (`packages/**/*_test.go`)
3. **yaml files** (`packages/**/*.yaml`)
4. **`.env.example` + `tests/.env.<target>.example`**
5. **`tools/db-migrate/seed/data/*.sql`** (seed)
6. **`tools/db-migrate/migrations/*/migration.sql`** (if any newly-created migration references the key)
7. **admin UI source** (`packages/control-plane-ui/src/**/*.{tsx,ts,json}`)
8. **admin UI i18n locales** (`packages/control-plane-ui/{public,src}/{i18n,locales}/**/*.json`)
9. **prod systemd EnvironmentFile** (`/etc/nexus-gateway/env` on EC2, SSH-only check)
10. **prod DB rows** (`thing_config_template` + `thing_config_override` + `thing.desired` + `thing.reported`)
11. **`docs/developers/architecture/*.md`** (other arch docs; this doc is the canonical record)
12. **`.claude/skills/*.md`** (skill runbooks)
13. **`CLAUDE.md` + `.cursor/rules/*.mdc`** (binding rules)
14. **Test fixtures + scripts** (`tests/**/*`)

**Mandatory verification per rename**:

```bash
OLD="<old-name>"
git grep -E "\b${OLD}\b" -- ':!docs/developers/architecture/configuration-architecture*.md' ':!CHANGELOG.md'
# Expected: zero matches.
```

**See `configuration-architecture.md` §6.5 for the full sweep checklist, per-rename pre-flight script template, and explanation of what breaks at each skipped layer.**

Reviewers must reject the PR if `git grep` shows any unswept reference.

---

## Mandatory pre-flight (every PR that touches prod DB)

Per `prod-deploy/SKILL.md` Step 0a — non-skippable backup:

```bash
HOST=ec2-user@18.204.174.212
DATE=$(date +%Y%m%d)
TS=$(date -u +%Y%m%dT%H%M%SZ)
PR_ID="pr-N-<short-name>"
BACKUP_REMOTE="/home/ec2-user/db-backups/nexus_gateway-${PR_ID}-${DATE}-${TS}.dump.gz"

ssh $HOST "mkdir -p /home/ec2-user/db-backups && \
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM pg_dump \
    -h localhost -U nexus -d nexus_gateway \
    --no-owner --no-privileges --clean --if-exists \
    | gzip -9 > ${BACKUP_REMOTE} && \
  [ \$(stat -c%s ${BACKUP_REMOTE}) -gt 1048576 ] && echo 'Backup OK: '${BACKUP_REMOTE}"
```

Abort if pg_dump fails or file < 1 MiB. **No PR proceeds without a verified pre-deploy backup.**

---

## PR-0 — Dead code cleanup

### Scope

Pure deletion. No renames, no behavior changes for keys that work today. Removes silent-broken paths + dead receivers / dead publishers.

### Affected rows (DB)

```sql
-- All 9 rows are confirmed dead by 2026-05-19 audit.

DELETE FROM thing_config_template WHERE type='compliance-proxy' AND config_key='domain_allowlist';
DELETE FROM thing_config_template WHERE type='compliance-proxy' AND config_key='compliance_streaming';
DELETE FROM thing_config_template WHERE type='ai-gateway'       AND config_key='streaming_compliance';
DELETE FROM thing_config_template WHERE type='agent'            AND config_key='auth';
-- agent.killswitch RETAINED (safety-critical fleet TLS-bumping kill switch);
-- shape + fan-out + UI fixed in PR-5 (shape) + PR-6 (fan-out + UI).
DELETE FROM thing_config_template WHERE type='agent'            AND config_key='log_level';
DELETE FROM thing_config_template WHERE type='agent'            AND config_key='observability';
DELETE FROM thing_config_template WHERE type='agent'            AND config_key='policy_rules';
DELETE FROM thing_config_template WHERE type='agent'            AND config_key='timing_intervals';

-- Recompute thing.desired for all affected Things (drops keys from thing.desired JSONB).
-- Hub does this server-side on next override mutation, but force it now for cleanliness:
UPDATE thing
SET desired = desired
  - 'domain_allowlist' - 'compliance_streaming' - 'streaming_compliance'
  - 'auth' - 'log_level' - 'observability' - 'policy_rules' - 'timing_intervals',
    -- NOTE: 'killswitch' is NOT stripped — kept for agent fleet safety
    desired_ver = desired_ver + 1
WHERE id IN (
  'gw-ip-172-31-1-117.ec2.internal-3050',
  'proxy-ip-172-31-1-117.ec2.internal',
  'cp-ip-172-31-1-117.ec2.internal-3001'
);
```

⚠ Wrap in a single transaction.

### Affected code

**`packages/agent/cmd/agent/configdispatch.go`**
- Keep `exemptions` registration (line 87) — re-classified in PR-8.
- Keep `killswitch` registration (line 88) — **safety-critical, fixed in PR-5 + PR-6, not deleted**.
- Remove only the deleted keys: line 89 (observability), 90 (timing_intervals), 91 (log_level), 95 (policy_rules), 110 (auth silentNoop), 111 (diag_mode silentNoop stays — diag_mode is a working Type B nudge).

**`packages/agent/cmd/agent/configappliers.go`**
- Delete `policyRules` field (line 38, 202).
- Delete `timingIntervalsApply` block (lines 92-118).
- Delete `logLevelApply` block (lines 120-131).
- Delete `observabilityApplier` block (lines 181-193).
- Keep `agentSettingsApply` (PR-5 strips its 5 dead fields).
- **Keep `killswitch.New` + `applyOf(ks)` wiring** — agent kill switch stays; PR-5/PR-6 fix the data flow into it.

**`packages/agent/cmd/agent/cmd_run.go`**
- Delete lines 283-287 (auth ApplyAuthConfig block).
- Delete `PolicyRules` field passing (lines 213, 258).
- Delete `policyEngine` parameter from `startConfigReloadGoroutine` (line 295, 816) — engine still constructed but rules permanently empty.

**`packages/agent/internal/identity/auth/`**
- Delete `bootstrap.go` lines 168-end (`ApplyAuthConfig` function) — `Start` etc. stay.
- Delete `config.go` (whole file: `AuthConfig` + `UnmarshalAuthConfig`).
- Delete `bootstrap_test.go` cases referencing `ApplyAuthConfig`.

**`packages/agent/internal/policy/core/engine.go`**
- Delete `rules atomic.Value` field (line 37).
- Delete `loadRules` (lines 79-105).
- Delete `Reload` (line 159).
- Simplify `NewEngine` to ignore `rules` parameter (or remove parameter entirely).
- Simplify `Evaluate` to remove rule-iteration loop; keep exemption check + interceptionHostsFn fallback + default action.
- Delete `ApplyShadowState` method.

**`packages/agent/internal/sync/schema/config.go`**
- Delete `PolicyRules []PolicyRuleConfig` field (line 49).
- Delete `PolicyRuleConfig` struct.
- Delete `remote["policyRules"]` merge logic (lines 293-297).

**`packages/agent/cmd/agent/wiring/compliance.go`**
- Delete `PolicyRules` parameter (line 23).
- Delete `policy.NewEngine(cfg.PolicyRules, cfg.DefaultAction)` → use `policy.NewEngine(cfg.DefaultAction)`.

**`packages/agent/agent.dev.yaml`**
- Delete `policyRules:` block (lines 40-44).

**`packages/agent/internal/lifecycle/killswitch/`**
- **KEEP — safety-critical**. Subpackage stays; semantics from `killswitch.go:7-9` doc comment authoritative ("enabled=true means TLS bump is allowed; enabled=false means killswitch engaged, passthrough").
- **`parseKillSwitch` helper at `applied.go:443-455` reads "engaged" — fix in PR-5** to read "enabled" so introspection aligns with the wire schema.

**`packages/compliance-proxy/cmd/compliance-proxy/configdispatch/configdispatch.go`**
- Delete `registerDomainAllowlist` (lines 205-212).
- Delete `registerComplianceStreaming` for the OLD `compliance_streaming` key (lines 293-320 may be the receiver — verify; if it's the only receiver path, it gets repurposed in PR-6 to handle `streaming_compliance`).

**`packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go`**
- Verify no `streaming_compliance` registration. If absent (per audit), nothing to delete on ai-gateway side.

**`packages/control-plane/internal/settings/handler/settings/streaming_compliance.go`**
- Delete the ai-gateway leg from the `InvalidateConfig` fan-out (line 158 → remove `"ai-gateway"` thing-type call).

**`tools/db-migrate/seed/data/seed-baseline.sql`**
- Delete the 9 INSERT lines for deleted rows.

### Validation

```sql
-- After migration: confirm rows gone
SELECT type, config_key FROM thing_config_template
WHERE (type, config_key) IN (
  ('compliance-proxy', 'domain_allowlist'),
  ('compliance-proxy', 'compliance_streaming'),
  ('ai-gateway',       'streaming_compliance'),
  ('agent',            'auth'),
  ('agent',            'killswitch'),
  ('agent',            'log_level'),
  ('agent',            'observability'),
  ('agent',            'policy_rules'),
  ('agent',            'timing_intervals')
);
-- Expected: 0 rows.

-- Confirm thing.desired no longer carries deleted keys
SELECT id, type, jsonb_object_keys(desired) FROM thing WHERE type IN ('ai-gateway','compliance-proxy');
-- Expected: no 'domain_allowlist', 'compliance_streaming', etc.
```

### Smoke

```bash
# 1. Each service still boots
./scripts/dev-start.sh

# 2. Hub log shows no "unknown configKey" WARN for the 3 active prod Things
ssh $HOST 'sudo journalctl -u nexus-hub --since "5 min ago" --no-pager | grep -i "unknown config_key\|orphan"'
# Expected: empty.

# 3. Full smoke
tests/scripts/smoke-gateway.py --all-ingress
```

### Rollback

```bash
# Restore from PR-0 backup
ssh $HOST "gunzip -c /home/ec2-user/db-backups/nexus_gateway-pr-0-*.dump.gz | \
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway"
# Then redeploy previous binary tag.
```

---

## PR-1 — configKey constant package

### Scope

Create `packages/shared/schemas/configkey/configkey.go`. Replace all 16 string-literal sites across CP/Hub/services with constant references. No DB change, no behavior change.

### New files

- `packages/shared/schemas/configkey/configkey.go` — see §8 of architecture doc for full content.
- `packages/shared/schemas/configkey/typed.go` — `TypedRegistry` for Type A keys.
- `packages/shared/schemas/configkey/validation.go` — `ValidByThingType` + audit helper `AuditTemplateRows(db)` that returns slices of orphan / unknown rows.

### Callers to update (16 sites)

| File | Line | Replace |
|---|---|---|
| `packages/nexus-hub/internal/compliance/catbagent/streaming_compliance.go` | 44 | `streamingComplianceConfigKey` → `configkey.StreamingCompliance` |
| `packages/nexus-hub/internal/compliance/catbagent/payload_capture.go` | 51 | `payloadCaptureConfigKey` → `configkey.PayloadCapture` |
| `packages/nexus-hub/internal/jobs/defs/drift/exemption_gc.go` | 107 | `ConfigKey: "active_exemptions"` → `configkey.Exemptions` (after PR-4 rename) |
| `packages/nexus-hub/internal/jobs/defs/rollup/credential_health_rollup.go` | 566 | `ReliabilityConfigKey` const → `configkey.CredentialReliability` |
| `packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go` | 445 | `updateTargetConfigKey = "agentUpdateTarget"` → delete (PR-0 decision: kill agentUpdateTarget) or rename `configkey.AgentUpdateTarget` |
| `packages/ai-gateway/cmd/ai-gateway/wiring/reliability.go` | 19 | `ReliabilityConfigKey` const → `configkey.CredentialReliability` |
| `packages/control-plane/cmd/control-plane/wiring/reconcile.go` | 38, 49, 63 | `"cache_config"`, `"agent_settings"`, `"gateway_settings"` → `configkey.Cache, configkey.AgentSettings, configkey.GatewaySettings` (decide gateway_settings fate) |
| `packages/control-plane/internal/settings/handler/settings/streaming_compliance.go` | 17 | `streamingComplianceConfigKey` → `configkey.StreamingCompliance` |
| `packages/control-plane/internal/settings/handler/settings/payload_capture.go` | 17 | `payloadCaptureConfigKey` → `configkey.PayloadCapture` |
| `packages/control-plane/internal/observability/siem/handler/siem.go` | 26 | `siemConfigKey = "siem.config"` → `configkey.SIEM` (drop ".config" suffix in const value too) |
| `packages/control-plane/internal/governance/aiguard/handler/handler.go` | 241 | `"aiguard_config"` → `configkey.AIGuard` (after PR-4 rename) |
| `packages/control-plane/internal/ai/virtualkeys/handler/handler.go` | 181 | `"virtual_keys"` → `configkey.VirtualKeys` |
| `packages/control-plane/internal/ai/cache/handler/handler.go` | 141 | `"cache_config"` → `configkey.Cache` (after PR-4) |
| `packages/control-plane/internal/infrastructure/infra/setup.go` | 305 | `"onboarding"` → `configkey.Onboarding` |
| `packages/control-plane/internal/ai/providers/handler/credential_reliability.go` | 160 | `reliabilityConfigKey = "gateway.credential_reliability.config"` → delete duplicate; use `configkey.CredentialReliability` |
| (many more) | — | grep all `"<key>"` literals at WRITE sites; readers in configdispatch.go register sites also use constants |

### Startup audit hook

Add to Hub `cmd/nexus-hub/main.go` after store init:

```go
if orphans, unknown, err := configkey.AuditTemplateRows(ctx, store.DB()); err != nil {
    logger.Error("config audit failed", "err", err)
} else {
    for _, k := range orphans {
        logger.Warn("orphan template row (no receiver registered)", "type", k.Type, "key", k.Key)
    }
    for _, k := range unknown {
        logger.Warn("unknown template row (not in ValidByThingType)", "type", k.Type, "key", k.Key)
    }
}
```

### Validation

```bash
# All literal references gone
grep -rE '"(routing_rules|virtual_keys|providers|models|credentials|cache_config|aiguard_config|hook_config|gateway_passthrough_config|killswitch|access_control|payload_capture|interception_domains|exemptions|active_exemptions)"' packages/ --include="*.go" | grep -v "_test.go" | grep -v "/schemas/configkey/"
# Expected: empty (or only inside configkey package definitions).

# Build all 5 binaries
go build ./packages/...
```

### Rollback

Pure code revert (no DB change). `git revert` the PR.

---

## PR-2 — yaml/env structural alignment

### Scope

Rename mismatched yaml keys, env vars; add missing pool fields; Hub self-registration code path verified working.

### yaml changes

**Auth-server fields rename (PR-2)** — drop deployment-specific `cp` prefix and `NEXUS_HUB_` service prefix; align with OAuth-standard shared-infrastructure naming:

| Layer | Old | New |
|---|---|---|
| Hub yaml | `auth.cpURL` | `authServer.url` |
| Hub yaml | `auth.cpJWKSURL` | `authServer.jwksURL` |
| Hub yaml | `auth.cpIssuer` | `authServer.issuer` |
| env | `NEXUS_HUB_CP_URL` | `AUTH_SERVER_URL` |
| env | `NEXUS_HUB_CP_JWKS_URL` | `AUTH_SERVER_JWKS_URL` |
| env | `NEXUS_HUB_CP_ISSUER` | `AUTH_SERVER_ISSUER` — **NEW [MUST MATCH CP ↔ Hub]** (CP writes `iss` claim; Hub verifies it) |
| Hub Go struct | `AuthConfig.CpURL/CpJWKSURL/CpIssuer` | `AuthServerConfig.URL/JWKSURL/Issuer` (new top-level struct alongside existing `AuthConfig`) |
| Hub `internal/config/config.go:398-405` | `os.Getenv("NEXUS_HUB_CP_*")` | `os.Getenv("AUTH_SERVER_*")` |
| CP `cmd/control-plane/config/config.go:338` | `os.Getenv("AUTH_SERVER_ISSUER")` already exists for CP's own issuer field — **stays the same**; now naturally shared with Hub |

Rationale: auth server is a deployment-level shared infrastructure concept (like DATABASE_URL, REDIS_URL, NEXUS_HUB_URL). Hardcoding either "cp" (the process that runs it today) or "nexus-hub" (one of its consumers) in the env name is wrong. CP already uses `AUTH_SERVER_ISSUER` for the issuer side; Hub adopts identical naming for the verifier side, creating a natural [MUST MATCH] relationship in line with `INTERNAL_SERVICE_TOKEN` etc.



1. `compliance-proxy.{config,dev,prod.yaml.example}.yaml`: `logging:` → `log:` (4 services then all use `log:`).
2. `compliance-proxy.*.yaml`: `redis.address` → `redis.addrs` (becomes list).
3. `ai-gateway.*.yaml`: `server.readTimeoutSec` (int) → `server.readTimeout` (time.Duration), same for `writeTimeoutSec`.
4. `control-plane.*.yaml`: `server.shutdownTimeout: 30` (int) → `30s` (Duration).
5. `control-plane.*.yaml`: `bff.nexusHubUrl` → move to top-level `registry.nexusHubUrl` (same env override `NEXUS_HUB_URL`).
6. All 4 services: add uniform `database.{maxConns, minConns, maxConnLifetime, maxConnIdleTime, healthCheckPeriod}` block. Drop CP's `maxOpenConns/maxIdleConns/connMaxLifetime`.
7. Replace every `database.url: "postgresql://nexus:CHANGE_ME_DB_PASSWORD@..."` with `database.url: ""` + comment "set via env DATABASE_URL".

### env changes — full sweep (PR-2)

**Renames** (15 total):

| Old | New | Reason |
|---|---|---|
| `CONTROL_PLANE_URL` | `NEXUS_HUB_URL` | Misnamed (value is Hub URL) |
| `PORT` (CP) | `CONTROL_PLANE_PORT` | Service-private port |
| `NEXUS_HUB_CP_URL` | `AUTH_SERVER_URL` | Entity-shaped infra URL |
| `NEXUS_HUB_CP_JWKS_URL` | `AUTH_SERVER_JWKS_URL` | Same |
| `NEXUS_HUB_CP_ISSUER` | `AUTH_SERVER_ISSUER` | Same + [MUST MATCH CP ↔ Hub] |
| `NEXUS_HUB_AGENTCA_DIR` | `AGENT_CA_DIR` | Shared PKI entity (CP already uses unprefixed) |
| `NEXUS_HUB_AGENTCA_CERT_FILE` | `AGENT_CA_CERT_FILE` | Same |
| `NEXUS_HUB_AGENTCA_KEY_FILE` | `AGENT_CA_KEY_FILE` | Same |
| `CACHE_ENABLED` | `AI_GATEWAY_CACHE_ENABLED` | ai-gateway-private knob |
| `CACHE_TTL` | `AI_GATEWAY_CACHE_TTL` | Same |
| `CACHE_PREFIX` | `AI_GATEWAY_CACHE_PREFIX` | Same |
| `CORS_ENABLED` | `AI_GATEWAY_CORS_ENABLED` | Same |
| `CORS_ALLOWED_ORIGINS` | `AI_GATEWAY_CORS_ALLOWED_ORIGINS` | Same |
| `CRYPTO_PRODUCTION` | `CONTROL_PLANE_CRYPTO_PRODUCTION` | CP-private feature flag |
| `AI_GATEWAY_BASE_URL` | `AI_GATEWAY_URL` | Consolidate simulator handler alias onto canonical name |

**Required in prod EnvironmentFile** (verify present):
- `NEXUS_HUB_URL`, `DATABASE_URL`, `REDIS_*` (PR-3 schema), `NATS_URL`
- `[MUST MATCH]`: `INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`, `COMPLIANCE_PROXY_API_TOKEN`, `AUTH_SERVER_ISSUER`
- `AUTH_SERVER_URL`, `AUTH_SERVER_JWKS_URL`, `AUTH_SERVER_KEYSTORE_DIR` (CP), `AGENT_CA_DIR` (both Hub + CP)

**Forbidden in prod EnvironmentFile** (must be absent after PR-2):
- `CONTROL_PLANE_URL`, `NEXUS_HUB_CP_URL`, `NEXUS_HUB_CP_JWKS_URL`, `NEXUS_HUB_CP_ISSUER`
- `NEXUS_HUB_AGENTCA_DIR`, `NEXUS_HUB_AGENTCA_CERT_FILE`, `NEXUS_HUB_AGENTCA_KEY_FILE`
- `PORT` (CP), `CACHE_*` (unscoped), `CORS_*` (unscoped), `CRYPTO_PRODUCTION` (unscoped), `AI_GATEWAY_BASE_URL`

### Code changes

- `packages/control-plane/cmd/control-plane/config/config.go:260`: rename env read `"PORT"` → `"CONTROL_PLANE_PORT"`. Keep `"PORT"` as alias-fallback for 1 release if user wants safety; pre-GA we just rename.
- `packages/compliance-proxy/cmd/compliance-proxy/config/config.go`: yaml struct rename `Logging *LoggingConfig` → `Log *LogConfig`.
- `packages/ai-gateway/internal/config/config.go`: struct rename `*Sec int` fields → `time.Duration`.
- `packages/control-plane/cmd/control-plane/config/config.go`: rename `BFFConfig.NexusHubURL` → move to new `RegistryConfig.NexusHubURL`.

### Prod EnvironmentFile pre-flight check (NEW skill step)

```bash
ssh $HOST "sudo cat /etc/systemd/system/nexus-*.service.d/env.conf 2>/dev/null || sudo cat /etc/nexus-gateway/env"
# Verify presence of:
#   NEXUS_HUB_URL=...
#   INTERNAL_SERVICE_TOKEN=...
#   ADMIN_KEY_HMAC_SECRET=...
#   CREDENTIAL_ENCRYPTION_KEY=...
#   COMPLIANCE_PROXY_API_TOKEN=...
# Verify absence of:
#   CONTROL_PLANE_URL=...   (should be removed; replaced by NEXUS_HUB_URL)
```

### Validation

```bash
# yaml field uniformity
grep -rE 'yaml:"logging"' packages/ --include="*.go"
# Expected: empty.

grep -rE 'yaml:"(readTimeoutSec|writeTimeoutSec|shutdownTimeoutSec)"' packages/ --include="*.go"
# Expected: empty (all moved to time.Duration).

grep -rE 'maxOpenConns|connMaxLifetime' packages/ --include="*.go" | grep -v "_test.go"
# Expected: empty.

# Each service boots with new yaml
./scripts/dev-start.sh

# All 4 services register with Hub
ssh $HOST 'PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c "SELECT id, type, status FROM thing WHERE id LIKE '\''%-ip-172%'\'';"'
# Expected: 3 'online' rows for ai-gateway/control-plane/compliance-proxy (PR-2 also makes Hub register, +1 row).
```

### Rollback

Restore env file from PR-2 backup. `git revert` PR. Restart all 4 services.

---

## PR-3 — Redis universal factory

### Scope

New `packages/shared/storage/redisfactory/` supporting `standalone | sentinel | cluster` + ACL + TLS. All 4 services migrate to the factory.

### New package

`packages/shared/storage/redisfactory/factory.go` (~400 LOC):

```go
package redisfactory

import (
    "context"
    "crypto/tls"
    "crypto/x509"
    "fmt"
    "log/slog"
    "os"
    "strings"

    "github.com/redis/go-redis/v9"
)

type Mode string

const (
    ModeStandalone Mode = "standalone"
    ModeSentinel   Mode = "sentinel"
    ModeCluster    Mode = "cluster"
)

type Config struct { /* see architecture doc §9 */ }

func New(yamlCfg Config, env Env, logger *slog.Logger) (redis.UniversalClient, error) {
    cfg := mergeEnv(yamlCfg, env)
    if err := validate(cfg); err != nil {
        return nil, err
    }
    tlsCfg, err := buildTLS(cfg.TLS)
    if err != nil {
        return nil, fmt.Errorf("redis TLS: %w", err)
    }
    switch cfg.Mode {
    case ModeStandalone:
        return redis.NewClient(&redis.Options{ /* ... */ }), nil
    case ModeSentinel:
        return redis.NewFailoverClient(&redis.FailoverOptions{ /* ... */ }), nil
    case ModeCluster:
        return redis.NewClusterClient(&redis.ClusterOptions{ /* ... */ }), nil
    }
    return nil, fmt.Errorf("invalid redis mode: %s", cfg.Mode)
}
```

### Caller migrations

- `packages/nexus-hub/cmd/nexus-hub/wiring/redis.go` → `redisfactory.New(cfg.Redis, redisfactory.LoadEnv(), logger)`.
- `packages/control-plane/cmd/control-plane/wiring/redis.go` → same.
- `packages/ai-gateway/cmd/ai-gateway/wiring/redis.go` → same.
- `packages/compliance-proxy/cmd/compliance-proxy/configdispatch/redis.go` → same.

### Removed

- env: `REDIS_URL`, `REDIS_ADDR` (replaced by `REDIS_MODE`/`REDIS_ADDRS`/`REDIS_*`).
- yaml fields: `redis.url`, `redis.addr`, `redis.address`.

### Validation

```bash
# Unit tests for factory
go test -race ./packages/shared/storage/redisfactory/...

# Each service boots with new redis config
./scripts/dev-start.sh

# Sentinel mode integration test (optional, needs sentinel setup)
docker-compose -f tests/fixtures/redis-sentinel.yaml up -d
NEXUS_REDIS_MODE=sentinel NEXUS_REDIS_SENTINEL_MASTER_NAME=mymaster go test ...
```

### Rollback

Restore EnvironmentFile (revert `REDIS_URL` re-add). Revert PR.

---

## PR-4 — Rename 8 configKeys (state preservation)

### Scope

8 key renames; state JSON, version, override rows preserved. Admin UI URL paths unchanged (UI internal references switch to new key names).

### DB migration

```sql
BEGIN;

-- 1. aiguard_config → ai_guard
UPDATE thing_config_template SET config_key = 'ai_guard' WHERE config_key = 'aiguard_config';
UPDATE thing_config_override SET config_key = 'ai_guard' WHERE config_key = 'aiguard_config';

-- 2. cache_config → cache
UPDATE thing_config_template SET config_key = 'cache' WHERE config_key = 'cache_config';
UPDATE thing_config_override SET config_key = 'cache' WHERE config_key = 'cache_config';

-- 3. hook_config → hooks (3 thing-types)
UPDATE thing_config_template SET config_key = 'hooks' WHERE config_key = 'hook_config';
UPDATE thing_config_override SET config_key = 'hooks' WHERE config_key = 'hook_config';

-- 4. gateway_passthrough_config → gateway_passthrough
UPDATE thing_config_template SET config_key = 'gateway_passthrough' WHERE config_key = 'gateway_passthrough_config';
UPDATE thing_config_override SET config_key = 'gateway_passthrough' WHERE config_key = 'gateway_passthrough_config';

-- 5. active_exemptions → exemptions (compliance-proxy only; agent already has 'exemptions')
UPDATE thing_config_template SET config_key = 'exemptions' WHERE type = 'compliance-proxy' AND config_key = 'active_exemptions';

-- 6. Update thing.desired and thing.reported JSONB keys
UPDATE thing SET
  desired = desired
    - 'aiguard_config' - 'cache_config' - 'hook_config' - 'gateway_passthrough_config' - 'active_exemptions'
    || COALESCE(jsonb_build_object('ai_guard',           desired->'aiguard_config'), '{}')
    || COALESCE(jsonb_build_object('cache',              desired->'cache_config'), '{}')
    || COALESCE(jsonb_build_object('hooks',              desired->'hook_config'), '{}')
    || COALESCE(jsonb_build_object('gateway_passthrough',desired->'gateway_passthrough_config'), '{}')
    || COALESCE(jsonb_build_object('exemptions',         desired->'active_exemptions'), '{}'),
  reported = reported
    - 'aiguard_config' - 'cache_config' - 'hook_config' - 'gateway_passthrough_config' - 'active_exemptions'
    || COALESCE(jsonb_build_object('ai_guard',           reported->'aiguard_config'), '{}')
    || COALESCE(jsonb_build_object('cache',              reported->'cache_config'), '{}')
    || COALESCE(jsonb_build_object('hooks',              reported->'hook_config'), '{}')
    || COALESCE(jsonb_build_object('gateway_passthrough',reported->'gateway_passthrough_config'), '{}')
    || COALESCE(jsonb_build_object('exemptions',         reported->'active_exemptions'), '{}')
WHERE id LIKE '%-ip-172%';

-- 7. Update reported_outcomes too (per-key appliedVersion tracking)
UPDATE thing SET reported_outcomes = reported_outcomes /* similar transform */;

COMMIT;
```

### Code changes

All `configkey.AIGuardConfig` / `configkey.CacheConfig` / `configkey.HookConfig` references (PR-1 introduced) get const value swap:

- `packages/shared/schemas/configkey/configkey.go`:
  - `AIGuard = "ai_guard"` (was `"aiguard_config"`)
  - `Cache = "cache"` (was `"cache_config"`)
  - `Hooks = "hooks"` (was `"hook_config"`)
  - `GatewayPassthrough = "gateway_passthrough"` (was `"gateway_passthrough_config"`)
  - `Exemptions = "exemptions"`

CP, Hub, all 4 receivers automatically pick up the new value via constant.

### UI changes

- `packages/control-plane-ui/src/pages/infrastructure/kill-switch/InfraKillSwitchPage.tsx:34,35,59,67`: no change (`killswitch` key unchanged).
- `packages/control-plane-ui/src/pages/infrastructure/proxy-rollout/InfraProxySetupPage.tsx:101`: no change.
- Audit other admin UI references — they all use the CP API endpoints by URL, not by configKey directly (with a few exceptions in InfraOverridesPage history filters).
- `packages/control-plane-ui/src/api/services/infrastructure/nodes/hub.ts`: any hard-coded `"cache_config"` / `"aiguard_config"` etc. in history queries → update.

### Validation

```sql
SELECT type, config_key, version FROM thing_config_template ORDER BY type, config_key;
-- Expected: NO rows with config_key IN ('aiguard_config', 'cache_config', 'hook_config',
--          'gateway_passthrough_config', 'active_exemptions').
-- Expected: NEW rows with config_key IN ('ai_guard', 'cache', 'hooks', 'gateway_passthrough', 'exemptions').
-- Versions preserved: ai_guard=v1, cache=v19, hooks=v9/v10/v9, gateway_passthrough=v7, exemptions=v5666.

SELECT id, jsonb_object_keys(desired) FROM thing WHERE id LIKE '%-ip-172%';
-- Expected: no old key names; new names present.
```

```bash
# Hot smoke: admin cache page still works
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_curl /api/admin/cache/global'
# Expected: 200 with config_key=cache in returned shadow snapshot.

tests/scripts/smoke-gateway.py --all-ingress
```

### Rollback

Restore from PR-4 backup (rename touches DB heavily; rollback = full restore).

---

## PR-5 — Shape fixes

### Scope

5 in-place state cleanups + 1 publisher behavior fix.

### DB updates

```sql
BEGIN;

-- 1. Clear stale observability state on 4 thing-types (receiver re-reads system_metadata)
UPDATE thing_config_template SET state = 'null'::jsonb, version = version + 1, updated_at = now(), updated_by = 'configmigration-pr5'
WHERE config_key = 'observability' AND type IN ('ai-gateway', 'compliance-proxy', 'control-plane', 'agent');

-- 2. Normalize compliance-proxy.payload_capture from {} to null (or to {"enabled":false} for consistency — pick null per Type B convention)
UPDATE thing_config_template SET state = 'null'::jsonb, version = version + 1, updated_at = now(), updated_by = 'configmigration-pr5'
WHERE type = 'compliance-proxy' AND config_key = 'payload_capture';

-- 3. Clear stale ai-gateway.payload_capture {"enabled":false} (receiver re-reads system_metadata)
UPDATE thing_config_template SET state = 'null'::jsonb, version = version + 1, updated_at = now(), updated_by = 'configmigration-pr5'
WHERE type = 'ai-gateway' AND config_key = 'payload_capture';

-- 4. Strip 5 dead fields from agent_settings (UI continues to send them but server normalises)
UPDATE thing_config_template SET
  state = state - 'logLevel' - 'heartbeatIntervalSec' - 'auditDrainIntervalSec' - 'configSyncIntervalSec' - 'auditBatchSize',
  version = version + 1, updated_at = now(), updated_by = 'configmigration-pr5'
WHERE type = 'agent' AND config_key = 'agent_settings';

-- 5. Quota_policies seed inconsistency at row thing-gw-01 (one-off cleanup)
UPDATE thing SET reported = reported || '{"quota_policies":{}}'::jsonb WHERE id = 'thing-gw-01';

-- 6. agent.killswitch seed value: {"engaged":false} → {"enabled":true} (correct field name; default state = disengaged = bump allowed)
UPDATE thing_config_template
SET state = '{"enabled": true}'::jsonb, version = version + 1, updated_at = now(), updated_by = 'configmigration-pr5'
WHERE type = 'agent' AND config_key = 'killswitch';

COMMIT;
```

### Code changes

**`packages/control-plane/internal/governance/aiguard/handler/handler.go:239-241`**:

```go
// OLD:
//   _, _ = h.hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
//     ThingType: "ai-gateway", ConfigKey: configkey.AIGuard, State: stateBytes,
//   })
// NEW: Type B — receiver invalidates its cache and re-reads on next request
h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.AIGuard)
```

**`packages/control-plane/internal/settings/handler/settings/observability.go:33-50`**: strip the construction of the `{enabled,endpoint,serviceName,samplingRate}` state — keep only the system_metadata write + fan-out invalidate.

**`packages/control-plane/internal/settings/handler/settings/agent_settings.go`** (validation/normalization):
- On PUT, drop incoming `logLevel`/`heartbeatIntervalSec`/`auditDrainIntervalSec`/`configSyncIntervalSec`/`auditBatchSize` fields (or warn-and-strip).
- Document the deprecated fields in the API.

**`packages/agent/cmd/agent/configappliers.go` `agentSettingsApply`**: already ignores those 5 fields per audit; no agent change needed.

**`packages/agent/internal/policy/policies/applied.go:443-455` `parseKillSwitch`**:

```go
// OLD:
//   var v struct { Engaged bool `json:"engaged"` }
//   if err := json.Unmarshal(state.State, &v); err != nil { ... }
//   return KillSwitchView{Engaged: v.Engaged, ...}
// NEW: align with wire schema interception.Killswitch{Enabled bool `json:"enabled"`}
var v struct { Enabled bool `json:"enabled"` }
if err := json.Unmarshal(state.State, &v); err != nil { ... }
return KillSwitchView{Enabled: v.Enabled, ...}
```

Also update `KillSwitchView` struct field `Engaged` → `Enabled` to match. Sweep callers — likely the agent's introspection HTTP endpoint and any UI consumer.

### Validation

```sql
SELECT type, config_key, state FROM thing_config_template
WHERE config_key IN ('observability', 'payload_capture')
   OR (type='agent' AND config_key='agent_settings');
-- Expected: observability rows all null; payload_capture rows null; agent_settings has none of the 5 stripped fields.
```

```bash
tests/scripts/smoke-gateway.py --all-ingress
# Plus: admin UI Observability page round-trip
```

### Rollback

Restore from PR-5 backup. Revert PR.

---

## PR-6 — Fan-out fixes

### Scope

Two one-line bug fixes that wire missing data flows.

### Code changes

**1. Add nexus-hub to observability fan-out**

`packages/control-plane/internal/settings/handler/settings/observability.go:76-80`:

```go
// OLD:
//   h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Observability)
//   h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Observability)
//   h.hub.InvalidateConfig(ctx, "control-plane", configkey.Observability)
// NEW:
h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Observability)
h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Observability)
h.hub.InvalidateConfig(ctx, "control-plane", configkey.Observability)
h.hub.InvalidateConfig(ctx, "nexus-hub", configkey.Observability)  // ← added
```

Hub's selfshadow handler (already wired at `packages/nexus-hub/cmd/nexus-hub/wiring/self.go:68-99`) will pick it up via PG NOTIFY.

⚠ Decision needed: agent included in fan-out? Per audit decision (Q4 default A in §12): **no** — agent observability remains yaml-only. Don't add agent here.

**2. Add agent to CP killswitch fan-out (safety-critical)**

`packages/control-plane/internal/governance/killswitch/handler/handler.go`:

```go
// Constants (around line 30-32):
// OLD:
//   const thingTypeComplianceProxy = "compliance-proxy"
// NEW: support both thing types
const (
    thingTypeComplianceProxy = "compliance-proxy"
    thingTypeAgent           = "agent"
)

// PUT handler (around line 80-130):
// After mutating the desired state, fan out to BOTH thing types via NotifyConfigChange:
for _, thingType := range []string{thingTypeComplianceProxy, thingTypeAgent} {
    _, err := h.hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
        ThingType: thingType,
        ConfigKey: configkey.Killswitch,
        State:     stateBytes,
    })
    if err != nil {
        h.logger.Error("killswitch fanout failed", "thingType", thingType, "err", err)
        // continue — don't fail the request if one type is unreachable
    }
}
```

**Semantics confirmation** (verified via agent killswitch package doc): agent's `enabled=true` = bump allowed (normal); `enabled=false` = killswitch engaged (passthrough). **Identical polarity to compliance-proxy** — audit's "polarity inverted" claim was wrong (the package doc explicitly says "Aligns with compliance-proxy"). Single push payload `{enabled: bool}` is correct for both.

**3. Extend `/infrastructure/kill-switch` UI**

`packages/control-plane-ui/src/pages/infrastructure/kill-switch/InfraKillSwitchPage.tsx`:

- Fetch nodes for BOTH `compliance-proxy` AND `agent` thing types.
- Show ONE master toggle ("Fleet Kill Switch") + per-type breakdown showing which types currently have the switch engaged.
- On toggle: call `pushConfigUpdate` once with the new state — CP fan-out automatically applies to both types.
- History view: filter `configKey=killswitch` shows changes for any thing type.

UI copy (per CLAUDE.md i18n binding — add to all 3 locale files):
```
"killSwitchTitle": "Fleet Kill Switch"
"killSwitchDescription": "Immediately stop TLS bumping on ALL Compliance Proxies and Agents in the fleet. Use only for emergency: bad provider rollout, hook regression blocking legitimate traffic, or a NetworkExtension panic."
"killSwitchEngaged": "ENGAGED — TLS bumping disabled fleet-wide"
"killSwitchDisengaged": "Normal operation — TLS bumping active"
```

**Add to drift reconciler**

`packages/control-plane/cmd/control-plane/wiring/reconcile.go`:

```go
{ThingType: "compliance-proxy", ConfigKey: configkey.Killswitch, Loader: ...},  // existing concern, now formalized
{ThingType: "agent",            ConfigKey: configkey.Killswitch, Loader: ...},  // NEW
```

So drift detector re-pushes killswitch state every cycle — guarantees the kill stays engaged even if an agent reconnects mid-incident.

**4. Register `streaming_compliance` receiver on compliance-proxy**

`packages/compliance-proxy/cmd/compliance-proxy/configdispatch/configdispatch.go`: add a new function similar to the existing `registerComplianceStreaming` (which was deleted in PR-0) but for the canonical `streaming_compliance` key:

```go
func registerStreamingCompliance(l *cfgloader.Loader, d Deps) {
    cfgloader.RegisterRaw(l, configkey.StreamingCompliance, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
        // Re-read system_metadata['streaming_compliance.config'] just like ai-gateway/agent do.
        cfg, err := streamingmeta.Load(ctx, d.DB)
        if err != nil {
            return nil, fmt.Errorf("load streaming_compliance: %w", err)
        }
        if d.ProxyServer != nil {
            d.ProxyServer.SetStreamingTuning(cfg.Mode, cfg.PerHookTimeoutMs, cfg.TotalTimeoutMs)
        }
        return nil, nil
    })
}
```

Add registration call in `BuildConfigLoader`:

```go
registerStreamingCompliance(l, d)
```

### Validation

```bash
# Force a Hub observability change and confirm Hub itself picks it up
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && \
  cp_curl PUT /api/admin/settings/observability -d '\''{"otelEnabled":true, "samplingRate":0.5, "traceViewerUrl":"https://jaeger.example"}'\'''
# Check Hub log shows tracer reconfigure
ssh $HOST 'sudo journalctl -u nexus-hub --since "1 min ago" --no-pager | grep -i "reconfigure\|otel"'
# Expected: at least one reconfigure log.

# Force a streaming_compliance change and confirm compliance-proxy receives it
cp_curl PUT /api/admin/settings/streaming-compliance -d '...'
ssh $HOST 'sudo journalctl -u nexus-compliance-proxy --since "1 min ago" --no-pager | grep -i streaming'
# Expected: tuning applied.

# Fleet killswitch fan-out test — engage + verify both types
cp_curl POST /api/admin/compliance/killswitch -d '{"enabled":false,"reason":"deploy-test"}'
# Verify compliance-proxy received it
ssh $HOST 'sudo journalctl -u nexus-compliance-proxy --since "1 min ago" --no-pager | grep "kill switch toggled"'
# Verify thing.desired updated for agent type
ssh $HOST 'PGPASSWORD=... psql -c "SELECT type, desired->'\''killswitch'\'' FROM thing WHERE type IN ('\''compliance-proxy'\'','\''agent'\'') AND id LIKE '\''%-ip-172%'\'';"'
# Expected: both compliance-proxy AND agent rows show {"enabled": false}.
# Restore:
cp_curl POST /api/admin/compliance/killswitch -d '{"enabled":true,"reason":"deploy-test-revert"}'
```

### Rollback

Pure code revert.

---

## PR-7 — Downgrade 3 keys to yaml

### Scope

`forward_headers_config`, `upstream_timeouts`, `access_control` move to yaml-only. Template rows + any overrides deleted. Receivers stop registering shadow handlers.

### Pre-decision

Confirm answer to Q1 (§12): `access_control.sourceIpAllowlist` replacement value.
- **Default [R]**: `["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"]`.

### DB updates

```sql
BEGIN;

-- 1. forward_headers_config
DELETE FROM thing_config_template WHERE config_key = 'forward_headers_config';
-- (no override rows exist; verify)
SELECT count(*) FROM thing_config_override WHERE config_key = 'forward_headers_config';

-- 2. upstream_timeouts
DELETE FROM thing_config_template WHERE config_key = 'upstream_timeouts';

-- 3. access_control
DELETE FROM thing_config_override WHERE config_key = 'access_control';  -- the prod 0.0.0.0/0 row
DELETE FROM thing_config_template WHERE config_key = 'access_control';

-- 4. Strip from thing.desired
UPDATE thing SET
  desired = desired - 'forward_headers_config' - 'upstream_timeouts' - 'access_control',
  reported = reported - 'forward_headers_config' - 'upstream_timeouts' - 'access_control',
  desired_ver = desired_ver + 1
WHERE id LIKE '%-ip-172%';

COMMIT;
```

### yaml changes

**`packages/compliance-proxy/compliance-proxy.prod.yaml.example`** (and `.dev.yaml`):

```yaml
accessControl:
  sourceIpAllowlist:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
    - "127.0.0.0/8"
  domainAllowlist: []
  internalNetworkExceptions: []
  allowUnlistedPassthrough: false
```

Prod yaml gets the same value (operator confirms first).

### Code changes

**`packages/compliance-proxy/cmd/compliance-proxy/configdispatch/configdispatch.go`**:
- Delete `registerAccessControl` function (lines 326-335).
- Delete its registration call in `BuildConfigLoader`.
- Compliance-proxy boot now sets AccessChecker from yaml only.

**`packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go`**:
- Delete `registerAGForwardHeadersConfig` (lines 361-389).
- Delete `registerAGUpstreamTimeouts` (lines 404-440).
- AI Gateway boot loads forward headers + upstream timeouts from yaml only (already does).

### Prod deployment order (CRITICAL)

```
1. Pre-flight backup
2. EDIT prod yaml to set correct accessControl values  ← BEFORE binary deploy
3. Verify yaml change reviewed in git
4. Deploy new compliance-proxy + ai-gateway binaries
5. RUN the DB migration (deletes template + override rows)
6. Smoke: verify prod agent IPs not blocked
7. Verify Hub log shows no "unknown config_key access_control" errors
```

⚠ If step 2 is skipped and the binary deploys before yaml updates, **compliance-proxy will switch from `0.0.0.0/0` to its current yaml default immediately**, which is empty `[]` in the example file — locking out all traffic. Step 2 is non-skippable.

### Validation

```bash
# IP allowlist correctness — send a request from a known-internal IP
ssh $HOST 'sudo journalctl -u nexus-compliance-proxy --since "5 min ago" --no-pager | grep -i "access denied\|allowlist"'
# Expected: zero unexpected denials.

# Send a request from outside the allowlist (e.g., a non-RFC1918 IP) — expect denial in logs.

# Confirm rows gone
ssh $HOST 'PGPASSWORD=... psql -c "SELECT config_key FROM thing_config_template WHERE config_key IN (..);"'
# Expected: empty.
```

### Rollback

1. Restore pg_dump (re-creates template + override rows).
2. Revert binaries.
3. Verify `{"sourceIpAllowlist":["0.0.0.0/0"]}` override is back.

---

## PR-8 — Agent `exemptions` Cat A → Cat B

### Scope

Fix the silent prod bug where agent.exemptions is currently registered as Cat A (`RegisterRaw`) but CP-side flow is Cat B (`InvalidateConfig`). Agent receives empty seed payload `{enabled:false}` forever; operator-added exemptions silently dropped.

**Zero impact today** (no agents enrolled in prod) — but fix proactively before first agent enrolls.

### Code changes

**1. Switch agent registration from `RegisterRaw` to `RegisterRawPull`**

`packages/agent/cmd/agent/configdispatch.go:87`:

```go
// OLD:
//   cfgloader.RegisterRaw(l, "exemptions", d.Exemptions)
// NEW:
cfgloader.RegisterRawPull(l, configkey.Exemptions, d.Exemptions)
```

**2. Add Hub Cat-B loader for agent.exemptions**

New file `packages/nexus-hub/internal/compliance/catbagent/exemptions.go`:

```go
package catbagent

import (
    "context"
    "encoding/json"
    "github.com/.../packages/shared/schemas/configtypes/identity"
)

type AgentExemptionsLoader struct {
    DB ExemptionStore  // narrow interface
}

func (l *AgentExemptionsLoader) Load(ctx context.Context, thingID string) ([]byte, error) {
    grants, err := l.DB.ListAgentExemptionGrants(ctx, thingID)
    if err != nil {
        return nil, err
    }
    payload := identity.AgentExemptions{
        AdminExemptions: grants,
        Denylist:        []string{}, // populated from policy table if needed
    }
    return json.Marshal(payload)
}
```

Register in `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go:46-57`:

```go
catBRegistry.Register("agent", "exemptions", &catbagent.AgentExemptionsLoader{DB: store.ExemptionStore()})
```

**3. Agent receiver schema**

Already at `packages/agent/internal/policy/exemption/store.go:225` — expects `{admin_exemptions:[], denylist:[]}` shape. Confirm matches Hub loader output.

### DB updates

```sql
-- Normalize the seed value to '{}' or null per Type B convention
UPDATE thing_config_template SET state = '{}', updated_at = now(), updated_by = 'configmigration-pr8'
WHERE type = 'agent' AND config_key = 'exemptions';
```

### Validation

```bash
# Once a real agent enrolls, force a Hub WS push:
ssh $HOST 'curl -X POST http://localhost:3060/api/internal/things/<agent-id>/config/exemptions'

# Confirm agent received update
# (smoke once an agent fleet exists)

# Static: hub starts up + audit reports zero missing receivers
ssh $HOST 'sudo journalctl -u nexus-hub --since "5 min ago" --no-pager | grep -i "orphan\|unknown"'
```

### Rollback

Pure code revert. PR-8 is the lowest-impact PR since prod has no agents.

---

## PR-9 — Drift reconciler expansion + skill docs

### Scope

Add high-value emergency keys to drift reconciler. Update prod-deploy + prod-debug skills.

### Code changes

**`packages/control-plane/cmd/control-plane/wiring/reconcile.go`**: extend the watch list:

```go
configWatcher.Watch([]configreconcile.Spec{
    {ThingType: "ai-gateway",       ConfigKey: configkey.Cache,              Loader: ...},
    {ThingType: "agent",            ConfigKey: configkey.AgentSettings,      Loader: ...},
    {ThingType: "ai-gateway",       ConfigKey: configkey.GatewayPassthrough, Loader: ...},   // ← added (emergency passthrough)
    {ThingType: "compliance-proxy", ConfigKey: configkey.Killswitch,         Loader: ...},   // ← added (safety)
    {ThingType: "agent",            ConfigKey: configkey.Killswitch,         Loader: ...},   // ← added (PR-6 wired agent fan-out; reconciler ensures eventual consistency)
})
```

Drop `gateway_settings` from the watch list (orphan key with no receiver).

### Skill updates

**`.claude/skills/prod-deploy/SKILL.md`**: insert new Step 5.5 between binary install and service restart:

```markdown
## Step 5.5 — Env-var preflight (MANDATORY — added 2026-05-20)

Before kill-then-start, verify the prod EnvironmentFile contains all required variables.

\`\`\`bash
ssh $HOST 'sudo cat /etc/systemd/system/nexus-*.service.d/env.conf 2>/dev/null || sudo cat /etc/nexus-gateway/env' > /tmp/prod-env-snapshot

# Required [MUST MATCH] secrets — must be IDENTICAL across all services that need them
grep -E '^(INTERNAL_SERVICE_TOKEN|ADMIN_KEY_HMAC_SECRET|CREDENTIAL_ENCRYPTION_KEY|COMPLIANCE_PROXY_API_TOKEN)=' /tmp/prod-env-snapshot | sort -u
# Expected: 4 lines, each unique.

# Required infrastructure URLs
grep -E '^(DATABASE_URL|REDIS_MODE|NATS_URL|NEXUS_HUB_URL)=' /tmp/prod-env-snapshot
# Expected: at least these 4 present.

# Verify CONTROL_PLANE_URL is GONE (renamed to NEXUS_HUB_URL in 2026-05-20 refactor)
grep '^CONTROL_PLANE_URL=' /tmp/prod-env-snapshot && echo "⚠ STOP: CONTROL_PLANE_URL still present, must remove"
\`\`\`

If any check fails, abort and fix the EnvironmentFile before proceeding.
```

**`.claude/skills/prod-debug/SKILL.md`**: add to "Common failure patterns" table:

```markdown
| 401/403 between services after deploy | check EnvironmentFile [MUST MATCH] env vars | One of `INTERNAL_SERVICE_TOKEN` / `ADMIN_KEY_HMAC_SECRET` / `CREDENTIAL_ENCRYPTION_KEY` / `COMPLIANCE_PROXY_API_TOKEN` drifted between services. Re-issue from secrets manager and restart all services. |
| Service won't register as Thing | check EnvironmentFile `NEXUS_HUB_URL` | If `CONTROL_PLANE_URL` is set instead (pre-2026-05-20 name), the new binary won't read it. Rename to `NEXUS_HUB_URL`. |
```

### Architecture doc references

**`CLAUDE.md`**: under "Mandatory rules", insert binding cite:

```markdown
- **Configuration changes go through `configuration-architecture.md`.** Any edit that adds/removes/renames a yaml field, env variable, or thing_config_template key MUST conform to the 4-layer model + R1-R5 invariants in `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md`. The per-key catalog in §7 is authoritative — adding a new key requires updating that table + `packages/shared/schemas/configkey/` in the same PR.
```

**`docs/developers/architecture/README.md`**: add row:

```markdown
| Editing yaml / env / thing_config_template / system_metadata config | docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md + configuration-architecture-migration.md |
```

### Validation

```bash
npm run check:arch-doc-triggers  # enforces lockstep
```

### Rollback

Pure doc + reconciler config revert.

---

## Production deployment runbook (Day-Of)

1. **Pre-flight** (T-30min)
   - Run pg_dump backup, verify > 100 MiB.
   - Verify no in-flight admin UI sessions making edits (check `audit_log` last 5 min).
   - Confirm no other team is mid-deploy.

2. **PR-0** (T+0min) — Dead cleanup. Backup. SQL. Binary deploy. Smoke. ~15min total.
3. **PR-1** (T+30min) — Constants. No DB change. Binary deploy. Smoke. ~10min.
4. **PR-2** (T+45min) — yaml/env align. Update EnvironmentFile FIRST. Binary deploy. Verify all 4 (5 incl Hub) services online. ~30min (the env edit + restart + verify cycle).
5. **PR-3** (T+1h30min) — Redis factory. Update EnvironmentFile. Binary deploy. ~30min.
6. **PR-4** (T+2h) — Renames. Backup. SQL. Binary deploy. UI deploy. Smoke. ~30min.
7. **PR-5** (T+2h30min) — Shape fixes. SQL + minor code. ~15min.
8. **PR-6** (T+3h) — Fan-out fixes. Code only. ~10min.
9. **PR-7** (T+3h15min) — yaml downgrades. **CRITICAL ordering**: edit prod yaml first, then deploy binary, then SQL. ~30min.
10. **PR-8** (T+3h45min) — Agent exemptions Cat B. Hub + agent binary. ~15min.
11. **PR-9** (T+4h) — Reconciler + skill docs. Code + docs. ~10min.

**Total estimated deploy window: ~4 hours.** Includes 30min smoke between each.

**Smoke between every PR**:

```bash
tests/scripts/smoke-gateway.py --models claude-sonnet-4-6 --no-cache
```

**Full smoke after PR-9**:

```bash
tests/scripts/smoke-gateway.py --all-ingress
```

---

## Decision log

| Q | Default | Operator decision | Date | Rationale |
|---|---|---|---|---|
| 1. access_control.sourceIpAllowlist | RFC1918 | applied (downgraded to yaml `accessControl:`) | 2026-05-20 | Yaml-only per SRE-tier authority; prod override removed during PR-7. |
| 2. agentUpdateTarget | delete | **PENDING** | — | Auto-updater feature is unscheduled; constant + handler still present in code, no seed row, no admin write path. Resolve before any future agent self-update epic begins. |
| 3. agent.killswitch | **fix (mandatory)** | fix | 2026-05-19 | Safety-critical per CLAUDE.md NE fail-open binding. agent killswitch is the only fleet-wide kill of TLS bumping. Reverted [R]=delete after user pushback. Schema fixed to `{enabled: bool}`. |
| 4. agent.observability | delete | deleted | 2026-05-20 | OTEL config kept yaml-only + standard OTEL env vars. |
| 5. agent.log_level | delete | deleted | 2026-05-20 | log_level remains env/yaml-tier, not admin-tier. |
| 6. agent.timing_intervals | delete + consolidate | deleted (consolidated into `agent_settings`) | 2026-05-20 | `agentSettingsApply` reads `heartbeatIntervalSec` / `auditDrainIntervalSec` / `configSyncIntervalSec` directly. |

---

## Acceptance checklist (post-deploy)

- [x] `SELECT count(*) FROM thing_config_template;` returns ≤ 39 rows (deleted keys absent from `seed-baseline.sql`; new E61/E72 keys add a few back, balance still within target).
- [x] `SELECT count(*) FROM thing_config_override WHERE config_key IN ('forward_headers_config','upstream_timeouts','access_control');` returns 0 (no rows in `seed-baseline.sql`).
- [x] Hub startup log shows zero "unknown config_key" / "orphan template row" warnings (verified via `configkey.AuditTemplateRows` + live `ValidByThingType`).
- [x] Hub Cat-B registry contains `agent/exemptions` entry (re-registered as `RegisterRawPull`).
- [x] `CLAUDE.md` cites `configuration-architecture.md` as binding ("Configuration changes go through `configuration-architecture.md`" rule).
- [ ] `prod-deploy SKILL.md` has Step 5.5 (env preflight) — verify in skill catalog before closing this item.
- [ ] `prod-debug SKILL.md` has env-drift failure pattern — verify in skill catalog before closing this item.
- [ ] Full smoke green: `tests/scripts/smoke-gateway.py --all-ingress` (run on each release).
- [ ] No prod alerts firing in the 30min after the deploy window closes (per-release check).

---

## Appendix A — Files most touched (post-PR landed reference)

The list below is the live "where to look" map; the original pre-merge LOC estimate has been removed (the work has shipped, the exact LOC delta no longer carries operational value).

- `tools/db-migrate/seed/data/seed-baseline.sql` — single source for the post-migration template + override rows.
- `packages/shared/schemas/configkey/{configkey.go,validation.go,typed.go}` — constants + `ValidByThingType` + `TypedRegistry` + startup audit.
- `packages/agent/cmd/agent/configdispatch.go` — registrations for the surviving agent keys.
- `packages/agent/cmd/agent/configappliers.go` — appliers for the surviving agent keys.
- `packages/compliance-proxy/cmd/compliance-proxy/configdispatch/configdispatch.go` — compliance-proxy receivers (incl. `streaming_compliance` registered in PR-6).
- `packages/control-plane/internal/governance/{killswitch,passthrough,exemptions,aiguard}/handler/handler.go` — admin endpoints + CP→Hub fan-out.
- `packages/control-plane/internal/settings/handler/settings/observability.go` — observability fan-out.
- `packages/control-plane/internal/platform/configreconcile/reconcile.go` — CP-side drift reconciler.
- `packages/shared/storage/redisfactory/` — universal Redis factory (PR-3).
