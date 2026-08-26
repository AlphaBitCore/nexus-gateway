#!/usr/bin/env bash
# entrypoint.sh — bring an empty database up to the current schema and seed it.
#
# Runs once per `docker compose up` and must exit 0, because every service
# waits on `service_completed_successfully`.
#
# 1.0 applies the multi-file schema/ folder with `prisma db push` (there is no
# migration history), then schema-extras.sql for the PostgreSQL-native objects
# db push cannot express.
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL must be set}"
# Checked up front, not just before rotation, so a missing rotation credential
# fails in seconds instead of after a full schema push and seed.
: "${NEXUS_ADMIN_PASSWORD:?NEXUS_ADMIN_PASSWORD must be set — refusing to leave the public seed password in place}"
: "${NEXUS_ASSISTANT_SYSTEM_VK:?NEXUS_ASSISTANT_SYSTEM_VK must be set — refusing to leave the public assistant key in place}"
: "${ADMIN_KEY_HMAC_SECRET:?ADMIN_KEY_HMAC_SECRET must be set — needed to hash the assistant key}"
# Guarded here rather than defaulted, because a wrong or missing console origin
# does not fail here — it fails later, as a 400 from /oauth/authorize when an
# admin first clicks Login, which reads as "the product is broken" rather than
# "a variable is unset". Fail loudly now instead.
: "${CONTROL_PLANE_PUBLIC_URL:?CONTROL_PLANE_PUBLIC_URL must be set — it is the origin the console is reached at, and its callback URL must be registered or no admin can log in}"
# Shape-checked here, at the top, because this value is later interpolated into
# SQL string literals and into a PL/pgSQL RAISE format string. It comes from an
# operator's .env, so a quote would break out of the literal, a `$$` would
# terminate the dollar-quoted block early, and a bare `%` would turn the
# exception message into "too few parameters specified for RAISE". A URL origin
# needs none of those characters, so the allowed set is a whitelist. Failing
# here is safe for the same reason every guard at the top of this script is:
# nothing has run yet, so no seed has published anything this run. Failing at
# the UPDATE instead would abort after the seed and take the rotations with it.
case "$CONTROL_PLANE_PUBLIC_URL" in
  http://*|https://*) ;;
  *) echo "==> [migrator] FATAL: CONTROL_PLANE_PUBLIC_URL must start with http:// or https:// (got: $CONTROL_PLANE_PUBLIC_URL)" >&2; exit 1 ;;
esac
case "$CONTROL_PLANE_PUBLIC_URL" in
  *[!A-Za-z0-9:/._~[\]-]*)
    echo "==> [migrator] FATAL: CONTROL_PLANE_PUBLIC_URL contains a character that cannot appear in a URL origin (got: $CONTROL_PLANE_PUBLIC_URL)" >&2
    exit 1
    ;;
esac

# ── Dependency install ───────────────────────────────────────────────────
# This image ships thin — no baked node_modules. deploy/docker-compose.yml
# mounts the `migrator-deps` named volume over /app/tools/db-migrate/
# node_modules, and the install below fills it on the first `docker compose
# up`. Every later `up` reuses it. This must run before anything else here:
# the database wait loop below already shells out to `npx prisma`.
#
# The reuse test is the lockfile's own sha256, recorded inside the volume
# after a successful install. Hashing rather than testing for a non-empty
# node_modules gets two properties: an image upgrade that changes the
# dependency set reinstalls instead of silently running against stale
# packages, and a half-finished install never counts as present, because the
# marker is written last, after the completed tree has been moved into place.
LOCK_HASH="$(sha256sum package-lock.json | cut -d' ' -f1)"
LOCK_MARKER="node_modules/.deps-lock-hash"
PREV_DIR="node_modules/.previous-install"
# The directory exists already whenever the volume is mounted; this covers the
# form factor that runs this image without one (the tarball install's one-shot
# `docker run ... db-migrator`), where the staging swap below still needs a
# destination.
mkdir -p node_modules
# A .previous-install directory means an earlier run was killed mid-swap, so
# node_modules holds a partial new tree at best and its marker cannot be
# trusted — it may even be the OLD marker, left behind because the move-aside
# had not reached it yet. Put the last complete tree back BEFORE the marker is
# read: with it and its marker restored, a matching lockfile skips the install
# below entirely and this run starts on the previous version without touching
# the network, which is the whole point of keeping the copy. Running this after
# the marker check (or after `npm ci`) would make recovery depend on the very
# registry access the operator may not have.
if [ -d "$PREV_DIR" ]; then
  echo "==> [migrator] found an interrupted dependency swap; restoring the previous tree first..." >&2
  find node_modules -mindepth 1 -maxdepth 1 ! -name .previous-install -exec rm -rf {} + || true
  if ! find "$PREV_DIR" -mindepth 1 -maxdepth 1 -exec mv -t node_modules/ {} +; then
    echo "==> [migrator] FATAL: could not restore the interrupted dependency tree; node_modules/.previous-install still holds it. Free disk space and retry, or remove the migrator-deps volume to reinstall from scratch." >&2
    exit 1
  fi
  rm -rf "$PREV_DIR"
  echo "==> [migrator] previous dependency tree restored." >&2
