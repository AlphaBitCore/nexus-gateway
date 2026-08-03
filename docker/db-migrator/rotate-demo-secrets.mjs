// rotate-demo-secrets.mjs — post-seed step, container migrator only.
//
// tools/db-migrate/seed/demo/index.ts (SEED_DEMO=true, the image default)
// re-stamps every NexusUser / VirtualKey / AdminApiKey demo row with a
// plaintext deterministically computable from the row's fixture id ALONE
// (DEMO_PASSWORD, demoVkKey(id), demoAdminKey(id)) — public information
// committed to the OSS repository:
//   - 13 NexusUser rows (source: local), password "nexus-demo"
//   - 12 VirtualKey rows, plaintext "nvk_demo_<id[:8]>"
//   -  5 AdminApiKey rows, plaintext "nak_demo_<id[:8]>" (two of them
//      members of the super-admins IAM group)
// entrypoint.sh's existing rotation block only replaces the TWO bootstrap
// credentials (admin@nexus.ai's password, the system-assistant VK). Nothing
// touched these demo rows, so a published quickstart exposed a working
// super-admin API key on :8080/api and 12 working virtual keys on :3050/v1
// to anyone who read the repo. This script closes that gap.
//
// ── Row selection is EVIDENCE-BASED, not exclusion-based ───────────────────
//
// A row is rotated ONLY if its stored hash still equals the hash of the
// KNOWN PUBLIC seed plaintext for that row's id. This is a deliberate change
// from an earlier, exclusion-only version of this script ("rotate every
// source='local' NexusUser except the bootstrap admin", "every VirtualKey
// except the assistant VK", "every AdminApiKey") — that rule equals "only the
// demo rows" ONLY on a first boot against an empty database. This script runs
// on EVERY `docker compose up` (db-migrator has no "already ran" gate, and
// the documented upgrade path is literally `docker compose pull && docker
// compose up -d`), and admin-API-created users/keys also default to
// source='local' / land in these same tables. Under the exclusion rule, the
// FIRST rotation pass after an operator created a row would silently
// overwrite that row's hash — locking the operator out of a user they
// created, or 401-ing a virtual key they had already deployed into an app or
// agent. Idempotence (re-running reproduces the SAME rotated value) does not
// save an operator row: the row never HAD that rotated value to begin with,
// so the first rotation after creation still destroys the value the operator
// was actually given.
//
// The evidence-based predicate fixes this: it only ever touches a row whose
// hash still verifies against the public seed plaintext, so it is a no-op on
// (a) a row an operator created (never had the public plaintext), and (b) a
// row this script already rotated on a previous run (its hash now matches
// the ROTATED plaintext, not the public one). It stays a RULE — no
// enumerated id list — so a demo fixture added later is still covered
// automatically, provided it follows the same public-plaintext convention.
//
//   NexusUser:   organizationId is a member of the demo-org set (derived at
//                runtime from tools/db-migrate/seed/fixtures/demo/
//                Organization.json — see loadDemoOrgIds() below, never a
//                hardcoded id list) AND the stored passwordHash VERIFIES
//                against the public plaintext DEMO_PASSWORD ("nexus-demo",
//                tools/db-migrate/seed/demo/index.ts). Verification (not
//                recomputation) is required because hashPassword() salts
//                randomly — a fresh hash of "nexus-demo" would never equal a
//                previously stored one even for a genuine demo row. The
//                bootstrap admin id is ALSO excluded as belt-and-suspenders:
//                entrypoint.sh rotates admin@nexus.ai to NEXUS_ADMIN_PASSWORD
//                (a non-"nexus-demo" value) earlier in the same script, so by
//                the time this runs its hash already fails the verify check
//                on its own — the id exclusion is redundant given that
//                ordering but cheap insurance against the ordering changing.
//
//                The org-membership condition exists because the password
//                check ALONE is not evidence enough for this table: an
//                operator can deliberately set their OWN account's password
//                to the literal string "nexus-demo" (nothing stops them),
//                which would then verify against DEMO_PASSWORD exactly like
//                a genuine demo row and get silently rotated out from under
//                them. Demo NexusUser rows only ever live in demo
//                organizations (org fixture ids "10000000-*" and
//                "5fabaad6-..."; the bootstrap org "b0075000-...-a0" is NOT
//                one of them), so requiring organizationId to be in that set
//                closes the collision without weakening the evidence-based
//                rule: a row must satisfy BOTH conditions to be touched.
//   VirtualKey:  the stored keyHash equals HMAC-SHA256(subkey, plaintext)
//                for plaintext = demoVkKey(id) = "nvk_demo_<id[:8]>" (the
//                seed's own formula), where subkey is deriveSubkey() under
//                the VIRTUAL-KEY trust-domain class. The system-assistant VK
//                naturally fails this check (its plaintext is
//                "nvk_local_<id[:8]>", a different domain — see
//                tools/db-migrate/seed/bootstrap/index.ts), so the id
//                exclusion below is belt-and-suspenders, not load-bearing.
//   AdminApiKey: the stored keyHash equals HMAC-SHA256(subkey, plaintext)
//                for plaintext = demoAdminKey(id) = "nak_demo_<id[:8]>",
//                subkey under the ADMIN trust-domain class (NOT the
//                virtual-key class — nexus/apikey/admin/v1). Bootstrap seeds
//                no AdminApiKey rows at all, so there is no bootstrap row to
//                exclude here.
//
//                VirtualKey and AdminApiKey deliberately get NO org-
//                membership condition (unlike NexusUser above): both
//                plaintexts are server-generated random tokens minted by the
//                admin API (nvk_<48 hex>, nak_<...>), so an operator cannot
//                choose one and make it collide with demoVkKey(id) /
//                demoAdminKey(id) — the HMAC-equality check is already
//                unforgeable evidence on its own. AdminApiKey additionally
//                has no organizationId column to condition on at all.
//
// Idempotent: nexus-ami/scripts/rotate-demo-secret.js derives each
// credential as a pure function of (ADMIN_KEY_HMAC_SECRET, row id, kind), so
// re-running this script (the next `docker compose up`, or a migrator retry)
// reproduces the SAME credentials rather than invalidating ones an operator
// was already given — AND, per the evidence-based predicate above, a second
// run finds the row's hash no longer matches the public seed value (it now
// matches the previously-rotated value) and correctly leaves it alone.

