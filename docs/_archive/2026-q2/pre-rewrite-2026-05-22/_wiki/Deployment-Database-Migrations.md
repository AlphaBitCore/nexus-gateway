# Deployment Database Migrations

Nexus Gateway uses Prisma for dev-time schema authoring and migration generation, and hand-written pgx queries at runtime — Prisma Client is never used by the services themselves. The schema lives at `tools/db-migrate/schema.prisma`; migrations are in `tools/db-migrate/migrations/`; runtime service types mirror the schema as hand-maintained Go structs under `packages/shared/schemas/configtypes/`. This page covers the migration workflow, the binding timestamp-uniqueness invariant, production application, and the seed ordering constraint that causes hard-to-debug failures on fresh deploys.

---

## The two-layer model

| Layer | Tool | Purpose |
|---|---|---|
| Dev-time | Prisma | Author migrations, manage schema, and generate the SQL diff |
| Runtime | Hand-written SQL + pgx | Service code reads and writes the database |

Services do not import Prisma Client. The Go types that mirror Prisma models are hand-maintained under `packages/shared/schemas/configtypes/` (split into `enums/`, `identity/`, `interception/`, `observability/`, `policy/`). When `schema.prisma` changes, the corresponding Go struct must be updated in the same commit — the code/doc lockstep rule applies.

---

## Directory layout

```
tools/db-migrate/
  schema.prisma                  # source of truth for all tables
  migrations/
    00000000000000_baseline/     # collapsed baseline migration
    20260513103821_xyz/          # one folder per migration; YYYYMMDDHHMMSS prefix
      migration.sql
  seed/
    seed.ts                      # canonical seed (dev DB + baseline data)
    data/
      seed-baseline.sql          # baseline data snapshot (credentials redacted)
  manual-scripts/                # one-off operations; not part of the migration sequence
```

---

## Adding a migration

```bash
cd tools/db-migrate
$EDITOR schema.prisma                  # edit the schema
npx prisma migrate dev --name <name>   # generates migration folder + SQL diff
npm run check:migration-timestamps     # verify timestamp uniqueness (see below)
# Then update the matching Go struct(s) under packages/shared/schemas/configtypes/
```

`prisma migrate dev` creates a folder like `20260520120000_my_migration/` with a `migration.sql` diff inside.

---

## Timestamp uniqueness (binding)

Every migration folder's name starts with a 14-character `YYYYMMDDHHMMSS` prefix. Two folders sharing the same prefix cause Prisma to silently skip one migration — skipped migrations in production produce schema drift that can be invisible until a query fails.

The pre-commit hook and CI both run:

```bash
ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d
# must be empty
```

When creating a migration by hand or copying an existing one, run this check before committing. Automated `prisma migrate dev` generates unique timestamps by construction; the risk is copy-paste.

---

## Applying migrations in production

Migrations are applied with `prisma migrate deploy`. The command is non-interactive and idempotent: pending migrations are applied; already-applied ones are skipped. The `_prisma_migrations` table tracks state.

```bash
cd tools/db-migrate
npx prisma migrate deploy
```

Services do not auto-apply migrations at startup. The schema must be ready before any service starts. If migrations are applied while services are running and the schema change is additive (new column, new table), running services continue without restart; non-additive changes require a coordinated deploy.

For SSH-tunneled access to production databases:

```bash
ssh -L 5555:localhost:5432 deploy-user@<prod-ip> -N &
export DATABASE_URL="postgresql://nexus:PASSWORD@localhost:5555/nexus_gateway?sslmode=disable"
cd tools/db-migrate && npx prisma migrate deploy
```

---

## The seed ordering constraint

The seed at `tools/db-migrate/seed/seed.ts` registers foundational data including the local `IdentityProvider` row. The Control Plane reads this row at startup (`idps.GetLocal()`) to register the `/authserver/password` route. If the seed has not run when the Control Plane starts, the route is permanently absent for that process lifetime — password login returns `404` with no further error.

**Fix**: run the seed before starting the Control Plane, or restart the Control Plane after the seed completes.

```bash
npx prisma migrate deploy
npx prisma db seed
sudo systemctl restart nexus-control-plane
```

The seed is idempotent: running it on a populated database does not break data.

The seed also registers the `cp-ui` OAuth client with the `redirectUris` array. If the production URL is absent from that array, the PKCE authorize flow returns `redirect_uri not registered`. Add it to `tools/db-migrate/seed/auth-seed.ts` and re-run the seed, or patch the DB row directly.

---

## Manual scripts

`tools/db-migrate/manual-scripts/` holds one-off operations that are not part of the Prisma migration sequence — data fixup scripts, post-reset table sync, historical repairs. Each script has a companion `.md` explaining its purpose and when to run it. Manual scripts are applied explicitly by operators, never automatically.

---

## Go struct alignment

When `schema.prisma` changes, update the matching Go struct in `packages/shared/schemas/configtypes/<category>/`. The five categories are:

| Sub-package | Contents |
|---|---|
| `enums/` | Cross-domain enums (status codes, event kinds) |
| `identity/` | `OAuthClient`, `IdentityProvider`, `RefreshToken` |
| `interception/` | `InterceptionDomain`, kill-switch types |
| `observability/` | `MetricOpsRaw`, `ThingDiagEvent`, rollup types |
| `policy/` | `HookConfig`, `AIGuardConfig`, rule pack types |

If the new schema field carries a shadow-key constant (stored in Hub shadow), also update `packages/shared/schemas/configkey/` (constants + `ValidByThingType` + `TypedRegistry`).

---

## Canonical docs

- [`db-migration-mechanics-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md) — authoritative mechanics: the two-layer model, timestamp uniqueness incident, seed details, and why sqlc is not used
- [`ec2-single-node.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/ec2-single-node.md) — database setup and the seed ordering constraint in the deployment checklist

**Adjacent wiki pages**: [Deployment-Single-Node-Production](Deployment-Single-Node-Production) · [Deployment-Environment-Variables](Deployment-Environment-Variables) · [Operations-Migrations-On-Prod](Operations-Migrations-On-Prod) · [Configuration-Architecture](Configuration-Architecture)
