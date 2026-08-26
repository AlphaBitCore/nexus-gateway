#!/bin/bash
# first-boot-db.sh — initialise PostgreSQL, materialise schema, seed baseline
# rows, randomise the admin password, and surface credentials to the operator.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md
# Hashing matches tools/db-migrate/seed/lib.ts hashPassword(): scrypt N=2^17,
# r=8, p=1, salt=32B, key=64B, format "salt_hex:hash_hex" (see set-admin-password.js).

set -euo pipefail

# Put the bundled Node 20 on PATH so `npx` (whose shebang is
# `#!/usr/bin/env node`) can resolve `node`. Without this the script aborts
# at the first prisma call with `/usr/bin/env: 'node': No such file or
# directory` because systemd starts this unit with the system PATH that does
# not include /opt/nexus/node/bin. Hit on 2026-05-28 first-launch test of
# build #8.
export PATH=/opt/nexus/node/bin:$PATH

PRISMA_DIR=/opt/nexus/prisma
# AWS Marketplace AMI policy: default admin credentials must be generated on
# first boot (never baked into the AMI), and the read-once file must live
# outside /var/log, be mode 0600, and be owned by root only. /root satisfies
# this — see ami-appliance-architecture.md §5.
ADMIN_CREDS=/root/nexus-admin-credentials.txt

# Source the per-service env file written by first-boot-secrets.sh — the seed
# requires CREDENTIAL_ENCRYPTION_KEY (re-encrypts seeded credential rows) and
# ADMIN_KEY_HMAC_SECRET (re-hashes seeded VK lookup keys). Both live in
# control-plane.env which has the union of secrets for the two services that
# need them.
# shellcheck disable=SC1091
. /etc/nexus/control-plane.env
export CREDENTIAL_ENCRYPTION_KEY ADMIN_KEY_HMAC_SECRET INTERNAL_SERVICE_TOKEN COMPLIANCE_PROXY_API_TOKEN

DB_NAME=nexus_gateway
DB_USER=nexus
DB_PASSWORD=$(openssl rand -hex 24)
ADMIN_PASSWORD=$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-20)
PGDATA=/var/lib/pgsql/data
# Post-DB sub-marker. The outer first-boot marker (/etc/nexus/.initialized)
# is written only after later steps (OAuth redirect registration) succeed; a
# failure there would otherwise re-run this whole script — including the
# non-idempotent admin-password reset + creds write — on the next reboot.
# This finer marker makes the admin/creds tail run exactly once. harden.sh
# wipes it so a re-baked AMI re-initialises.
DB_MARKER=/etc/nexus/.db-initialized

# ─── initdb on first launch (install.sh deferred this so harden.sh's wipe
# leaves a clean snapshot; see install-postgres.sh for the why). ────────
if [ ! -f "$PGDATA/PG_VERSION" ]; then
  echo "[first-boot-db] initialising PostgreSQL data directory..."
  /usr/bin/postgresql-setup --initdb

  echo "[first-boot-db] enforcing localhost-only + scram-sha-256 auth..."
  sed -i "s/^#listen_addresses.*/listen_addresses = '127.0.0.1'/" "$PGDATA/postgresql.conf"
  sed -i "s/^listen_addresses.*/listen_addresses = '127.0.0.1'/"  "$PGDATA/postgresql.conf"
  sed -i "s/^#password_encryption.*/password_encryption = scram-sha-256/" "$PGDATA/postgresql.conf"
  sed -i "s/^password_encryption.*/password_encryption = scram-sha-256/"  "$PGDATA/postgresql.conf"

  echo "[first-boot-db] applying high-concurrency tuning..."
  # Appended (PostgreSQL honours the LAST occurrence of each setting), which
  # is more robust than sed-replacing whichever commented/uncommented form
  # the stock postgresql.conf ships.
  mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
  sb_mb=$(( mem_mb / 4 )); [ "$sb_mb" -lt 128 ]  && sb_mb=128;  [ "$sb_mb" -gt 8192 ] && sb_mb=8192
  ec_mb=$(( mem_mb / 2 )); [ "$ec_mb" -lt 256 ]  && ec_mb=256
  cat >> "$PGDATA/postgresql.conf" <<PGTUNE