import pkg from 'pg';
import { execFileSync } from 'node:child_process';
import { scryptSync, timingSafeEqual, hkdfSync, createHmac } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const { Client } = pkg;

// Fixed ids for the two rows rotated separately, above, under operator-
// supplied secrets (NEXUS_ADMIN_PASSWORD / NEXUS_ASSISTANT_SYSTEM_VK) —
// belt-and-suspenders exclusions layered ON TOP of the evidence-based checks
// below, not the primary selection mechanism. See the header comment.
const BOOTSTRAP_ADMIN_ID = 'nexus-user-super-admin';
const ASSISTANT_VK_ID = 'b0075000-0000-4000-8000-0000000000a2';

const HELPER = '/opt/nexus-scripts/rotate-demo-secret.js';

// ── Demo-org set (NexusUser scoping) ────────────────────────────────────────
//
// docker/db-migrator/Dockerfile COPYs the whole tools/db-migrate/ tree
// (including seed/fixtures/) into /app/tools/db-migrate — the same directory
// this script itself lands in — so the fixture is always present alongside
// it at runtime. Read as a RULE off the fixture file, not a hardcoded id
// list, so a demo org fixture added later is covered automatically without
// touching this script.
const __dirname = dirname(fileURLToPath(import.meta.url));
const DEMO_ORG_FIXTURE_PATH = join(__dirname, 'seed', 'fixtures', 'demo', 'Organization.json');

/** Returns the Set of organization ids seeded by the demo fixture. */
function loadDemoOrgIds() {
  const raw = readFileSync(DEMO_ORG_FIXTURE_PATH, 'utf8');
  const orgs = JSON.parse(raw);
  if (!Array.isArray(orgs) || orgs.length === 0) {
    throw new Error(
      `rotate-demo-secrets: ${DEMO_ORG_FIXTURE_PATH} did not parse to a non-empty array of organizations`,
    );
  }
  const ids = orgs.map((org) => org.id).filter((id) => typeof id === 'string' && id.length > 0);
  if (ids.length !== orgs.length) {
    throw new Error(
      `rotate-demo-secrets: ${DEMO_ORG_FIXTURE_PATH} contains an organization row with a missing/invalid id`,
    );
  }
  return new Set(ids);
}

// ── Public-seed-value detection ─────────────────────────────────────────────
//
// These mirror, byte-for-byte, the PUBLIC formulas the seed uses to stamp a
// demo row (tools/db-migrate/seed/demo/index.ts DEMO_PASSWORD / demoVkKey /
// demoAdminKey) and the hashing scheme the running services verify against
// (tools/db-migrate/seed/lib.ts hashPassword / hashVirtualKey / hashAdminKey,
// packages/shared/core/keyderive). They exist ONLY to answer "does this row
// still carry the public plaintext" — the REPLACEMENT plaintext is a
// different, secret-dependent derivation computed by rotate-demo-secret.js.