fi

if [ "$(cat "$LOCK_MARKER" 2>/dev/null || true)" = "$LOCK_HASH" ]; then
  echo "==> [migrator] dependencies already present for this lockfile — skipping install."
else
  echo "==> [migrator] installing dependencies (first run for this lockfile; needs the npm registry)..."
  # Installed into a staging directory and swapped in only after npm exits 0,
  # because `npm ci` empties node_modules BEFORE it fetches anything. Installing
  # in place would mean a reinstall that fails — npm registry unreachable, an
  # egress policy change, a half-downloaded package — destroys the working
  # dependency set that was already there, together with the marker that
  # records it. Every service gates on db-migrator:
  # service_completed_successfully, so that leaves the stack unstartable at ANY
  # version: rolling NEXUS_VERSION back does not recover it, because the older
  # image finds the same emptied volume and reinstalls too. Staging keeps the
  # previous tree serving until a complete new one exists.
  #
  # --ignore-scripts: nothing here needs an install-time script — Prisma 7's
  # schema engine ships as a bundled WASM module (prisma/build/
  # schema_engine_bg.wasm), not a postinstall-fetched native binary, and this
  # package reaches Postgres through @prisma/adapter-pg (the `pg` driver)
  # rather than a Prisma binary engine.
  # --omit=dev is a no-op today (tools/db-migrate/package.json declares no
  # devDependencies) but guards against a future dev-only addition landing in
  # the volume.
  STAGE_DIR=/tmp/nexus-migrator-deps
  rm -rf "$STAGE_DIR"
  mkdir -p "$STAGE_DIR"
  cp package.json package-lock.json "$STAGE_DIR/"
  if ! (cd "$STAGE_DIR" && npm ci --no-audit --no-fund --ignore-scripts --omit=dev); then
    rm -rf "$STAGE_DIR"
    echo "==> [migrator] FATAL: dependency install failed. The first run for a given lockfile must reach the npm registry (https://registry.npmjs.org); later runs reuse the migrator-deps volume. Any dependencies already in that volume were left untouched, so a previously working stack can still start on its previous version. Check network/proxy access and retry \`docker compose up\`." >&2
    exit 1
  fi
  # Swap, keeping the previous tree recoverable throughout. node_modules is the
  # volume's mount point and cannot be renamed, so its contents move aside into
  # a sibling directory INSIDE the volume — a same-filesystem rename, unlike the
  # move that follows, which crosses from the container's writable layer into
  # the volume and is therefore a copy. If that copy fails partway, the old
  # contents go back, so the failure costs a run instead of the volume.
  #
  # The cost is honest: holding both trees at once needs roughly twice the free
  # space the old delete-then-move needed, so an upgrade with between one and
  # two trees of headroom now fails where it used to succeed. That is the trade
  # taken deliberately — the old code's version of "succeeds" was to destroy a
  # working dependency set on any partial failure, which left the stack
  # unstartable at ANY version until the npm registry was reachable again, and
  # recovering from THIS failure is just re-running with more disk.
  mkdir -p "$PREV_DIR"
  find node_modules -mindepth 1 -maxdepth 1 ! -name .previous-install -exec mv -t "$PREV_DIR/" {} +
  moved_aside="$(find "$PREV_DIR" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
  if ! find "$STAGE_DIR/node_modules" -mindepth 1 -maxdepth 1 -exec mv -t node_modules/ {} +; then
    echo "==> [migrator] dependency swap failed; restoring the previous dependency tree..." >&2
    # `|| true` on the clear: a failure there must not exit before the restore
    # runs, because this is the one path whose whole job is to put the old tree
    # back.
    find node_modules -mindepth 1 -maxdepth 1 ! -name .previous-install -exec rm -rf {} + || true
    # No `|| true` on the restore itself: this is the one path whose entire job
    # is to put the tree back, and exiting silently would be worse than the
    # clear-failure message below.
    if ! find "$PREV_DIR" -mindepth 1 -maxdepth 1 -exec mv -t node_modules/ {} +; then
      echo "==> [migrator] FATAL: the dependency swap failed AND the previous tree could not be moved back; it is still under node_modules/.previous-install. Free disk space and retry \`docker compose up\`." >&2
      exit 1
    fi
    rm -rf "$PREV_DIR" "$STAGE_DIR"
    # Reports what was moved back, not what the operator can now do with it:
    # this counts directory entries, which is evidence that something was
    # restored, not that it is a complete install.
    echo "==> [migrator] FATAL: could not install the new dependency set. ${moved_aside} previous dependency entr(y/ies) were moved back into the volume; if that was a complete install, the stack can still start on its previous version." >&2
    exit 1
  fi
  rm -rf "$PREV_DIR" "$STAGE_DIR"
  printf '%s\n' "$LOCK_HASH" > "$LOCK_MARKER"
