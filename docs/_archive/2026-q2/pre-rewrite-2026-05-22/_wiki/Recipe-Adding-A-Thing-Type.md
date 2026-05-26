# Recipe Adding A Thing Type

*Audience: contributors registering a new managed service type with Nexus Hub.*

Every managed entity in Nexus Gateway is a Thing — a row in the `thing` table with a typed extension row, a config shadow (desired vs. applied state), and a heartbeat lifecycle. Adding a new Thing type means creating the extension table, registering the config keys it cares about (Cat A/B/C), and wiring `OnConfigChanged` callbacks in the service binary. The `add-shadow-key` skill (`Skill('add-shadow-key')`) automates the three-path consistency audit for any shadow key change. The canonical reference is [`thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) and [`configuration-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md).

---

## Background — the five current Thing types

| Thing type | Extension table | role |
|---|---|---|
| Hub (self) | `thing_service` | Platform ops center |
| Control Plane | `thing_service` | Admin API |
| AI Gateway | `thing_service` | `/v1/*` traffic |
| Compliance Proxy | `thing_service` | Transparent TLS proxy |
| Agent | `thing_agent` | Desktop endpoint binary |

Backend services share `thing_service`; desktop agents use `thing_agent`. A new managed service type typically shares `thing_service` unless it needs device-specific fields (OS, hostname, user ID).

---

## Step 1 — Decide the extension table

If the new Thing is a server-side service, reuse `thing_service` with a new `service_kind` constant. Add the constant to `packages/shared/schemas/configtypes/enums/` and to the `service_kind` CHECK constraint in the Prisma schema (`tools/db-migrate/schema.prisma`). Generate the migration:

```bash
cd tools/db-migrate
npx prisma migrate dev --name add_thing_service_kind_<name>
npm run check:migration-timestamps   # timestamps must be unique
```

If the new Thing is a client-side agent with device-specific metadata, create a new extension table (model `ThingYourType` in `schema.prisma`) with the device fields. Add the JOIN to Hub's Thing-registry queries.

## Step 2 — Define the config keys (Cat A/B/C)

Config keys fall into three categories:

| Category | Shape in shadow | When to use |
|---|---|---|
| **Cat A — inline** | Full value inlined in the shadow JSON | Small fast-path values (kill-switch toggle, a single boolean flag) |
| **Cat B — pull-on-signal** | `{ version, needsPull: true }` pointer | Mid-to-large configs (hook configs, routing rules, settings blobs); the Thing pulls the full value on receiving the signal |
| **Cat C — template-fallback** | Template default + optional per-Thing override | Defaults with per-Thing tuning |

**Cat B keys must carry `needsPull: true`** — this is binding. The #91 production incident was caused by Cat B keys registered without `needsPull`, leaving agents in silent drift.

Register each new key in `packages/shared/schemas/configkey/configkey.go`:

```go
const KeyYourNewKey = "your_service_kind.your_key_name"
```

Add the key to `ValidByThingType` and `TypedRegistry` in the same package.

## Step 3 — Update configreconcile (runtime path)

`packages/shared/configreconcile/` is the CP-side drift watchdog — it periodically compares the source-of-truth config tables against the corresponding `thing.desired.<key>` and re-emits `Hub.NotifyConfigChange` to heal divergence.

Add an entry for each new key:

```go
template[KeyYourNewKey] = configtypes.TemplateEntry{
    Category:  "B",
    NeedsPull: true,
    DefaultValue: ...,
}
```

This is path 1 of the three-path audit. Without it, freshly registered Things never receive the key in their initial desired state.

## Step 4 — Update seed.ts (factory defaults path)

Add the same key to `tools/db-migrate/seed/seed.ts` inside the `thing_config_template` seed block:

```typescript
{
  thingKind: '<service_kind>',
  key: 'your_service_kind.your_key_name',
  category: 'B',
  needsPull: true,
  defaultValue: JSON.stringify({ ... }),
}
```

This is path 2 of the three-path audit. Dev DBs and the prod-data baseline must start with the key.

## Step 5 — Write the migration (durable schema path)

Generate the migration that inserts the `thing_config_template` row for the new key:

```bash
cd tools/db-migrate
npx prisma migrate dev --name add_thing_config_template_<key>
npm run check:migration-timestamps
```

This is path 3 of the three-path audit. Verify all three paths reference the key:

```bash
grep -n 'your_key_name' packages/shared/configreconcile/   # path 1
grep -n 'your_key_name' tools/db-migrate/seed/seed.ts      # path 2
ls tools/db-migrate/prisma/migrations/ | grep your_key_name # path 3
```

## Step 6 — Wire OnConfigChanged in the service binary

Each Thing that cares about the new key must register a callback in its `main.go`. The callback validates the raw JSON, atomic-pointer-swaps the in-memory snapshot, and stamps the reported state:

```go
client.OnConfigChanged(configkey.KeyYourNewKey, func(ctx context.Context, raw json.RawMessage) error {
    var cfg YourKeySchema
    if err := json.Unmarshal(raw, &cfg); err != nil {
        return fmt.Errorf("invalid %s config: %w", configkey.KeyYourNewKey, err)
    }
    yourConfigSnapshot.Store(&cfg)
    return nil
})
```

Without this callback, the change-signal arrives but nothing updates locally — the service silently runs on stale config.

## Step 7 — Expose the new Thing type in the CP UI

The CP Infrastructure → Nodes page lists all registered Things. No code change is needed for the list view — the page reads from the `thing` table and shows all rows. However:

- Update the service-kind display label in `packages/control-plane-ui/src/` if the kind is new.
- If the new Thing type has type-specific detail fields, add a detail drawer section.
- Run the IAM impact review if any new admin API endpoint was added for the new type (see [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action)).

## Step 8 — Verify end-to-end

```bash
# Start Hub locally:
cd packages/nexus-hub && go run ./cmd/nexus-hub/

# Register a fresh Thing of the new type (start your service):
cd packages/<your_service> && go run ./cmd/<service>/

# Observe Hub logs for the shadow push:
# Expected: "thing shadow pushed" with your new key in the desired blob.

# Confirm the Thing appears in the CP UI Nodes page:
cp_login && cp_curl /api/admin/things?kind=<service_kind>
```

---

## What links break if you skip this

- **Missing configreconcile entry (path 1)**: Things registered after the initial seed never receive the key in their desired state. They boot with no config for that key, and the apply receipt never closes — the Config Sync page shows permanent drift.
- **Missing seed.ts entry (path 2)**: Dev DBs and fresh prod installs start without the key in `thing_config_template`. Hub's initial fan-out skips the key.
- **Missing needsPull on Cat B keys**: the change-signal includes only `{ version }` with no `needsPull: true`. Things receive the signal but never pull the full value — silent staleness. This is the exact pattern that caused the #91 production incident.
- **Missing OnConfigChanged callback**: the change-signal arrives and the apply receipt closes, but the in-memory snapshot never updates. The service continues using stale config with no error log.

---

## Canonical docs

- [`thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) — Thing abstraction, extension tables, shadow JSONB columns, three-path audit invariant
- [`configuration-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md) — 4-layer model, R1-R5 invariants, per-key catalog
- [`thing-config-sync-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md) — Cat A/B/C pull-on-signal mechanics

**Adjacent wiki pages**: [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Configuration Architecture](Configuration-Architecture) · [Hub Coordination](Hub-Coordination) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Recipe Index](Recipe-Index)
