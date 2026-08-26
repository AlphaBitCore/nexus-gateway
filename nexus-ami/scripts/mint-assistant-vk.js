// mint-assistant-vk.js — generate a per-instance random secret for the seeded
// system-assistant Virtual Key (the key Chat-with-Nexus authenticates with),
// unless ASSISTANT_VK_PLAINTEXT supplies one to hash instead.
//
// Prints one tab-separated line to stdout: "<plaintext>\t<keyHash>\t<keyPrefix>".
// first-boot-db.sh UPDATEs the assistant VK row with keyHash/keyPrefix and writes
// the plaintext into control-plane.env as NEXUS_ASSISTANT_SYSTEM_VK.
//
// Why this exists: the seed ships the assistant VK with a deterministic local
// plaintext (tools/db-migrate/seed/bootstrap/index.ts assistantVkKey) so dev
// machines work out of the box. That plaintext is public in the OSS repo and
// MUST NOT be usable on an internet-facing appliance, so first boot replaces it
// with this random value — the same per-instance reset the admin password gets.
//
// The hash MUST match the AI Gateway / Control Plane verifier: an HKDF-SHA256
// sub-key derived from ADMIN_KEY_HMAC_SECRET under the virtual-key trust-domain
// class, then HMAC-SHA256 of the key under that sub-key (mirrors
// tools/db-migrate/seed/lib.ts hashVirtualKey + packages/shared/core/keyderive).

'use strict';

const { randomBytes, createHmac, hkdfSync } = require('crypto');

// MUST byte-match packages/shared/core/keyderive ClassAPIKeyVirtualKey.
const CLASS_API_KEY_VIRTUAL_KEY = 'nexus/apikey/virtual-key/v1';

const secret = process.env.ADMIN_KEY_HMAC_SECRET;
if (!secret || !secret.trim()) {
  process.stderr.write(
    'mint-assistant-vk: ADMIN_KEY_HMAC_SECRET env must be set (must match the running services).\n',
  );
  process.exit(1);
}

// Derive the 32-byte per-domain sub-key exactly as keyderive.DeriveKey32 does:
// HKDF-SHA256 over the RAW secret-string bytes, empty salt, class as info.
function deriveSubkey(classInfo) {
  const ikm = Buffer.from(secret.trim(), 'utf8');
  return Buffer.from(hkdfSync('sha256', ikm, Buffer.alloc(0), Buffer.from(classInfo, 'utf8'), 32));
}

// "nvk_" prefix is what vkauth actually gates on (any nvk_-prefixed token
// passes regardless of length); the 48 hex chars are this script's own
// stricter shape for a credential we mint, not a vkauth requirement.
//
// Container deployments generate the plaintext before the stack starts, because
// Compose reads .env before any container runs: the control plane already holds
// NEXUS_ASSISTANT_SYSTEM_VK by the time the migrator executes, so the migrator's
// job is to make the stored hash agree with a value it did not choose. The
// appliance path supplies nothing and keeps generating its own.
const supplied = process.env.ASSISTANT_VK_PLAINTEXT;
if (supplied !== undefined && !/^nvk_[0-9a-f]{40,}$/.test(supplied)) {
  process.stderr.write(
    'mint-assistant-vk: ASSISTANT_VK_PLAINTEXT must look like nvk_<40+ hex chars>. ' +
      'vkauth itself accepts any "nvk_"-prefixed token regardless of length ' +
      '(vkauth.go looksLikeRealKey: prefix OR length > 20); this stricter shape ' +
      'is our own floor on a credential we mint, not something vkauth requires.\n',
  );
  process.exit(1);
}
const plaintext = supplied ?? `nvk_${randomBytes(24).toString('hex')}`;
const keyHash = createHmac('sha256', deriveSubkey(CLASS_API_KEY_VIRTUAL_KEY))
  .update(plaintext)
  .digest('hex');
const keyPrefix = plaintext.slice(0, 12);

process.stdout.write(`${plaintext}\t${keyHash}\t${keyPrefix}`);