const DEMO_PASSWORD = 'nexus-demo';
const SCRYPT_KEY_LENGTH = 64;
// MUST match tools/db-migrate/seed/lib.ts SCRYPT_OPTIONS /
// packages/control-plane/internal/identity/authn/password.go scryptN.
const SCRYPT_OPTIONS = { N: 1 << 17, r: 8, p: 1, maxmem: 256 * 1024 * 1024 };

// MUST byte-match packages/shared/core/keyderive Class* constants.
const CLASS_API_KEY_VIRTUAL_KEY = 'nexus/apikey/virtual-key/v1';
const CLASS_API_KEY_ADMIN = 'nexus/apikey/admin/v1';

const demoVkKey = (id) => `nvk_demo_${id.slice(0, 8)}`;
const demoAdminKey = (id) => `nak_demo_${id.slice(0, 8)}`;

/**
 * True iff `stored` ("saltHex:hashHex") verifies against the public demo
 * password. Verification, not recomputation, because hashPassword() salts
 * randomly — recomputing a fresh hash of DEMO_PASSWORD would never equal a
 * previously stored hash even for a genuine, still-public demo row.
 */
function isPublicDemoPasswordHash(stored) {
  if (typeof stored !== 'string') return false;
  const sep = stored.indexOf(':');
  if (sep < 0) return false;
  const saltHex = stored.slice(0, sep);
  const hashHex = stored.slice(sep + 1);
  let salt;
  let expected;
  try {
    salt = Buffer.from(saltHex, 'hex');
    expected = Buffer.from(hashHex, 'hex');
  } catch {
    return false;
  }
  if (salt.length === 0 || expected.length !== SCRYPT_KEY_LENGTH) return false;
  const derived = scryptSync(DEMO_PASSWORD, salt, SCRYPT_KEY_LENGTH, SCRYPT_OPTIONS);
  return derived.length === expected.length && timingSafeEqual(derived, expected);
}

// Derive the 32-byte per-domain sub-key exactly as keyderive.DeriveSubkey
// does: HKDF-SHA256 over the RAW secret-string bytes, empty salt, class as
// info. Same derivation as tools/db-migrate/seed/lib.ts deriveSubkey() /
// nexus-ami/scripts/rotate-demo-secret.js deriveSubkey().
function deriveSubkey(classInfo) {
  const secret = process.env.ADMIN_KEY_HMAC_SECRET.trim();
  const ikm = Buffer.from(secret, 'utf8');
  return Buffer.from(hkdfSync('sha256', ikm, Buffer.alloc(0), Buffer.from(classInfo, 'utf8'), 32));
}

/** True iff `storedHash` equals HMAC-SHA256(subkey, demoVkKey(id)). */
function isPublicDemoVkHash(id, storedHash) {
  if (typeof storedHash !== 'string' || storedHash.length === 0) return false;
  const expected = createHmac('sha256', deriveSubkey(CLASS_API_KEY_VIRTUAL_KEY))
    .update(demoVkKey(id))
    .digest('hex');
  return storedHash === expected;
}

/** True iff `storedHash` equals HMAC-SHA256(subkey, demoAdminKey(id)). */
function isPublicDemoAdminKeyHash(id, storedHash) {
  if (typeof storedHash !== 'string' || storedHash.length === 0) return false;
  const expected = createHmac('sha256', deriveSubkey(CLASS_API_KEY_ADMIN))
    .update(demoAdminKey(id))
    .digest('hex');
  return storedHash === expected;
}

function rotateOne(kind, id) {
  const out = execFileSync('node', [HELPER], {
    env: { ...process.env, ROTATE_KIND: kind, ROTATE_ID: id },
    encoding: 'utf8',
  });
  return out.split('\t');
}

function padName(name, width) {
  return name.length >= width ? name.slice(0, width) : name + ' '.repeat(width - name.length);
}