# ─── Nexus high-concurrency tuning (appended; last value wins) ───────────
# max_connections=200: headroom above the sum of every service's DB pool
# (~84: hub 40 + ai-gateway 25 + control-plane 15 + compliance-proxy ~5).
# The stock default of 100 would be exhausted under a concurrency burst,
# surfacing as "too many clients already" connection errors.
max_connections = 200
# shared_buffers ~= 25% of detected RAM (PostgreSQL's standard starting
# point), clamped to [128MB, 8GB] for the single-box appliance that also
# hosts Valkey + NATS + the four Go services.
shared_buffers = ${sb_mb}MB
# effective_cache_size ~= 50% RAM — a planner hint, not an allocation.
effective_cache_size = ${ec_mb}MB
PGTUNE

  cat > "$PGDATA/pg_hba.conf" <<'PGHBA'
# Nexus appliance — localhost-only, scram-sha-256 for nexus user, peer for postgres OS user.
local   all             postgres                                peer
local   all             all                                     scram-sha-256
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
PGHBA
  chown postgres:postgres "$PGDATA/pg_hba.conf"
  chmod 0600              "$PGDATA/pg_hba.conf"
fi

echo "[first-boot-db] starting PostgreSQL..."
systemctl start postgresql

# Wait until accepting connections (postgresql-setup is async-ish on some AL2023 builds).
for i in 1 2 3 4 5 6 7 8 9 10; do
  if sudo -u postgres pg_isready -q -h /var/run/postgresql; then
    break
  fi
  echo "[first-boot-db] waiting for PostgreSQL... ($i/10)"
  sleep 1
done

# If a previous DATABASE_URL was already stamped into an env file, reuse the
# password it encodes — the role already exists in PG with that password, and
# rotating it here would break that consistency. Otherwise this is the first
# run and we generate a fresh DB_PASSWORD above.
#
# `|| true` is load-bearing: on a fresh boot the env file exists (written by
# first-boot-secrets) but contains NO DATABASE_URL line yet, so grep returns 1.
# Under `set -euo pipefail` that fails the command substitution and `set -e`
# kills the whole script BEFORE we ever reach the role-creation block. Hit on
# 2026-05-28 first-launch test of build #8.
EXISTING_URL=$(grep -h '^DATABASE_URL=' /etc/nexus/control-plane.env 2>/dev/null | tail -1 | sed 's/^DATABASE_URL=//' || true)
if [ -n "$EXISTING_URL" ]; then
  echo "[first-boot-db] reusing prior DATABASE_URL from /etc/nexus/control-plane.env (idempotent)."
  DATABASE_URL="$EXISTING_URL"
  DB_PASSWORD=$(echo "$DATABASE_URL" | sed -E "s|.*://$DB_USER:([^@]+)@.*|\1|")
fi

echo "[first-boot-db] ensuring role and database exist (idempotent)..."
# The DB role is created SUPERUSER so first-boot can apply schema-extras.sql
# (which DROP/CREATEs the partitioned metric_ops_raw and defines a function +
# view) and own every object outright. Acceptable for this appliance: Postgres
# binds 127.0.0.1 only (see the listen_addresses tweak above) and pg_hba.conf
# forces scram-sha-256, so the attack surface is local processes only — same
# boundary as the rest of the appliance.
sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$DB_USER') THEN
    EXECUTE format('CREATE ROLE %I WITH LOGIN SUPERUSER PASSWORD %L', '$DB_USER', '$DB_PASSWORD');
  END IF;
END \$\$;
ALTER ROLE "$DB_USER" SUPERUSER;
SELECT 'CREATE DATABASE "$DB_NAME"'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$DB_NAME')
\gexec
ALTER DATABASE "$DB_NAME" OWNER TO "$DB_USER";
SQL

DATABASE_URL="postgresql://$DB_USER:$DB_PASSWORD@127.0.0.1:5432/$DB_NAME?sslmode=disable"

