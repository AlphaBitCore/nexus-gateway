---
name: prod-deploy
description: >
  Deploy a new version of all Nexus Gateway services to the production EC2
  instance (taskforce10x.com). Covers: git tag, build all 4 Go services +
  UI, upload to EC2, replace binaries, deploy UI, kill-then-start all
  services in correct order, verify nodes online, run smoke check.
  Also covers applying a specific DB migration without touching other data.
  Trigger keywords: deploy to prod, prod deploy, deploy prod,
  production release, push to prod, release to prod, /prod-deploy.
user-invocable: true
---

# Prod Deploy

Full deploy of Nexus Gateway to `ec2-user@18.204.174.212` (taskforce10x.com).

## ⚠ MANDATORY: post-deploy smoke is non-skippable (binding)

Every prod-deploy invocation MUST end with a **green** smoke verification — Step 7 in this skill. If Step 7 surfaces ANY of:

- Hub log errors / fatals / panics in the last minute (7b).
- `cp_curl /api/admin/analytics/summary` non-200 or empty (7c).
- Audit pipeline flush failures in the last 5 minutes (7d).
- Any node in the registry reporting `degraded` / `offline` after the deploy window (7a).

…the deploy is **NOT** complete. **Roll back, fix, redeploy** — do not declare success while smoke is red.

This binding exists because the 2026-05-14 incident shipped a schema-mismatched binary that left the audit pipeline silently broken for 16 hours. Every other check passed; only the audit-pipeline-alive smoke would have caught it (Step 7d was added in response). User-binding 2026-05-16 escalates this to "non-skippable for every deploy".

After a clean smoke, the closing report MUST include the 4 sub-step results explicitly so the user (and the audit trail) can verify smoke ran.

## When to use

- User says "deploy to prod", "release to prod", "push to prod", `/prod-deploy`
- After code changes are reviewed and ready to ship
- After a hotfix that must go live immediately

## Prod environment

| Item | Value |
|------|-------|
| EC2 IP | `18.204.174.212` |
| SSH user | `ec2-user` |
| Auth | passwordless (id_rsa.pub deployed to server) |
| UI root | `/var/www/nexus-ui` |
| Binaries | `/usr/local/bin/nexus-{hub,control-plane,ai-gateway,compliance-proxy}` |
| Service manager | systemd — units: `nexus-hub`, `nexus-control-plane`, `nexus-ai-gateway`, `nexus-compliance-proxy` |

### Public domains (nginx /etc/nginx/conf.d/nexus.conf)