fi

echo "==> [migrator] waiting for the database to accept connections..."
# `prisma db execute` (Prisma 7, see prisma.config.ts) takes no --url flag: the
# CLI reads the datasource URL exclusively from prisma.config.ts, which itself
# reads DATABASE_URL from the environment. Passing --url is a hard "unknown or
# unexpected option" error, which in this wait loop looks exactly like a
# database that never becomes reachable.
db_ready=false
for _ in $(seq 1 60); do
  if npx --yes prisma db execute --stdin <<<'SELECT 1;' >/dev/null 2>&1; then
    db_ready=true
    break
  fi
  sleep 2
done
if [ "$db_ready" != true ]; then
  echo "==> [migrator] FATAL: gave up waiting for the database after 60 attempts (120s) — check DATABASE_URL and that the database container is healthy." >&2
  exit 1
fi

echo "==> [migrator] applying schema (prisma db push)..."
# --skip-generate was a flag on older Prisma CLI versions to skip regenerating
# the client after push. Prisma 7's `db push` has no such flag (see its
# `Usage` output: --schema/--url/--accept-data-loss/--force-reset only) and
# rejects it outright with "unknown or unexpected option" — this image never
# runs `prisma generate` in the db:push step anyway (only `seed` does), so
# dropping it changes nothing about what gets generated.
npm run db:push -- --accept-data-loss

echo "==> [migrator] applying schema-extras.sql..."
npx --yes prisma db execute --file schema-extras.sql

echo "==> [migrator] seeding..."
# Default to the two-tier seed (reference data + demo tenant with sample
# provider credentials): the maintainer's explicit choice for this quickstart
# so a fresh `docker compose up` gives an operator something to look at
# immediately, not an empty shell. This image runs the same entrypoint for
# every container form factor; the appliance's production first-boot path
# (nexus-ami/scripts/first-boot-db.sh) passes SEED_DEMO=false explicitly to
# opt out for that deployment, rather than relying on the image default.
# Demo data needs CREDENTIAL_ENCRYPTION_KEY to encrypt the placeholder
# provider credentials it inserts — deploy/docker-compose.yml wires it into
# db-migrator's environment for exactly this reason. Callers that want
# reference-only data opt out explicitly with SEED_DEMO=false.
#
# The failure is RECORDED and re-raised at the very end of this script rather
# than allowed to abort it here, because `set -e` would stop between the seed
# and the rotation below — the one ordering this script must never produce.
# The seed writes the PUBLIC credentials on its way through: bootstrap rewrites
# admin@nexus.ai's hash to hashPassword("nexus-demo") on every run, and the demo
# tier writes 30 more rows whose plaintexts are published in the OSS repository.
# seed.ts also runs each tier even when an earlier one failed (a reference-table
# conflict must not leave an install with no super-admin), so a failing seed
# still leaves those values in the database. Aborting here would leave them live
# and reachable: on an existing stack, `docker compose up -d` keeps the previous
# containers serving :8080 and :3050 for as long as it takes an operator to
# notice the migrator failed. The exit code is unchanged — the rotation just
# happens before it.
seed_failed=0
if [ "${SEED_DEMO:-true}" = "true" ]; then
  npm run seed || seed_failed=1