echo "[first-boot-db] stamping DATABASE_URL into all 4 service env files (replace, not append)..."
for envfile in /etc/nexus/nexus-hub.env \
               /etc/nexus/control-plane.env \
               /etc/nexus/ai-gateway.env \
               /etc/nexus/compliance-proxy.env; do
  sed -i '/^DATABASE_URL=/d' "$envfile"
  echo "DATABASE_URL=$DATABASE_URL" >> "$envfile"
done

echo "[first-boot-db] materialising schema via prisma db push..."
cd "$PRISMA_DIR"
# --skip-generate was removed in newer Prisma CLI; client generation is now a
# separate explicit call below. Hit on 2026-05-28 first-launch test of build #8:
# "! unknown or unexpected option: --skip-generate". --accept-data-loss alone is
# enough — on a fresh DB there is no data to lose, but Prisma requires the flag
# to push without an interactive y/n prompt.
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma db push --accept-data-loss

echo "[first-boot-db] applying schema-extras.sql (PostgreSQL-native objects db push can't express)..."
# db push only creates what schema/'s model graph expresses. schema-extras.sql
# adds the metric_ops_raw RANGE partitioning (Hub ops-raw-partition job needs it),
# the cache_key_source function + cache_provider_effective view, and the partial/
# expression/GIN indexes — including thing_type_physical_id_uniq (agent identity)
# and DeviceAssignment_deviceId_active_uidx. Re-runnable; safe on every boot.
sudo -u postgres psql -d "$DB_NAME" -v ON_ERROR_STOP=1 -f "$PRISMA_DIR/schema-extras.sql"

echo "[first-boot-db] generating Prisma client (required by seed)..."
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma generate

# SEED_DEMO=false: the appliance seeds ONLY the reference catalog + bootstrap
# tenant (one org, one project, the super-admin admin@nexus.ai, the
# system-assistant VK). The demo playground (extra users, demo VKs + credentials
# whose plaintexts are public in the OSS repo) is NOT seeded — shipping any
# repo-committed credential on an internet-facing appliance is default-credential
# exposure that AWS Marketplace prohibits.
#
# The fixture seed is idempotent (Prisma upserts keyed on each row's unique key),
# so a re-run converges instead of aborting. Still fast-skip when NexusUser is
# already populated, so a partial failure downstream cannot force a full re-seed
# on every boot. The seed needs CREDENTIAL_ENCRYPTION_KEY + ADMIN_KEY_HMAC_SECRET
# (already stamped into the service env files) to stamp the bootstrap secrets
# under this instance's local keys.
SEEDED_USERS=$(sudo -u postgres psql -d "$DB_NAME" -tAc 'SELECT count(*) FROM "NexusUser";' 2>/dev/null | tr -d '[:space:]' || echo 0)
if [ "${SEEDED_USERS:-0}" -gt 0 ]; then
  echo "[first-boot-db] NexusUser already populated ($SEEDED_USERS rows); skipping seed (idempotent re-run)."
else
  echo "[first-boot-db] seeding (Tier A reference catalog + bootstrap tenant; demo skipped)..."
  SEED_DEMO=false DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx tsx seed/seed.ts
fi

# Admin password reset + demo-account lockdown + creds file: run exactly once
# (guarded by $DB_MARKER) so a reboot can't re-randomise the admin password
# and orphan the creds the operator already read.
if [ -f "$DB_MARKER" ]; then
  echo "[first-boot-db] admin/demo lockdown already applied (marker $DB_MARKER); skipping."
  echo "[first-boot-db] complete (idempotent re-run)."
  exit 0
fi

echo "[first-boot-db] randomising admin@nexus.ai password..."
# The bootstrap seed ships admin@nexus.ai with a public scrypt hash (password
# "nexus-demo") so dev machines log in out of the box. Replace it with a
# per-instance random password before the appliance is ever reachable. (Demo
# accounts are not seeded here — SEED_DEMO=false above — so there is nothing
# else to disable.)
NEW_ADMIN_HASH=$(NEW_PASSWORD="$ADMIN_PASSWORD" /opt/nexus/node/bin/node "$PRISMA_DIR/set-admin-password.js")
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma db execute --stdin <<SQL
UPDATE "NexusUser" SET "passwordHash" = '$NEW_ADMIN_HASH', "passwordUpdatedAt" = NOW() WHERE email = 'admin@nexus.ai';
SQL