async function main() {
  const demoOrgIds = loadDemoOrgIds();

  const client = new Client({ connectionString: process.env.DATABASE_URL });
  await client.connect();

  const rotatedUsers = [];
  const rotatedVks = [];
  const rotatedAdminKeys = [];
  let skippedUsers = 0;
  let skippedVks = 0;
  let skippedAdminKeys = 0;

  // A row that cannot be rotated must not stop the rows after it, and above all
  // must not stop the CLASSES after it: the three loops below run in the order
  // NexusUser, VirtualKey, AdminApiKey, so a single throw in the first one used
  // to leave 12 published virtual keys and 5 published admin API keys — one of
  // them a never-expiring member of the super-admins group — untouched in a
  // database an operator's still-running containers are serving. Worse, when the
  // fault is deterministic (a missing helper, an unreadable fixture, a role
  // without UPDATE), every later `docker compose up` aborts at the same row and
  // those two classes are rotated on no run, ever. Failures are collected and
  // re-raised together after every class has had its turn, which is the same
  // rule docker/db-migrator/entrypoint.sh applies to the steps around this one.
  const failures = [];
  const attempt = (kind, id, fn) =>
    fn().then(
      (v) => v,
      (err) => {
        failures.push({ kind, id, err });
        console.error(`==> [migrator] rotating ${kind} ${id} FAILED: ${err && err.message ? err.message : err}`);
        return null;
      },
    );
  // Per-CLASS isolation as well as per-row: the SELECT that opens each class,
  // and the predicate that decides whether a row is still public, are outside
  // any row-level attempt(). A connection dropped mid-run would otherwise take
  // every later class with it — and with it the banner and the aggregate, so
  // the operator would not even learn which rows had already been rotated.
  const attemptClass = (kind, fn) =>
    fn().then(
      (v) => v,
      (err) => {
        failures.push({ kind, id: '(whole class)', err });
        console.error(`==> [migrator] rotating the ${kind} class FAILED: ${err && err.message ? err.message : err}`);
        return null;
      },
    );

  try {
    // ── NexusUser ──────────────────────────────────────────────────────
    await attemptClass('password', async () => {
      // organizationId = ANY(demo org set) is the collision guard: without it,
      // an operator who deliberately set their OWN account's password to the
      // literal string "nexus-demo" would verify against DEMO_PASSWORD just
      // like a genuine demo row and get silently rotated. Demo rows only ever
      // live in demo orgs, so this condition is free of false negatives against
      // the actual demo tier while closing the collision.
      const users = await client.query(
        'SELECT id, email, "passwordHash" FROM "NexusUser" WHERE source = $1 AND id <> $2 AND "organizationId" = ANY($3) ORDER BY email',
        ['local', BOOTSTRAP_ADMIN_ID, [...demoOrgIds]],
      );
      for (const row of users.rows) {
        if (!isPublicDemoPasswordHash(row.passwordHash)) {
          // Not the public seed value — either an operator-created row, or a
          // row this script already rotated on a previous run. Leave it alone.
          skippedUsers++;
          continue;
        }
        const ok = await attempt('password', row.id, async () => {
          // The plaintext is deliberately dropped on the floor: this process's
          // stdout is the container log, and nothing downstream needs it (see the
          // banner below).
          const [, hash] = rotateOne('password', row.id);
          const res = await client.query(
            'UPDATE "NexusUser" SET "passwordHash" = $1, "passwordUpdatedAt" = NOW() WHERE id = $2',
            [hash, row.id],
          );
          if (res.rowCount !== 1) {
            throw new Error(`rotate-demo-secrets: NexusUser UPDATE matched ${res.rowCount} rows for id ${row.id} (expected 1)`);
          }
          return true;
        });
        if (ok) rotatedUsers.push({ name: row.email, id: row.id, kind: 'password' });
      }
    });

    // ── VirtualKey ─────────────────────────────────────────────────────
    await attemptClass('virtual-key', async () => {
      const vks = await client.query('SELECT id, name, "keyHash" FROM "VirtualKey" WHERE id <> $1 ORDER BY name', [
        ASSISTANT_VK_ID,
      ]);
      for (const row of vks.rows) {
        if (!isPublicDemoVkHash(row.id, row.keyHash)) {
          skippedVks++;
          continue;
        }
        const ok = await attempt('virtual-key', row.id, async () => {
          const [, keyHash, keyPrefix] = rotateOne('virtual-key', row.id);
          const res = await client.query('UPDATE "VirtualKey" SET "keyHash" = $1, "keyPrefix" = $2 WHERE id = $3', [
            keyHash,
            keyPrefix,
            row.id,
          ]);
          if (res.rowCount !== 1) {
            throw new Error(`rotate-demo-secrets: VirtualKey UPDATE matched ${res.rowCount} rows for id ${row.id} (expected 1)`);
          }
          return true;
        });
        if (ok) rotatedVks.push({ name: row.name, id: row.id, kind: 'virtual-key' });
      }
    });

    // ── AdminApiKey ────────────────────────────────────────────────────
    await attemptClass('admin-key', async () => {
      const adminKeys = await client.query('SELECT id, name, "keyHash" FROM "AdminApiKey" ORDER BY name');
      for (const row of adminKeys.rows) {
        if (!isPublicDemoAdminKeyHash(row.id, row.keyHash)) {
          skippedAdminKeys++;
          continue;
        }
        const ok = await attempt('admin-key', row.id, async () => {
          const [, keyHash, keyPrefix] = rotateOne('admin-key', row.id);
          const res = await client.query('UPDATE "AdminApiKey" SET "keyHash" = $1, "keyPrefix" = $2 WHERE id = $3', [
            keyHash,
            keyPrefix,
            row.id,
          ]);
          if (res.rowCount !== 1) {
            throw new Error(`rotate-demo-secrets: AdminApiKey UPDATE matched ${res.rowCount} rows for id ${row.id} (expected 1)`);
          }
          return true;
        });
        if (ok) rotatedAdminKeys.push({ name: row.name, id: row.id, kind: 'admin-key' });
      }
    });
  } finally {
    await client.end();
  }

  // ── Banner ───────────────────────────────────────────────────────────
  // What was rotated, NOT what it was rotated to. This process writes to the
  // container's stdout, which Docker persists as a json-file log and any log
  // collector ships onward — a much wider audience than deploy/.env (mode 600),
  // which the operator doc presents as the only copy of a deployment's secrets.
  // Two of the three row classes here carry super-admin authority (the
  // "super-admin" AdminApiKey is a never-expiring member of the super-admins
  // IAM group; one demo NexusUser is the console super-admin), so printing them
  // would put full administrative takeover into every log stream.
  //
  // Nothing is lost by withholding them, because the derivation is a pure
  // function of (ADMIN_KEY_HMAC_SECRET, kind, row id): an operator holding .env
  // can reproduce any one of these on demand with the command below, and the
  // release smoke already recovers them that way rather than by parsing this
  // output. Names and ids ARE printed — the ids are public fixture values, and
  // without them the command below cannot be aimed.
  const nameWidth = 30;
  const rotated = [...rotatedUsers, ...rotatedVks, ...rotatedAdminKeys];
  console.log('');
  console.log('╔═══════════════════════════════════════════════════════════════╗');
  console.log('║       ROTATED DEMO CREDENTIALS (per-install — not public)    ║');
  console.log('╠═══════════════════════════════════════════════════════════════╣');
  if (rotated.length === 0) {
    console.log('║  nothing to rotate — no row still holds a public seed value.');
  }
  for (const r of rotated) {
    console.log(`║  ${padName(r.name, nameWidth)} ${r.kind}  ${r.id}`);
  }
  console.log('╠═══════════════════════════════════════════════════════════════╣');
  console.log('║  The new values are NOT printed: this is the container log.');
  console.log('║  Re-derive any one of them (same value every time) with:');
  console.log('║    docker compose run --rm --entrypoint node \\');
  console.log('║      -e ROTATE_KIND=<kind above> -e ROTATE_ID=<id above> \\');
  console.log('║      db-migrator /opt/nexus-scripts/rotate-demo-secret.js');
  console.log('╚═══════════════════════════════════════════════════════════════╝');
  console.log('');
  // The "left N untouched" half is only meaningful for a class that was actually
  // examined. A class whose SELECT failed contributes zeroes to both halves,
  // which would otherwise read as "there was nothing to do".
  const failedClasses = failures.filter((f) => f.id === '(whole class)').map((f) => f.kind);
  console.log(
    `==> [migrator] rotated ${rotatedUsers.length} demo user password(s), ${rotatedVks.length} demo virtual key(s), ${rotatedAdminKeys.length} demo admin API key(s); ` +
      `left ${skippedUsers} user(s), ${skippedVks} virtual key(s), ${skippedAdminKeys} admin API key(s) untouched (not the public seed value — operator-created or already rotated).` +
      (failedClasses.length > 0
        ? ` THOSE COUNTS ARE PARTIAL: the ${failedClasses.join(' and ')} class(es) could not be examined at all, so nothing is known about their rows.`
        : ''),
  );

  // Raised only after the banner, so an operator whose run failed partway still
  // learns which rows DID rotate — those now hold values they were never shown,
  // and the list above is how they aim the re-derive command at them.
  if (failures.length > 0) {
    throw new Error(
      `rotate-demo-secrets: ${failures.length} row(s) could not be rotated and still hold a plaintext published in this repository: ` +
        failures.map((f) => `${f.kind} ${f.id} (${f.err && f.err.message ? f.err.message : f.err})`).join('; '),
    );
  }
}

main().catch((err) => {
  console.error('==> [migrator] FATAL: demo-secret rotation failed:', err);
  process.exit(1);
});
