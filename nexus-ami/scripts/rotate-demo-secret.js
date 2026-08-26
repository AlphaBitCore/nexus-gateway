// rotate-demo-secret.js — derive a per-install, secret-dependent credential
// for ONE demo-tier row and print the value(s) needed to re-stamp it.
//
// tools/db-migrate/seed/demo/index.ts ships every NexusUser / VirtualKey /
// AdminApiKey demo row with a plaintext that is deterministically computable
// from the row's fixture id ALONE (DEMO_PASSWORD = "nexus-demo",
// demoVkKey(id) = "nvk_demo_" + id.slice(0,8), demoAdminKey(id) =
// "nak_demo_" + id.slice(0,8)) — and every fixture id is public in the OSS
// repository. A quickstart that ships that seed as-is exposes a working
// super-admin API key and a dozen working virtual keys to anyone who reads
// the source.
//
// This script computes a REPLACEMENT plaintext for one row that ALSO depends
// on ADMIN_KEY_HMAC_SECRET — an install-specific secret nobody reading the
// repo has — while staying a pure function of (secret, row id, kind), so
// re-running it (e.g. on the next `docker compose up`) reproduces the SAME
// credential instead of invalidating one an operator was already given. It
// deliberately does NOT touch the database; the caller (docker/db-migrator's
// rotate-demo-secrets.mjs) is responsible for finding the rows to rotate and
// writing the UPDATE.
//
// Usage:
//   ADMIN_KEY_HMAC_SECRET=... ROTATE_KIND=password|virtual-key|admin-key \
//     ROTATE_ID=<row id> node rotate-demo-secret.js
//
// Output (one line, tab-separated, no trailing newline):
//   password:     <password>\t<saltHex:hashHex>
//   virtual-key:  <plaintext>\t<keyHash>\t<keyPrefix>
//   admin-key:    <plaintext>\t<keyHash>\t<keyPrefix>
//
// The password hash MUST match tools/db-migrate/seed/lib.ts hashPassword() /
// packages/control-plane/internal/identity/authn/password.go (scrypt,
// "saltHex:hashHex"). The key hashes MUST match tools/db-migrate/seed/lib.ts
// hashVirtualKey() / hashAdminKey() and packages/shared/core/keyderive (HKDF
// sub-key per trust-domain class, then HMAC-SHA256 of the plaintext under
// it) — see set-admin-password.js and mint-assistant-vk.js in this same
// directory for why this is a hand-kept mirror rather than an import: the
// appliance ships no TypeScript toolchain, only these standalone scripts.

'use strict';

const { scryptSync, randomBytes, createHmac, hkdfSync } = require('crypto');

const SALT_LENGTH = 32;
const KEY_LENGTH = 64;
// MUST match tools/db-migrate/seed/lib.ts SCRYPT_OPTIONS /
// packages/control-plane/internal/identity/authn/password.go scryptN.
const SCRYPT_OPTIONS = { N: 1 << 17, r: 8, p: 1, maxmem: 256 * 1024 * 1024 };

// MUST byte-match packages/shared/core/keyderive Class* constants.
const CLASS_API_KEY_VIRTUAL_KEY = 'nexus/apikey/virtual-key/v1';
const CLASS_API_KEY_ADMIN = 'nexus/apikey/admin/v1';

const secret = process.env.ADMIN_KEY_HMAC_SECRET;
if (!secret || !secret.trim()) {
  process.stderr.write(
    'rotate-demo-secret: ADMIN_KEY_HMAC_SECRET env must be set (must match the running services).\n',
  );
  process.exit(1);
}

const id = process.env.ROTATE_ID;
if (!id || !id.trim()) {
  process.stderr.write('rotate-demo-secret: ROTATE_ID env must be set to the row id being rotated.\n');
  process.exit(1);
}

const kind = process.env.ROTATE_KIND;

// Derive the 32-byte per-domain sub-key exactly as keyderive.DeriveSubkey
// does: HKDF-SHA256 over the RAW secret-string bytes, empty salt, class as
// info. This is the STORAGE hash step — computes the value the admission
// path will independently recompute to verify a presented credential.
function deriveSubkey(classInfo) {
  const ikm = Buffer.from(secret.trim(), 'utf8');
  return Buffer.from(hkdfSync('sha256', ikm, Buffer.alloc(0), Buffer.from(classInfo, 'utf8'), 32));
}

// Derive a per-row plaintext SEED: HMAC-SHA256(secret, domain|id) as hex.
// This is a DIFFERENT step from deriveSubkey above — it picks the secret
// VALUE a human/operator would use, not the hash that verifies it. Distinct
// domain strings per kind (and per this script vs. the storage-hash classes
// above) keep the two purposes from ever colliding.
function derivePlaintextSeed(domain) {
  return createHmac('sha256', secret.trim()).update(`${domain}|${id}`).digest('hex');
}

switch (kind) {
  case 'password': {
    // 24 hex chars (96 bits) — short enough to read/copy, long enough that
    // guessing it without the secret is infeasible.
    const password = derivePlaintextSeed('nexus/demo/user-password/v1').slice(0, 24);
    const salt = randomBytes(SALT_LENGTH);
    const hash = scryptSync(password, salt, KEY_LENGTH, SCRYPT_OPTIONS);
    process.stdout.write(`${password}\t${salt.toString('hex')}:${hash.toString('hex')}`);
    break;
  }
  case 'virtual-key': {
    // "nvk_" prefix is REQUIRED — vkauth rejects keys without it that are
    // also <=20 chars (see mint-assistant-vk.js for the exact rule).
    const plaintext = `nvk_${derivePlaintextSeed('nexus/demo/vk-plaintext/v1')}`;
    const keyHash = createHmac('sha256', deriveSubkey(CLASS_API_KEY_VIRTUAL_KEY))
      .update(plaintext)
      .digest('hex');
    process.stdout.write(`${plaintext}\t${keyHash}\t${plaintext.slice(0, 12)}`);
    break;
  }
  case 'admin-key': {
    // "nak_" prefix is a display convention only (matches the seed's own
    // demoAdminKey naming) — the CP admin-key admission path
    // (packages/control-plane/internal/identity/users/apikeystore) does no
    // prefix check, it looks the key up purely by keyHash.
    const plaintext = `nak_${derivePlaintextSeed('nexus/demo/admin-key-plaintext/v1')}`;
    const keyHash = createHmac('sha256', deriveSubkey(CLASS_API_KEY_ADMIN)).update(plaintext).digest('hex');
    process.stdout.write(`${plaintext}\t${keyHash}\t${plaintext.slice(0, 12)}`);
    break;
  }
  default:
    process.stderr.write(
      `rotate-demo-secret: ROTATE_KIND must be one of password|virtual-key|admin-key (got "${kind}").\n`,
    );
    process.exit(1);
}