| Domain | → backend | Service |
|---|---|---|
| `nexus.taskforce10x.com` | 127.0.0.1:3001 | Control Plane (admin BFF + SPA UI; SSO, /api/admin/*, OAuth, IdP, /api/public/agent-bootstrap) |
| `api.taskforce10x.com`   | 127.0.0.1:3050 | AI Gateway (OpenAI-compatible /v1/* — end-user LLM clients hit this) |
| `hub.taskforce10x.com`   | 127.0.0.1:3060 | Hub (WebSocket /ws for thingclient + /api/internal/* REST) |

NO `cp.taskforce10x.com` server_name in nginx — Control Plane is on `nexus.taskforce10x.com`.

Compliance Proxy (`nexus-compliance-proxy`, port 3040) is NOT behind public nginx; it's a TLS CONNECT intercept point reached directly by org-managed devices.

**Auth for prod API calls:**
```bash
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_curl "/api/admin/..."'
```
All prod credentials/URLs come from `tests/.env.prod`; the loader fails-closed
if it points at localhost.

## Step 0 — Walk every pending migration SQL (BEFORE binaries)

The 2026-05-22 docs rewrite archived
`docs/operators/ops/runbooks/prod-deploy-data-changes.md`. Source of
truth is now the migrations directory itself: compute the diff between
`tools/db-migrate/migrations/` and prod's `_prisma_migrations`, then
read each pending SQL file and classify it before touching prod.

```bash
# 1. List repo migrations and prod-applied migrations side by side.
ls tools/db-migrate/migrations/ | grep -v '\.toml$' | sort > /tmp/repo_migs.txt
ssh -o StrictHostKeyChecking=no ec2-user@18.204.174.212 \
  "PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -t -c \
   \"SELECT migration_name FROM _prisma_migrations ORDER BY migration_name;\"" \
  2>/dev/null | tr -d ' ' | sed '/^$/d' > /tmp/prod_migs.txt

echo "=== pending on prod ==="
comm -23 /tmp/repo_migs.txt /tmp/prod_migs.txt
```

For every line that's NOT `00000000000000_baseline_*` (the baseline
row is conventionally absent from a prod that was seeded fresh — leave
it alone), open the SQL and classify it:

| Category | What to look for | Risk |
|---|---|---|
| schema-add | `ADD COLUMN IF NOT EXISTS`, `CREATE TABLE IF NOT EXISTS`, new indexes | LOW — apply pre-binary |
| data-fill | `UPDATE …` with `COALESCE` or `WHERE … IS NULL`, `INSERT … ON CONFLICT DO NOTHING` | MEDIUM — apply pre-binary, verify row counts |
| destructive | `DROP TABLE`, `DROP COLUMN`, `ALTER … DROP`, `DELETE` | HIGH — check migration comment for binary-ordering hint; some MUST run AFTER kill-then-start |
| seed-needed | column added with `DEFAULT '{"rules":[]}'` or similar empty default, AND the matching seed JSON in `tools/db-migrate/seed/data/` populates real rows | MEDIUM — migration alone is not enough; replay the seed.ts UPSERT |

Two recurring traps:

- **`semantic_cache_config.time_sensitive_overrides`** (column added by
  `20260602000000_e61_semantic_cache_overrides_org`) defaults to
  `{"rules":[]}`. The real 11-rule set lives in
  `tools/db-migrate/seed/data/time-sensitive-rules.json` and is
  populated by `seed.ts`. seed.ts does NOT run during prod-deploy —
  replay the same idempotent UPSERT manually:

  ```bash
  HOST=ec2-user@18.204.174.212
  JSON_RAW=$(cat tools/db-migrate/seed/data/time-sensitive-rules.json)
  WRAP=$(mktemp); cat >"$WRAP" <<EOF
  BEGIN;
  UPDATE semantic_cache_config
     SET time_sensitive_overrides = \$tsr\$${JSON_RAW}\$tsr\$::jsonb,
         updated_at = NOW(),
         updated_by = 'prod-deploy-manual-seed'
   WHERE id = 'singleton'
     AND (time_sensitive_overrides IS NULL
          OR jsonb_array_length(COALESCE(time_sensitive_overrides->'rules', '[]'::jsonb)) = 0);
  COMMIT;
  EOF
  scp -q "$WRAP" $HOST:/tmp/seed-tsr.sql; rm -f "$WRAP"
  ssh $HOST "PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -v ON_ERROR_STOP=1 -f /tmp/seed-tsr.sql && rm /tmp/seed-tsr.sql"
  ```

- **A migration whose comment says "MUST run AFTER … gateway replicas"**
  (e.g. `20260608000001_drop_provider_pricing_table`) — HOLD it until
  after Step 6 kill-then-start. Old gateway code still reads the table
  during snapshot reload; dropping pre-restart would crash all live
  ai-gateways with a SQLSTATE error.

After classification, apply each migration via `apply-migration.sh`
(see "Applying migrations" section below) — that script propagates psql
exit codes through ssh, wraps SQL in `BEGIN/COMMIT`, and records each
in `_prisma_migrations` with a real SHA-256 checksum so future
`prisma migrate deploy` runs see no drift.

## Step 0a — MANDATORY: pg_dump backup BEFORE any DB change (BINDING)

**Hard rule, no waiver in chat is sufficient.** Before applying any
migration, manual SQL, or one-shot data fix in Section 1/2/3 of the
runbook above, take a full `pg_dump` of the prod database and verify
the dump is non-empty. If the backup fails for ANY reason, abort the
deploy — do not proceed to schema or data changes.

Rationale: every production incident in this repo's history that
required a restore happened because the deploy applied a destructive
migration and there was no recent backup to roll back to. The pg_dump
takes ~30 s and a few hundred MB; that is cheap compared to a half-
day of data reconstruction.

```bash
HOST=ec2-user@18.204.174.212
DATE=$(date +%Y%m%d)
# Use the same release tag the deploy will use; if Step 1 already created
# prod-${DATE}, this filename will line up with it for easy correlation.
TAG="prod-${DATE}"
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_REMOTE="/home/ec2-user/db-backups/nexus_gateway-${TAG}-${TS}.dump.gz"

# Run pg_dump on the EC2 box (db is local to the host). Custom format would
# be faster to restore selectively, but plain SQL + gzip is portable, easy
# to grep, and survives a Prisma version mismatch on restore. Keep it simple.
ssh -o StrictHostKeyChecking=no $HOST "\
  mkdir -p /home/ec2-user/db-backups && \
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM pg_dump \
    -h localhost -U nexus -d nexus_gateway \
    --no-owner --no-privileges --clean --if-exists \
    | gzip -9 > ${BACKUP_REMOTE}"

# Verify: file exists + size > 1 MB (a freshly-seeded prod db is tens of
# MB compressed; anything under 1 MB means pg_dump bailed early).
ssh -o StrictHostKeyChecking=no $HOST "\
  ls -lh ${BACKUP_REMOTE} && \
  [ \$(stat -c%s ${BACKUP_REMOTE}) -gt 1048576 ] || { echo 'BACKUP TOO SMALL — abort'; exit 1; }"

# Show the latest 3 backups for sanity (oldest gets recycled by ops).
ssh -o StrictHostKeyChecking=no $HOST "ls -lht /home/ec2-user/db-backups/ | head -5"

echo "Backup OK: ${BACKUP_REMOTE}"
```

**Restore quick-reference** (in case Step 1/2/3 corrupts data and rollback
of just the binaries is not enough):

```bash
# 1. Stop all 4 services (kill on the EC2 box) so nothing writes during restore.
# 2. Pipe the gzipped dump back into psql. --clean --if-exists in the dump
#    handles DROP-then-CREATE so a partial schema is OK.
ssh $HOST "gunzip -c ${BACKUP_REMOTE} | PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway"
# 3. Restart services (Step 6).
# 4. Verify Step 7a (nodes online).
```

Retention: keep at least 7 days of backups on the EC2 box. A weekly
cron can prune `mtime +7` but is out of scope for this skill — the
mandate is "always create a backup before a deploy", not "manage the
backup vault".

## Step 1 — Create prod release tag

```bash
# Use today's date. If tag already exists from an earlier deploy today, delete and recreate at HEAD.
DATE=$(date +%Y%m%d)
git tag -d prod-${DATE} 2>/dev/null || true
git tag -a prod-${DATE} -m "prod release ${DATE} — <one-line summary>"
```

Verify version string:
```bash
VER="$(git describe --tags --match 'prod-*' --abbrev=0)@$(git rev-parse --short HEAD)"
echo $VER   # e.g. prod-20260509@0d9c3a2c
```

## Step 2 — Build all 4 Go services (Linux amd64)

```bash
mkdir -p /tmp/nexus-deploy
VER="$(git describe --tags --match 'prod-*' --abbrev=0)@$(git rev-parse --short HEAD)"
LDFLAGS="-X main.buildVersion=${VER}"

# Build in parallel
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /tmp/nexus-deploy/nexus-hub            ./packages/nexus-hub/cmd/nexus-hub/ &
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /tmp/nexus-deploy/nexus-control-plane  ./packages/control-plane/cmd/control-plane/ &
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /tmp/nexus-deploy/nexus-ai-gateway     ./packages/ai-gateway/cmd/ai-gateway/ &
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o /tmp/nexus-deploy/nexus-compliance-proxy ./packages/compliance-proxy/cmd/compliance-proxy/ &
wait && echo "All 4 built"

ls -lh /tmp/nexus-deploy/   # expect ~47–54 MB each
```

## Step 3 — Build UI

```bash
npm run build -w packages/control-plane-ui
cd packages/control-plane-ui && tar -czf /tmp/nexus-ui-dist.tar.gz dist/
```

## Step 4 — Upload to EC2

```bash
HOST=ec2-user@18.204.174.212

scp -o StrictHostKeyChecking=no \
  /tmp/nexus-deploy/nexus-hub \
  /tmp/nexus-deploy/nexus-control-plane \
  /tmp/nexus-deploy/nexus-ai-gateway \
  /tmp/nexus-deploy/nexus-compliance-proxy \
  /tmp/nexus-ui-dist.tar.gz \
  ${HOST}:/tmp/ && echo "Upload complete"
```

## Step 5 — Install binaries + UI on EC2

**BINDING — preserve /var/www/nexus-ui/downloads/ across UI deploys.**
That subdirectory holds the agent .pkg distributions served from
https://nexus.taskforce10x.com/downloads/NexusAgent-latest.pkg, which
end users (and ops smoke tests) download and install. Naive
`rm -rf /var/www/nexus-ui` followed by `mv /tmp/dist` deletes the
entire downloads tree → 404 on every Mac install attempt until
someone notices and re-uploads. Lost twice on 2026-05-15 from
parallel CP UI deploys before this rule landed. Stage the new UI
into a tmp dir, rsync the assets in, leave /downloads/ alone:

```bash
ssh -o StrictHostKeyChecking=no $HOST "
  # Binaries
  sudo mv /tmp/nexus-hub /tmp/nexus-control-plane /tmp/nexus-ai-gateway /tmp/nexus-compliance-proxy /usr/local/bin/
  sudo chmod +x /usr/local/bin/nexus-{hub,control-plane,ai-gateway,compliance-proxy}

  # UI (macOS tar produces LIBARCHIVE.xattr warnings — harmless, ignore them).
  # Stage to a tmp dir, then rsync into the live tree EXCLUDING the
  # downloads/ subtree. --delete cleans stale dist/* assets from the
  # prior deploy without touching downloads/. Symlinks (e.g.
  # NexusAgent-latest.pkg) inside downloads/ are preserved.
  sudo tar -xzf /tmp/nexus-ui-dist.tar.gz -C /tmp/ 2>/dev/null
  sudo mkdir -p /var/www/nexus-ui/downloads
  sudo rsync -a --delete --exclude='/downloads' /tmp/dist/ /var/www/nexus-ui/
  sudo rm -rf /tmp/dist

  # Reload nginx so it closes any sendfile file descriptors pointing at old files
  sudo nginx -s reload

  ls -lh /usr/local/bin/nexus-* && echo 'Install OK'
  # Sanity-check downloads/ survived the rsync.
  ls /var/www/nexus-ui/downloads/ | head -5 || echo 'WARN downloads/ empty'
"
```

**If the deploy script you're using ALREADY rm -rf'd /var/www/nexus-ui**
(legacy script, not yet updated), restore the .pkg before declaring
success:

```bash
# After Step 5 (binaries done), if /var/www/nexus-ui/downloads is empty:
LATEST_PKG=$(ls -t dist/macos/NexusAgent-*.pkg 2>/dev/null | head -1)
[ -n "$LATEST_PKG" ] && {
  scp -o StrictHostKeyChecking=no "$LATEST_PKG" ${HOST}:/tmp/
  PKG_BASENAME=$(basename "$LATEST_PKG")
  ssh -o StrictHostKeyChecking=no $HOST "
    sudo mkdir -p /var/www/nexus-ui/downloads
    sudo mv /tmp/$PKG_BASENAME /var/www/nexus-ui/downloads/
    sudo chmod 644 /var/www/nexus-ui/downloads/$PKG_BASENAME
    cd /var/www/nexus-ui/downloads && sudo ln -sf $PKG_BASENAME NexusAgent-latest.pkg
  "
}
```

## Step 5.5 — Env-var preflight (MANDATORY — added 2026-05-20)

Verify the prod EnvironmentFile contains all required variables BEFORE
kill-then-start. The 2026-05-20 configuration refactor renamed 15+ env
vars; a stale EnvironmentFile carrying the OLD names will silently
degrade services to yaml-fallback values (worst case: secrets fall
back to empty, so internal-service calls 401 and credential
decryption fails).

Reference: `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` §6
for the full rename table.

```bash
HOST=ec2-user@18.204.174.212
ssh -o StrictHostKeyChecking=no $HOST '
  sudo cat /etc/systemd/system/nexus-*.service.d/env.conf 2>/dev/null \
    || sudo cat /etc/nexus-gateway/env
' > /tmp/prod-env-snapshot

echo "=== Required [MUST MATCH] secrets — identical across all services that share them ==="
grep -E '^(INTERNAL_SERVICE_TOKEN|ADMIN_KEY_HMAC_SECRET|CREDENTIAL_ENCRYPTION_KEY|COMPLIANCE_PROXY_API_TOKEN|AUTH_SERVER_ISSUER)=' /tmp/prod-env-snapshot

echo "=== Required infrastructure URLs (NEW names — fail-closed) ==="
grep -E '^(DATABASE_URL|REDIS_MODE|REDIS_ADDRS|NATS_URL|NEXUS_HUB_URL|AUTH_SERVER_URL|AUTH_SERVER_JWKS_URL|AGENT_CA_DIR)=' /tmp/prod-env-snapshot

echo "=== Required service-private ports ==="
grep -E '^(NEXUS_HUB_PORT|AI_GATEWAY_PORT|COMPLIANCE_PROXY_PORT|CONTROL_PLANE_PORT)=' /tmp/prod-env-snapshot

echo "=== Required service-private API tokens ==="
grep -E '^(AI_GATEWAY_API_TOKEN|COMPLIANCE_PROXY_API_TOKEN)=' /tmp/prod-env-snapshot

echo "=== FORBIDDEN — OLD names that MUST be absent (renamed in 2026-05-20 refactor) ==="
forbidden=(
  CONTROL_PLANE_URL
  NEXUS_HUB_CP_URL NEXUS_HUB_CP_JWKS_URL NEXUS_HUB_CP_ISSUER
  NEXUS_HUB_AGENTCA_DIR NEXUS_HUB_AGENTCA_CERT_FILE NEXUS_HUB_AGENTCA_KEY_FILE
  PORT
  REDIS_URL REDIS_ADDR
  CACHE_ENABLED CACHE_TTL CACHE_PREFIX
  CORS_ENABLED CORS_ALLOWED_ORIGINS
  CRYPTO_PRODUCTION
  AI_GATEWAY_BASE_URL
)
for n in "${forbidden[@]}"; do
  if grep -qE "^${n}=" /tmp/prod-env-snapshot; then
    echo "STOP: ${n} is set but was renamed in the 2026-05-20 refactor — abort deploy and update EnvironmentFile first"
  fi
done
```

If ANY required line is missing, OR any FORBIDDEN var is present,
abort the deploy and update the EnvironmentFile first. Re-run this
preflight until both checks are clean.

Common mistakes caught by this step:
- `CONTROL_PLANE_URL` still present after rename to `NEXUS_HUB_URL` —
  service silently can't register as a Thing.
- `NEXUS_HUB_CP_*` (URL / JWKS_URL / ISSUER) still present — Hub falls
  back to empty `authServer.*` yaml fields, JWT verification fails,
  every CP→Hub call returns 401.
- `NEXUS_HUB_AGENTCA_*` still present — Hub can't load the agent CA,
  agent mTLS enrollment breaks.
- One of the [MUST MATCH] secrets drifted between services (e.g. CP
  rotated `INTERNAL_SERVICE_TOKEN` but ai-gateway's EnvironmentFile
  wasn't updated) — silent 403s on inter-service calls.

## Step 6 — Kill all services then start in order

**Rule: always kill then start — never use `systemctl restart`.**
Hub must start first (other services register with it over WebSocket).

**Wait for the actual PID to die, not a fixed sleep.** Hub's graceful
shutdown can run 12–15 s (drains MQ consumer queue, scheduler stop,
selfshadow.Stop, ws.HandleUpgrade context cancel, NATS connection close).
If `systemctl start nexus-hub` arrives while the old unit is still
shutting down, systemd treats the start as a no-op and Hub stays down —
silently. The 2026-05-23 deploy hit this with `sleep 4`. Two safe
patterns: poll for PID=0 before starting, then poll for port 3060
LISTEN before starting the other 3.

```bash
ssh -o StrictHostKeyChecking=no $HOST "
  set -u
  SERVICES='nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy'

  # 1. SIGTERM all four at once.
  PIDS=\$(sudo systemctl show -p MainPID \$SERVICES | grep -oP 'MainPID=\K[0-9]+' | grep -v '^0\$')
  if [ -n \"\$PIDS\" ]; then sudo kill \$PIDS 2>/dev/null; fi
  echo 'SIGTERM sent to PIDs:' \$PIDS

  # 2. Wait up to 30 s for systemd to report MainPID=0 on every unit.
  #    Polls every 1 s; on timeout, SIGKILL stragglers.
  for i in \$(seq 1 30); do
    ALIVE=\$(sudo systemctl show -p MainPID \$SERVICES | grep -oP 'MainPID=\K[0-9]+' | grep -v '^0\$' | wc -l)
    [ \"\$ALIVE\" = '0' ] && { echo \"all stopped after \${i}s\"; break; }
    sleep 1
  done
  # SIGKILL any remaining (e.g. hung graceful shutdown).
  for svc in \$SERVICES; do
    P=\$(sudo systemctl show -p MainPID \$svc | grep -oP 'MainPID=\K[0-9]+')
    if [ \"\$P\" != '0' ] && [ -n \"\$P\" ]; then
      sudo kill -9 \$P 2>/dev/null && echo \"SIGKILL \$svc PID \$P (graceful timeout)\"
    fi
  done

  # 3. Start Hub. Poll for port 3060 LISTEN (up to 30 s) before starting
  #    the other three — they register with Hub over WebSocket on boot.
  sudo systemctl start nexus-hub
  echo 'nexus-hub start issued; polling port 3060...'
  for i in \$(seq 1 30); do
    if sudo ss -ltn 'sport = :3060' 2>/dev/null | grep -q ':3060'; then
      echo \"hub listening after \${i}s\"; break
    fi
    sleep 1
  done
  # Fail-loud if Hub still not up after 30 s.
  if ! sudo ss -ltn 'sport = :3060' 2>/dev/null | grep -q ':3060'; then
    echo 'FATAL: hub did not bind port 3060 in 30s — aborting cascade'
    sudo journalctl -u nexus-hub --since '1 min ago' --no-pager -n 30
    exit 1
  fi

  # 4. Start the other three; brief settle, then status snapshot.
  sudo systemctl start nexus-control-plane nexus-ai-gateway nexus-compliance-proxy
  echo 'all 3 dependents started'
  sleep 3
  sudo systemctl status \$SERVICES --no-pager 2>&1 | grep -E 'Active:|Main PID'
"
```

## Step 7 — Verify

### 7a — Nodes online in DB

```bash
ssh -o StrictHostKeyChecking=no $HOST "
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \
  \"SELECT id, type, status, version, last_seen_at FROM thing WHERE id LIKE '%-ip-172%' ORDER BY type;\"
"
# Expect: all rows status=online, version=prod-YYYYMMDD@<sha>
```

### 7b — No startup errors in Hub logs

```bash
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-hub --since '1 min ago' --no-pager | grep -i 'error\|fatal\|panic' | head -20"
```

### 7c — Smoke check key API

```bash
cd <repo-root>
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_curl /api/admin/analytics/summary | python3 -m json.tool | head -20'
```

### 7d — Audit pipeline alive (MANDATORY — added 2026-05-14)

A previous deploy left prod's audit pipeline silently broken for 16 hours
because a Hub-side schema mismatch made every `traffic_event` batch flush
fail with `42703 column ... does not exist`. The pipeline appeared
healthy by every other check (services up, S3 writes succeeded, NATS
broker active) but `traffic_event` had zero new rows. This step catches
that class of regression.

```bash
ssh -o StrictHostKeyChecking=no $HOST "
  echo '--- NATS broker active ---'
  sudo systemctl is-active nats
  echo
  echo '--- Hub flush errors in last 5 min ---'
  sudo journalctl -u nexus-hub --since '5 min ago' --no-pager | \
    grep -iE 'flush.*fail|insert.*fail|42703|column.*does not exist' | head -10
  echo
  echo '--- traffic_event freshness (must be non-empty + recent) ---'
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \
    \"SELECT max(timestamp) AS latest_event,
             EXTRACT(EPOCH FROM (now() - max(timestamp)))::int AS seconds_since_latest,
             count(*) FILTER (WHERE timestamp >= now() - interval '5 minutes') AS rows_last_5min
       FROM traffic_event;\"
  echo
  echo '--- thing_diag_event freshness (catches diag-pipeline schema drift) ---'
  PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \
    \"SELECT max(occurred_at) AS latest_diag,
             EXTRACT(EPOCH FROM (now() - max(occurred_at)))::int AS seconds_since_latest,
             count(*) FILTER (WHERE occurred_at >= now() - interval '10 minutes') AS rows_last_10min
       FROM thing_diag_event;\"
"
```

**Expected**:
- `nats` → `active`
- Hub error block → empty
- `traffic_event` `seconds_since_latest` → typically < 60 s on a non-idle
  prod (acceptable up to a few minutes if traffic is sparse).
- `traffic_event` `rows_last_5min` → ≥ 1 on a non-idle prod.
- `thing_diag_event` `seconds_since_latest` → typically < 5 min (services
  publish lifecycle / startup / scheduler events through SlogSink at low
  rate even when traffic is idle). A value > 30 min on a service that's
  been up means the diag pipeline itself is wedged — same class of bug as
  the 2026-05-13/14 traffic_event flush incident, except now on the
  `thing_diag_event` writer side.
- `thing_diag_event` `rows_last_10min` → ≥ 1 on a deploy that just
  restarted services (every service emits a `<name> started` event at
  boot through the diag pipeline; if those didn't land, schema drift on
  `thing_diag_event` is the most likely cause).

**If the freshness query shows a stale `latest_event` (> 10 min) and
Hub flush errors are present**, STOP — the deploy is wedging the audit
pipeline. Common causes:
- a new migration that wasn't applied to prod (see "Applying a single DB
  migration" below);
- Hub binary expects a column / index / constraint that the prod schema
  lacks (forward-compat drift);
- NATS JetStream store moved or wiped (check `Store Directory:` in the
  nats.service log; should be a persistent path like
  `/var/lib/nats/jetstream`, never `/tmp/...`).

**If audit appears stale but Hub log is clean**, fire one manual probe
request through the AI Gateway (using a known-good VK) and re-query.
Audit batches flush in tight intervals; if the probe row also doesn't
appear in 30 s, the consumer side is broken.

## Step 8 — Update prod release tracking

After a successful deploy, update the memory file:
`/Users/nexus/.claude/projects/-Users-nexus-workspaces-workspace-nexus-nexus-gateway/memory/project_prod_releases.md`

Add a row to the release log table:
```
| prod-YYYYMMDD | <short-sha> | YYYY-MM-DD | <change summary> |
```

---

## Applying migrations (single or batch)

Use the bundled helper `apply-migration.sh` (in this skill directory) for
every prod-side migration application. It propagates psql exit codes
back through ssh (so a failed migration doesn't silently mark "OK"),
wraps each in `BEGIN/COMMIT`, escapes JSON/multi-statement SQL via
file transfer, and records each row in `_prisma_migrations` with the
real `sha256(migration.sql)` checksum — so the long-term migration to
`prisma migrate deploy` doesn't require backfilling checksums.

**FIRST run the mandatory backup from Step 0a above.** The script does
not take its own backup; applying migrations is exactly when an
unrecoverable rollback path becomes important.

```bash
# Single migration:
.claude/skills/prod-deploy/apply-migration.sh 20260601000000_e62_model_capability

# Batch (applies in order, stops on first failure):
.claude/skills/prod-deploy/apply-migration.sh \
  20260520120000_e61_response_cache_dual_tier \
  20260521134101_e68_gateway_cache_l2_entry_key \
  20260602000000_e61_semantic_cache_overrides_org

# Override defaults (rarely needed):
HOST=ec2-user@<other-ip> PGUSER=other PGDB=other_db \
  .claude/skills/prod-deploy/apply-migration.sh <name>
```

After applying, verify the schema change:
```bash
ssh ec2-user@18.204.174.212 \
  "PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c '\d <table_name>'"
```

Then restart any affected services so they pick up the new schema.

**If a migration depends on a table created by another migration that
isn't on prod yet** (e.g. the 2026-05-23 incident: 3 semantic_cache_*
migrations needed `semantic_cache_config`, which lived in an earlier
migration that had never landed on prod), the script will fail
immediately with `relation "X" does not exist`. Fix the gap by
applying the prerequisite migration first, then re-run the script
with the dependent ones.

---

## Rollback

```bash
# Tag rollback target first (don't stash — rule: never git stash)
git checkout <previous-sha>
# Then re-run Steps 1–7 with the old commit
```