echo "[first-boot-db] minting per-instance system-assistant VK (Chat-with-Nexus auth)..."
# The bootstrap seed's system-assistant VK ships with a public deterministic
# plaintext; replace it with a per-instance random secret (same reasoning as the
# admin password) and wire the new plaintext into control-plane.env. The id is
# fixed in tools/db-migrate/seed/fixtures/bootstrap/VirtualKey.json.
ASSISTANT_VK_ID="b0075000-0000-4000-8000-0000000000a2"
ASSISTANT_VK_MINT=$(/opt/nexus/node/bin/node "$PRISMA_DIR/mint-assistant-vk.js")
ASSISTANT_VK_PLAINTEXT=$(printf '%s' "$ASSISTANT_VK_MINT" | cut -f1)
ASSISTANT_VK_HASH=$(printf '%s' "$ASSISTANT_VK_MINT" | cut -f2)
ASSISTANT_VK_PREFIX=$(printf '%s' "$ASSISTANT_VK_MINT" | cut -f3)
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma db execute --stdin <<SQL
UPDATE "VirtualKey" SET "keyHash" = '$ASSISTANT_VK_HASH', "keyPrefix" = '$ASSISTANT_VK_PREFIX' WHERE id = '$ASSISTANT_VK_ID';
SQL
# Replace (never append) so a re-run can't stack duplicate lines.
sed -i '/^NEXUS_ASSISTANT_SYSTEM_VK=/d' /etc/nexus/control-plane.env
echo "NEXUS_ASSISTANT_SYSTEM_VK=$ASSISTANT_VK_PLAINTEXT" >> /etc/nexus/control-plane.env

echo "[first-boot-db] writing $ADMIN_CREDS..."
cat > "$ADMIN_CREDS" <<EOF
================================================================================
Nexus Gateway — appliance first-boot credentials
================================================================================

URL:       https://<this-instance-public-or-private-ip>/
Username:  admin@nexus.ai
Password:  $ADMIN_PASSWORD

IMPORTANT
---------
1. This file is mode 0600, owned by root — only root can read it. It was
   generated on first boot and is unique to this instance. Delete it as soon
   as you have logged in and changed the admin password from the UI:
       sudo rm $ADMIN_CREDS
2. The TLS certificate at /etc/nexus/tls.crt is SELF-SIGNED. Replace it with
   a cert signed for your hostname before exposing the appliance publicly,
   then run: sudo systemctl reload nginx
3. The Compliance Proxy MITM CA at /etc/compliance-proxy/ca.crt must be
   distributed to every device that egresses through the proxy on port 3128.
4. This appliance ships with NO demo data and NO default credentials —
   admin@nexus.ai (above) is the only account, and its password plus the
   internal system keys were generated uniquely for this instance on first boot.
5. ADD A PROVIDER API KEY before sending traffic: log in, go to
   Settings -> Providers, pick a provider (OpenAI, Anthropic, …) and add your
   key. Until then requests return "no available provider".
6. CHAT WITH NEXUS is wired to a per-instance system key automatically; once you
   have added at least one provider key (step 5) the in-product assistant works
   with no further configuration.

For full operator documentation see:
  https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/
================================================================================
EOF
chmod 0600 "$ADMIN_CREDS"
chown root:root "$ADMIN_CREDS"

cat > /etc/motd <<EOF

Nexus Gateway appliance — first-boot complete.

Admin credentials for this instance are saved at:
  $ADMIN_CREDS (mode 0600, root-only)

Run:  sudo cat $ADMIN_CREDS
Delete the file once you have changed the admin password:  sudo rm $ADMIN_CREDS

EOF

# Mark the admin/demo tail done so a reboot before /etc/nexus/.initialized
# is written does not re-randomise the admin password.
touch "$DB_MARKER"
chmod 0600 "$DB_MARKER"

echo "[first-boot-db] complete (DB seeded reference+bootstrap, admin password + system-assistant VK minted, $ADMIN_CREDS written)."