else
  npm run seed:prod || seed_failed=1
fi
if [ "$seed_failed" = 1 ]; then
  echo "==> [migrator] the seed FAILED (see the [seed] output above). Continuing to credential rotation so no public seed credential is left live; this run will exit non-zero at the end." >&2
fi

# ── Credential rotation ──────────────────────────────────────────────────
# Every step below runs even if an earlier one failed, and the script exits
# non-zero at the end if any did. This is the same rule as the seed above and it
# matters for the same reason: under `set -e` a step that aborts takes every
# rotation ordered after it with it, and a rotation that does not run is a
# credential published in this repository left live in a database. Nothing here
# is allowed to be the reason a later rotation is skipped.
rotation_failed=0

# The bootstrap seed ships admin@nexus.ai with the password "nexus-demo" and
# the system-assistant Virtual Key with a deterministic plaintext, both public
# in the OSS repository, so that a development machine works out of the box.
# Neither may survive into a deployment anyone can reach. This mirrors what
# nexus-ami/scripts/first-boot-db.sh does on the appliance.
# (NEXUS_ADMIN_PASSWORD / NEXUS_ASSISTANT_SYSTEM_VK / ADMIN_KEY_HMAC_SECRET
# are already guarded at the top of this script.)

echo "==> [migrator] replacing the seeded admin password..."
# Wrapped in a row-count assertion because `prisma db execute` reports success
# whether an UPDATE matched one row or none. A rename of the seeded
# administrator email, or a stale db-migrator tag run against a newer seed
# (NEXUS_VERSION and NEXUS_REGISTRY make that easy), would otherwise print
# "Script executed successfully", exit 0, start every dependent service, and
# leave the account on hashPassword("nexus-demo") — a password published in the
# repository. The demo-tier rotation already asserts its own row counts; these
# two are the credentials that matter most and had no such check.
# The helper is a separate process with its own preconditions — it rejects a
# password under 8 characters, for one — and a bare `VAR="$(...)"` assignment
# under `set -e` aborts the script, taking the assistant-key restamp and the
# whole demo-tier rotation with it. That is the ordering this block exists to
# prevent, so the failure is recorded like any other.
ADMIN_HASH=""
if ! ADMIN_HASH="$(NEW_PASSWORD="$NEXUS_ADMIN_PASSWORD" node /opt/nexus-scripts/set-admin-password.js)"; then
  rotation_failed=1
  ADMIN_HASH=""
  echo "==> [migrator] could not compute the new admin password hash; continuing with the remaining rotations." >&2
fi
if [ -n "$ADMIN_HASH" ] && ! npx --yes prisma db execute --stdin <<SQL
DO \$\$
DECLARE matched integer;
BEGIN
  UPDATE "NexusUser" SET "passwordHash" = '${ADMIN_HASH}', "passwordUpdatedAt" = NOW() WHERE email = 'admin@nexus.ai';
  GET DIAGNOSTICS matched = ROW_COUNT;
  IF matched <> 1 THEN
    RAISE EXCEPTION 'admin password rotation matched % rows for admin@nexus.ai (expected 1) - the seeded administrator would keep its public password', matched;
  END IF;
END
\$\$;
SQL
then
  rotation_failed=1
  echo "==> [migrator] admin password rotation FAILED; continuing with the remaining rotations." >&2
fi

echo "==> [migrator] restamping the system-assistant virtual key..."
# The plaintext comes from .env because Compose reads .env before any container
# starts: the control plane already holds NEXUS_ASSISTANT_SYSTEM_VK by the time
# this runs, so the migrator's job is to make the stored hash agree with it.
VK_ID="b0075000-0000-4000-8000-0000000000a2"
# Same reasoning as the admin hash above: mint-assistant-vk.js rejects a
# plaintext that is not "nvk_" + at least 40 hex characters, and an operator
# picks this value.
VK_LINE=""
if ! VK_LINE="$(ASSISTANT_VK_PLAINTEXT="$NEXUS_ASSISTANT_SYSTEM_VK" node /opt/nexus-scripts/mint-assistant-vk.js)"; then
  rotation_failed=1
  VK_LINE=""
  echo "==> [migrator] could not mint the system-assistant key hash; continuing with the remaining rotations." >&2
