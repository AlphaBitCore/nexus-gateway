---
doc: db-migration-mechanics-architecture
area: cross-cutting
service: storage
tier: 1
updated: 2026-05-20
---

# DB Migration Mechanics Architecture

> **Tier 3 architecture doc.** Read when adding a migration, debugging migration application, or designing a manual script. The binding rule on timestamp uniqueness lives in `.cursor/rules/migration-timestamp-unique.mdc`. The hand-written-SQL + Prisma-codegen pattern is canonical (no `sqlc`).

---

## 1. The two-layer model

| Layer | Tool | Purpose |
|---|---|---|
| Dev-time | Prisma | Author migrations + manage schema + generate Go types |
| Runtime | Hand-written SQL + pgx | Service code reads/writes DB |

Prisma is **only** dev-time. Services do NOT use Prisma Client — Go services read with `pgx` and types generated from the Prisma schema.

## 2. Where things live

```
tools/db-migrate/
  schema.prisma             # source of truth for tables (lives at db-migrate root, not under prisma/)
  prisma.config.ts          # Prisma TS config
  migrations/
    00000000000000_baseline_2026_05_13/   # collapsed baseline (cross-ref memory project_db_baseline_reset_2026_05_13)
      migration.sql
    20260513103821_xyz/    # one folder per migration; YYYYMMDDHHMMSS prefix
      migration.sql
    ...
  seed/
    seed.ts                            # canonical seed (dev DB + baseline data)
    data/
      seed-baseline.sql                # baseline dev snapshot loaded by seed.ts (Credential rows redacted)
      time-sensitive-rules.json
      README.md
  manual-scripts/           # one-off operations; not part of the migration sequence
  test/                     # db-migrate test fixtures
```

Note: on prod the seed.ts loader pulls `prod-data.sql` instead of `seed-baseline.sql` (cross-ref memory `project_db_baseline_reset_2026_05_13`).

## 3. Adding a migration

```bash
cd tools/db-migrate
$EDITOR schema.prisma                 # edit the schema (lives at db-migrate root)
npx prisma migrate dev --name <name>  # auto-generates the migration folder + SQL under migrations/
npm run check:migration-timestamps    # verify uniqueness
# Then hand-update the matching Go struct(s) under packages/shared/schemas/configtypes/<category>/
# so the Go side stays aligned with the schema.
```

`prisma migrate dev` autocreates a folder like `20260520120000_my_migration/` with the SQL diff inside.

## 4. Timestamp uniqueness (binding)

Two folders sharing the YYYYMMDDHHMMSS prefix → Prisma silently skips one (2026-05-14 prod 16-hour audit-gap incident). The check enforces:

```bash
ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d
# must be empty
```

If you craft a folder by hand or copy-paste, run the check before committing.

## 5. Go schema types — hand-maintained

Go struct types that mirror Prisma models live in `packages/shared/schemas/configtypes/`, split by domain into 5 sub-packages (plus a top-level `doc.go`):

```
packages/shared/schemas/configtypes/
  doc.go           # package-level documentation
  enums/           # cross-domain enums (BumpStatus, etc.)
  identity/        # OAuthClient, IdentityProvider, RefreshToken, ...
  interception/    # InterceptionDomain, InterceptionPath, killswitch, ...
  observability/   # MetricOpsRaw, ThingDiagEvent, MetricOpsRollup1h, ...
  policy/          # HookConfig, RulePack, AIGuardConfig, ...
```

Each sub-package declares `package <subdomain>` and is imported as
`schemas/configtypes/policy`, etc. Services use these types in hand-written SQL
queries with pgx.

`schemas/configtypes/` is one of four sibling subpackages under `packages/shared/schemas/`. The other three carry adjacent type concerns (kept distinct so the configtypes wave doesn't bloat):

- `schemas/configkey/` — Cat A / Cat B shadow-key constants + `ValidByThingType` + `TypedRegistry` registry (binding under the configuration-architecture rule).
- `schemas/credstate/` — per-credential Redis-key + circuit/health enum constants (single source of truth for the credstate dirty-set; cross-ref `credentials-architecture.md`).
- `schemas/domain/` — domain matcher input types shared between policy/domain and consumer code.
- `schemas/thingtype/` — canonical ThingType enum + helpers (`agent`, `ai-gateway`, `compliance-proxy`, `control-plane`, `nexus-hub`).

When you add a new Prisma model, update **both** the relevant `configtypes/<category>/` file (Go struct) and, if it carries a shadow-key constant, the matching `configkey/` entry.

Historically these files were emitted by a `codegen-go.mjs` script. That script
was removed 2026-05-19 because the P2.7 reorg moved files into the 5 sub-packages
above (with sub-package names, not the flat `package configtypes` the script
emitted), and re-running the generator would create a stray flat package at the
wrong path instead of updating the real code. The structs are now updated by
hand alongside any `schema.prisma` change. The schema is small enough that
hand-sync is faster than maintaining a re-bucketing generator.

## 6. Why no `sqlc`

`sqlc` parses SQL queries and generates Go code from them. We don't use it because:

1. The hand-maintained Go structs under `schemas/configtypes/` already cover the type side.
2. SQL queries are often dynamic (built up from filter conditions); `sqlc` doesn't help much.
3. One way to maintain Go ↔ schema alignment is simpler than two.

If a service has many repetitive queries, the developer authors a query helper in the service's `internal/store/` package — not via `sqlc`.

## 7. Seed

`tools/db-migrate/seed/seed.ts`:

```typescript
import { PrismaClient } from '@prisma/client';
const prisma = new PrismaClient();
async function main() {
  // canonical org / role / policy seeding
  await prisma.organization.create({ data: { id: 'nexus', ... } });
  // ...
  // load seed baseline (Credential rows redacted)
  const sql = readFileSync('seed/data/seed-baseline.sql', 'utf-8');
  await prisma.$executeRawUnsafe(sql);
}
```

The seed is idempotent — running it on a populated DB doesn't break.

## 8. Seed baseline

`tools/db-migrate/seed/data/seed-baseline.sql` is a snapshot of prod data with credentials redacted. It's used:

- To bootstrap a fresh dev DB with a realistic starting state.
- To stand up a staging environment.
- To verify migrations against realistic data shapes.

When prod data changes substantially, the baseline is refreshed (manual op).

## 9. Manual scripts

`tools/db-migrate/manual-scripts/` holds one-off operations:

- `sync-prisma-migrations-table-from-prod-baseline.sql` (post-baseline-reset; cross-ref memory `project_db_baseline_reset_2026_05_13`).
- Data fixup scripts (e.g., the 2026-05-13 HookConfig onMatch alignment).

Manual scripts are NOT part of the Prisma migration sequence — they're run explicitly by ops. Documented per-script with a checked-in `.md` companion.

## 10. The migration application path

In prod, migrations are applied by:

```bash
cd tools/db-migrate
npx prisma migrate deploy  # apply pending migrations
```

`prisma migrate deploy` is non-interactive and idempotent — pending migrations are applied; already-applied ones are skipped. The `_prisma_migrations` table tracks state.

Runtime services do NOT auto-apply migrations. They expect the schema to be ready.

## 11. Cross-references

- `.cursor/rules/migration-timestamp-unique.mdc` — IDE binding.
- `scripts/check-migration-timestamps.sh` — CI lint.
- Memory: `feedback_migration_timestamp_unique` — incident scar.
- Memory: `project_db_baseline_reset_2026_05_13` — baseline mechanics retrospective.
- `tenancy-architecture.md` — schema-level ancestor-path materialisation.
- `audit-pipeline-architecture.md` §11 — partitioning option.
