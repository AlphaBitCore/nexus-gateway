#!/bin/bash
# first-boot-db.sh — initialise PostgreSQL, materialise schema, seed baseline
# rows, randomise the admin password, and surface credentials to the operator.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md
# Hashing matches tools/db-migrate/seed/lib.ts hashPassword(): scrypt N=16384,
# r=8, p=1, salt=32B, key=64B, format "salt_hex:hash_hex".

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
# SUPERUSER is required because seed/data/seed-baseline.sql is a pg_dump that
# uses `ALTER TABLE ... DISABLE TRIGGER ALL` to load FK-related rows out of
# topological order. Postgres only lets SUPERUSER touch the system-generated
# RI_ConstraintTrigger_* triggers — without it the seed aborts with
#   permission denied: "RI_ConstraintTrigger_a_NNNN" is a system trigger
# Acceptable for this appliance: Postgres binds 127.0.0.1 only (see the
# listen_addresses tweak above) and pg_hba.conf forces scram-sha-256, so the
# attack surface is local processes only — same boundary as the rest of the
# appliance. Hit on 2026-05-28 first-launch test of build #8.
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

echo "[first-boot-db] generating Prisma client (required by seed)..."
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma generate

echo "[first-boot-db] loading baseline seed (organisations, IAM, roles)..."
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx tsx seed/seed.ts

echo "[first-boot-db] randomising admin@nexus.ai password..."
NEW_ADMIN_HASH=$(NEW_PASSWORD="$ADMIN_PASSWORD" /opt/nexus/node/bin/node "$PRISMA_DIR/set-admin-password.js")
DATABASE_URL="$DATABASE_URL" /opt/nexus/node/bin/npx prisma db execute --stdin <<SQL
UPDATE "NexusUser" SET "passwordHash" = '$NEW_ADMIN_HASH', "passwordUpdatedAt" = NOW() WHERE email = 'admin@nexus.ai';
SQL

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
4. Demo accounts (alice@/bob@/carol@/diana@nexus.ai) ship with documented
   dev passwords — disable them from the UI before opening this instance
   to external traffic.

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

echo "[first-boot-db] complete (DB seeded, admin password set, $ADMIN_CREDS written)."
