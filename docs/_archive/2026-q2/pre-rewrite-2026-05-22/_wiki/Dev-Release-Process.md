# Dev Release Process

*Audience: maintainers deploying Nexus Gateway to production.*

Releases are tagged commits in the format `prod-YYYYMMDD`. The full deploy sequence — tag, build four Go services and the UI, upload to EC2, install binaries, restart services in dependency order, and verify — is encapsulated in the `/prod-deploy` Claude Code skill. This page explains the overall process, the mandatory steps, migration sequencing, and the rollback procedure.

---

## Release tagging

Every production release is an annotated git tag:

```bash
DATE=$(date +%Y%m%d)
git tag -a prod-${DATE} -m "prod release ${DATE} — <one-line summary>"
```

The tag determines the `buildVersion` string embedded in each binary (format: `prod-YYYYMMDD@<short-sha>`). Tags are created on the commit being deployed; they are not pushed separately — the deploy itself confirms what shipped.

The active releases log is in [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md). The [Release History](Release-History) wiki page summarizes what each tag delivered.

---

## Build and deploy sequence

The full deploy runs via the `/prod-deploy` skill (`.claude/skills/prod-deploy/SKILL.md`). The eight steps must be followed in order:

1. **Read the data-change runbook.** Open `docs/operators/ops/runbooks/prod-deploy-data-changes.md` and work through Section 0 (pre-flight), Section 1 (migrations), and Section 2 (required data inserts) before touching any binary. This is the single source of truth for every schema migration and one-shot data fix the release needs.

2. **Database backup.** Before any migration, run `pg_dump` on the production database and verify the dump is non-empty (> 1 MB compressed). If the backup fails for any reason, abort the deploy. A restore guide lives in the data-change runbook.

3. **Create the release tag.** `git tag -a prod-YYYYMMDD ...`

4. **Build all four Go services for Linux amd64.** Use `GOOS=linux GOARCH=amd64 go build` with `-ldflags "-X main.buildVersion=${VER}"`. Build in parallel; all four must succeed before proceeding.

5. **Build the UI.** `npm run build -w packages/control-plane-ui` then tar the `dist/` output.

6. **Upload and install.** `scp` binaries and UI tarball to EC2; install binaries under `/usr/local/bin/`; rsync the UI into `/var/www/nexus-ui/` with `--exclude='/downloads'` to preserve the agent `.pkg` download tree.

7. **Env-var preflight.** Before restarting services, confirm the production `EnvironmentFile` contains all required variables under the current names and none of the obsolete names. Stale env files cause services to silently fall back to empty secrets, producing 401s and decryption failures.

8. **Restart in dependency order.** Stop and start services in this sequence: Hub first, then Control Plane, then AI Gateway and Compliance Proxy together, then verify each is responding at its health endpoint. The correct systemd commands and verification steps are in the `/prod-deploy` skill.

9. **Post-deploy smoke.** The smoke is non-skippable. Every deploy must end with a passing smoke: Hub log is clean, `cp_curl /api/admin/analytics/summary` returns 200, all nodes report `online` in the registry, and the audit pipeline is flushing. Declaring success while smoke is red is not permitted.

---

## Migration sequencing

Migrations use Prisma and must follow these constraints:

- Apply migrations before restarting the Hub and Control Plane — those services assume the schema is current on boot.
- Migration timestamp prefixes must be unique. Duplicate prefixes cause Prisma to silently skip the second migration. The pre-commit check `npm run check:migration-timestamps` enforces this; verify the deploy branch also passes before building.
- For destructive migrations (column drops, table drops, type changes): the data-change runbook records the exact `prisma migrate deploy` command and any manual SQL needed. Never apply a destructive migration without the runbook open.

A migration on a running prod database follows this safe sequence: backup → apply migration → verify schema → restart services. If the migration fails, restore from the backup taken in step 2.

---

## Rollback procedure

Rollback is `git revert`, not a feature flag. Pre-GA, there are no installed users and no backward-compatibility shims. To roll back:

1. Identify the last good tag: `git log --oneline --tags='prod-*'`.
2. Check out the previous tag or a revert commit.
3. Rebuild the four binaries (Step 4) and upload/install (Steps 5-6).
4. If the migration is reversible, apply the rollback SQL from the runbook backup notes.
5. If the migration is destructive and irreversible, restore from the `pg_dump` backup (Step 2) and redeploy the prior binaries.
6. Run the post-deploy smoke to confirm the rollback is stable.

---

## Post-deploy verification summary

After a successful deploy, the `/prod-deploy` skill records four sub-step results:

- **7a** — all nodes online in the registry (Hub, Control Plane, AI Gateway, Compliance Proxy).
- **7b** — Hub log: no errors, fatals, or panics in the last minute.
- **7c** — `cp_curl /api/admin/analytics/summary` returns 200 with non-empty body.
- **7d** — Audit pipeline: no flush failures in the last 5 minutes.

All four must be green before the deploy is considered complete. If any is red, roll back and fix before declaring success.

---

## Canonical docs

- [`docs/operators/ops/runbooks/prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md) — single source of truth for migrations and data fixes per release
- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — in-flight and queued work tracker; records shipped releases

**Adjacent wiki pages**: [Dev Code Review Checklist](Dev-Code-Review-Checklist) · [Operations Migrations On Prod](Operations-Migrations-On-Prod) · [Deployment Database Migrations](Deployment-Database-Migrations) · [Release History](Release-History) · [Operations Runbook Index](Operations-Runbook-Index)
