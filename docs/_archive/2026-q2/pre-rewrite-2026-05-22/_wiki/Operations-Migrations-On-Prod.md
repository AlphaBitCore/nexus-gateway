# Operations Migrations On Prod

*Audience: operators and maintainers applying database migrations to a running production deployment.*

Applying Prisma migrations to a production Nexus Gateway instance follows a strict sequencing discipline: the `prod-deploy` skill orchestrates building and uploading binaries, applying schema migrations, restarting services in dependency order, and running verification queries. The [`prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md) runbook is the single source of truth for pending migrations in each release cycle — the skill reads it before touching the database.

---

## Pre-flight checklist

Before every production deploy involving database changes:

1. Open `docs/operators/ops/runbooks/prod-deploy-data-changes.md` and confirm the migration list in Section 1 is accurate for this release.
2. SSH to the production host and establish the baseline counters:

```bash
HOST=${PROD_SSH_TARGET}
ssh $HOST 'PGPASSWORD=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed "s|.*://[^:]*:||;s|@.*||") \
  psql -h localhost -U nexus -d nexus_gateway -c \
  "SELECT COUNT(*) AS thing_count FROM thing;
   SELECT COUNT(*) AS credential_count FROM \"Credential\";
   SELECT migration_name FROM _prisma_migrations ORDER BY migration_name DESC LIMIT 5;"'
```

3. Confirm the most-recent migration rows match the expected baseline from `prod-deploy-data-changes.md`. If any expected migration is missing, stop and reconcile before proceeding.
4. Take a full PostgreSQL backup before any schema change:

```bash
TS=$(date -u +%Y%m%dT%H%M%SZ)
ssh $HOST "PGPASSWORD=\$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed 's|.*://[^:]*:||;s|@.*||') \
  pg_dump -h localhost -U nexus -d nexus_gateway -Fc \
  > /home/ec2-user/db-backups/nexus_gateway-pre-deploy-${TS}.dump"
```

---

## Deploy sequence

The `prod-deploy` skill automates this sequence. The numbered steps below document what the skill does, so operators can intervene at any point.

1. **Build all binaries** on the local machine: Hub, Control Plane, AI Gateway, Compliance Proxy, and the Control Plane UI bundle.

2. **Upload binaries to the production host** via `scp`.

3. **Apply Prisma migrations** (run from `tools/db-migrate/` on the production host):

```bash
ssh $HOST "cd /opt/nexus/app/tools/db-migrate && \
  DATABASE_URL=\$(grep ^DATABASE_URL /etc/nexus-gateway/env | cut -d= -f2-) \
  npx prisma migrate deploy"
```

   Migration output includes one line per applied migration. The `_prisma_migrations` table records each applied migration; Prisma skips already-applied migrations idempotently.

4. **Stop services in reverse-dependency order**:

```bash
ssh $HOST "systemctl stop nexus-compliance-proxy nexus-aigw nexus-control-plane nexus-hub"
```

5. **Replace binaries** — copy the new executables to the deployment directory and update symlinks.

6. **Start services in dependency order**:

```bash
ssh $HOST "
  systemctl start nexus-hub
  sleep 3
  systemctl start nexus-control-plane
  sleep 3
  systemctl start nexus-aigw
  sleep 3
  systemctl start nexus-compliance-proxy
"
```

7. **Run any post-migration data scripts** listed in Section 3 of `prod-deploy-data-changes.md`. These are manual SQL scripts (chunked PL/pgSQL for large tables, idempotent) that must run after the new binaries are live. The scripts and their ordering constraints are documented inline in the runbook.

8. **Smoke check** — run a targeted gateway smoke to confirm the deployed binaries and schema are functioning:

```bash
python3 tests/scripts/smoke-gateway.py \
  --target prod \
  --models gpt-4o \
  --no-all-ingress
```

---

## Migration sequencing rules

Several sequencing constraints recur across releases:

**Binary-first migrations (tables new binaries depend on):** Apply the migration before stopping services only if the migration is additive (new table, new nullable column, new index). PostgreSQL `ADD COLUMN` with a constant default is metadata-only and does not block reads.

**Binary-first drops (tables old binaries still read):** Deploy new binaries first, verify they are healthy, then run the `DROP TABLE` migration. Running a drop before the binary swap may cause the old gateway to error on its next snapshot reload.

**Historical data scripts must run after binary deploy:** Data recompute scripts (e.g., cost recompute) typically reference current Model row prices that are backfilled by a Prisma migration. The correct order is: apply Prisma migrations → deploy binaries → run data scripts.

---

## Verification after deploy

Run the Section 5 queries from `prod-deploy-data-changes.md`:

```sql
-- Confirm migrations applied
SELECT migration_name FROM _prisma_migrations ORDER BY migration_name DESC LIMIT 5;

-- Confirm all Things are online at the new version
SELECT id, type, status, version FROM thing
WHERE id LIKE '%-ip-172%'
ORDER BY type;

-- Confirm traffic is flowing (last 5 minutes)
SELECT COUNT(*) FROM traffic_event WHERE timestamp > NOW() - INTERVAL '5 minutes';
```

Check health endpoints:

```bash
curl -s http://<hub-host>:3060/readyz
curl -s http://<cp-host>:3001/healthz
curl -s http://<aigw-host>:3050/healthz
curl -s http://<proxy-host>:3040/healthz
```

---

## Rollback

If the deploy fails at any step:

**Before any database change:** restart the old binaries. No schema was modified; rollback is a service restart.

**After schema migration, before data scripts:** restore from the pre-deploy `pg_dump`. The Prisma migration is the only schema change; the backup restores the previous schema state.

**After data scripts:** restore from the specific table backups documented in the runbook's Section 3 (typically `traffic_event` and rollup tables). Not all data scripts are reversible without a backup.

Rollback command pattern:

```bash
ssh $HOST "PGPASSWORD=... pg_restore -h localhost -U nexus -d nexus_gateway \
  -Fc --clean --if-exists /home/ec2-user/db-backups/nexus_gateway-pre-deploy-<TS>.dump"
```

---

## Migration timestamp uniqueness

Two migration folders sharing the same `YYYYMMDDHHMMSS` prefix cause Prisma to silently skip one. Before creating a new migration:

```bash
ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d
# Must return empty
```

A non-empty result means a prefix collision exists and must be resolved before running `prisma migrate dev`.

---

## Canonical docs

- [`prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md) — living checklist of pending migrations and data scripts for each release cycle
- [`db-migration-mechanics-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md) — Prisma migration mechanics, baseline strategy, and in-flight migration lifecycle

**Adjacent wiki pages**: [Operations Runbook Index](Operations-Runbook-Index) · [Operations Backup Restore](Operations-Backup-Restore) · [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) · [Deployment Database Migrations](Deployment-Database-Migrations) · [Operations FAQ](Operations-FAQ)