fi
VK_HASH="$(printf '%s' "$VK_LINE" | cut -f2)"
VK_PREFIX="$(printf '%s' "$VK_LINE" | cut -f3)"
if [ -n "$VK_LINE" ] && ! npx --yes prisma db execute --stdin <<SQL
DO \$\$
DECLARE matched integer;
BEGIN
  UPDATE "VirtualKey" SET "keyHash" = '${VK_HASH}', "keyPrefix" = '${VK_PREFIX}' WHERE id = '${VK_ID}';
  GET DIAGNOSTICS matched = ROW_COUNT;
  IF matched <> 1 THEN
    RAISE EXCEPTION 'system-assistant key restamp matched % rows for id ${VK_ID} (expected 1) - the assistant key would keep its public plaintext', matched;
  END IF;
END
\$\$;
SQL
then
  rotation_failed=1
  echo "==> [migrator] system-assistant key restamp FAILED; continuing with the remaining rotations." >&2
fi

# The demo tier (SEED_DEMO=true, the default above) ships its OWN 13
# NexusUser / 12 VirtualKey / 5 AdminApiKey rows with plaintexts
# deterministically computable from each row's fixture id alone — also
# public in the OSS repository. The two UPDATEs above only cover the
# bootstrap admin and the system-assistant VK; nothing above this line
# touches the demo tier. See rotate-demo-secrets.mjs for the row-selection
# rule and rotate-demo-secret.js for the per-install derivation.
#
# This script runs on EVERY `docker compose up`, not just the first — the
# documented upgrade path is `docker compose pull && docker compose up -d`
# with no "already initialized" gate. rotate-demo-secrets.mjs's selection is
# therefore EVIDENCE-BASED (rotate a row only if its stored hash still
# equals the known PUBLIC seed value), not exclusion-based — an
# exclusion-only rule ("every source='local' NexusUser except the bootstrap
# admin", etc.) would equal "only the demo rows" solely on a first boot
# against an empty database, and would overwrite operator-created rows
# (admin-API-created users/keys also default to source='local') on every
# later run. When SEED_DEMO=false this is a no-op because no demo rows
# exist to match the public-seed-value check in the first place.
echo "==> [migrator] rotating demo-tier secrets (users, virtual keys, admin API keys)..."
if ! node rotate-demo-secrets.mjs; then
  rotation_failed=1
  echo "==> [migrator] demo-tier rotation FAILED; continuing so the console callback is still registered." >&2
fi

# ── Console redirect URI ─────────────────────────────────────────────────
# Deliberately AFTER every rotation above. Under `set -e` a failure in any step
# ordered before the rotations takes the rotations with it, so nothing that is
# not itself a credential rotation belongs in front of them: a transient
# database error on this one UPDATE would otherwise leave 30 public demo
# credentials live.
#
# Without this step the console cannot be logged into at all. The SPA derives
# its redirect_uri from the browser origin (packages/control-plane-ui/src/auth/
# pkce/pkceFlow.ts builds it from window.location.origin), but the seed fixture
# can only ship the localhost:3000 URLs a `npm run dev` developer uses — it
# cannot know the origin an install answers on. /oauth/authorize rejects an
# unregistered redirect_uri outright: store.RedirectAllowed accepts an exact
# match, or matchLoopback, which compares the port exactly unless the
# registered pattern carries a ":*" wildcard — and cp-ui's patterns do not. So
# a browser on this deployment's origin gets 400 invalid_request and the login
# form never renders.
#
# nexus-ami/scripts/first-boot.sh does the same thing for the appliance, whose
# per-instance address is likewise unknowable to a fixture.
#
# Idempotent by the same NOT ... = ANY(...) guard, and a re-seed cannot undo it:
# OAuthClient.redirectUris is in the seed's UNION_FIXTURE_FIELDS
# (tools/db-migrate/seed/reference/index.ts), which unions the fixture values in
# rather than replacing the column — precisely so a re-seed run for an unrelated
# reason cannot lock every admin out of the console.
echo "==> [migrator] registering this deployment's console redirect URI..."
# Trailing slash stripped so the value composes into exactly one callback URL
# whether or not the operator wrote CONTROL_PLANE_PUBLIC_URL with one.
CONSOLE_ORIGIN="${CONTROL_PLANE_PUBLIC_URL%/}"
CONSOLE_CALLBACK="${CONSOLE_ORIGIN}/auth/callback"
# Asserted as a POST-condition rather than a row count: the UPDATE deliberately
# matches zero rows when the callback is already registered, so "one row
# changed" is the wrong question. "Is it registered now" is the right one, and
# it also catches the case where no cp-ui client row exists at all.
npx --yes prisma db execute --stdin <<SQL
DO \$\$
DECLARE registered boolean;
BEGIN
  UPDATE "OAuthClient"
  SET "redirectUris" = array_append("redirectUris", '${CONSOLE_CALLBACK}'),
      "updatedAt" = NOW()
  WHERE "id" = 'cp-ui'
    AND NOT ('${CONSOLE_CALLBACK}' = ANY("redirectUris"));

  SELECT '${CONSOLE_CALLBACK}' = ANY("redirectUris") INTO registered FROM "OAuthClient" WHERE "id" = 'cp-ui';
  IF registered IS NOT TRUE THEN
    RAISE EXCEPTION 'console callback ${CONSOLE_CALLBACK} is not registered on the cp-ui OAuth client (row missing?) - /oauth/authorize would reject every login';
  END IF;
END
\$\$;
SQL
echo "    console callback registered: ${CONSOLE_CALLBACK}"

if [ "$rotation_failed" = 1 ]; then
  echo "==> [migrator] FATAL: at least one credential rotation failed (see the messages above). A credential whose plaintext is published in this repository may still be live in this database — do not expose this deployment until a run completes cleanly." >&2
  exit 1
fi

if [ "$seed_failed" = 1 ]; then
  echo "==> [migrator] FATAL: the seed failed earlier in this run. Every credential rotation above still ran, so no public seed credential was left live, but the database is not fully seeded. Fix the cause reported by [seed] above and re-run \`docker compose up\`." >&2
  exit 1
fi

# The guards at the top of this script catch an UNSET admin password. They
# cannot catch an operator who sets it to the seed's own published value:
# set-admin-password.js rejects only passwords under 8 characters, and the
# seeded default is longer than that, so such a run rotates "successfully" into
# a deployment whose super-admin password is published in this repository.
#
# The value is read out of the seed source that defines it (this image carries
# the whole tools/db-migrate tree) rather than repeated here, so the check
# cannot drift away from the password it is meant to recognise. If the constant
# is ever renamed the extraction yields nothing and the check goes quiet, which
# is the right failure direction for a warning.
#
# A warning, not a refusal: a local quickstart may legitimately choose the
# memorable value, and refusing would break `ADMIN_PASSWORD=<seed default>
# ./init-secrets.sh`.
SEED_PASSWORD="$(sed -n "s/^export const BOOTSTRAP_PASSWORD = '\\(.*\\)'\\r*$/\\1/p" \
  /app/tools/db-migrate/seed/bootstrap/index.ts 2>/dev/null || true)"
if [ -n "$SEED_PASSWORD" ] && [ "$NEXUS_ADMIN_PASSWORD" = "$SEED_PASSWORD" ]; then
  echo "==> [migrator] WARNING: NEXUS_ADMIN_PASSWORD is the seed's published default." >&2
  echo "    admin@nexus.ai now logs in with a password anyone can read in this" >&2
  echo "    repository. Fine for a local quickstart; do NOT expose this deployment." >&2
  echo "    To fix: rm deploy/.env && ./init-secrets.sh (it generates a random one)." >&2
fi

# Where an operator finds the console credentials. init-secrets.sh prints the
# password once, at generation time, which is several hundred lines of `up`
# output before anyone tries to sign in; this line runs on every `up` and is
# what `docker compose logs db-migrator` shows them.
#
# The email, not the password: this output lands in whatever collects the
# deployment's container logs, and a super-admin password does not belong
# there. The pointer is enough — the value is in .env on the host.
echo "==> [migrator] console sign-in: ${NEXUS_ADMIN_EMAIL:-admin@nexus.ai}"
echo "    password: the NEXUS_ADMIN_PASSWORD line in deploy/.env on the host"

echo "==> [migrator] done."
