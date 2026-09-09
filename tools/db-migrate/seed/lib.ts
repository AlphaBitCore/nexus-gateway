import { scryptSync, randomBytes, createHmac, createCipheriv, hkdfSync } from 'crypto';

const SALT_LENGTH = 32;
const KEY_LENGTH = 64;
// N=2^17 per OWASP Password Storage guidance. MUST stay in lockstep with the
// Go verifier (packages/control-plane/internal/identity/authn/password.go
// scryptN): the stored "salt:hash" format omits N, so a verifier using a
// different N can never match. maxmem is raised because Node's default 32 MiB
// cap is below the ~128 MiB (128*N*r) working set scrypt needs at N=2^17.
const SCRYPT_OPTIONS = { N: 1 << 17, r: 8, p: 1, maxmem: 256 * 1024 * 1024 };

export function hashPassword(password: string): string {
  const salt = randomBytes(SALT_LENGTH);
  const hash = scryptSync(password, salt, KEY_LENGTH, SCRYPT_OPTIONS);
  return `${salt.toString('hex')}:${hash.toString('hex')}`;
}

// Trust-domain class strings — MUST byte-match packages/shared/core/keyderive
// (ClassAPIKeyVirtualKey / ClassAPIKeyAdmin). The services derive a per-domain
// HKDF-SHA256 sub-key from the raw ADMIN_KEY_HMAC_SECRET, then HMAC the key under
// that sub-key (vkauth.go NewAuthenticator / the CP admin-key authenticator).
// A plain HMAC of the raw secret would never match — domain separation is the point.
const CLASS_API_KEY_VIRTUAL_KEY = 'nexus/apikey/virtual-key/v1';
const CLASS_API_KEY_ADMIN = 'nexus/apikey/admin/v1';

function adminHmacSecret(): string {
  // No committed-constant fallback. The seed must hash keys under the
  // SAME ADMIN_KEY_HMAC_SECRET the running services use, or seeded admin keys /
  // VKs would never verify. dev-start.sh propagates a per-developer value into
  // tools/db-migrate/.env before seeding; fail loud if it is somehow absent.
  const secret = process.env.ADMIN_KEY_HMAC_SECRET?.trim();
  if (!secret) {
    throw new Error(
      'ADMIN_KEY_HMAC_SECRET is required to seed API key / virtual key hashes ' +
        '(it must match the value used by the Control Plane and AI Gateway).',
    );
  }
  return secret;
}

// Derive the 32-byte per-domain sub-key exactly as keyderive.DeriveKey32 does:
// HKDF-SHA256 over the RAW secret-string bytes (hmackeyring.Single uses
// []byte(secret), no hex-decode), empty salt, the class as info.
function deriveSubkey(classInfo: string): Buffer {
  const ikm = Buffer.from(adminHmacSecret(), 'utf8');
  return Buffer.from(hkdfSync('sha256', ikm, Buffer.alloc(0), Buffer.from(classInfo, 'utf8'), 32));
}

/** HMAC-SHA256 of a virtual key under the virtual-key trust-domain sub-key (hex). */
export function hashVirtualKey(key: string): string {
  return createHmac('sha256', deriveSubkey(CLASS_API_KEY_VIRTUAL_KEY)).update(key).digest('hex');
}

/** HMAC-SHA256 of an admin API key under the admin trust-domain sub-key (hex). */
export function hashAdminKey(key: string): string {
  return createHmac('sha256', deriveSubkey(CLASS_API_KEY_ADMIN)).update(key).digest('hex');
}

export function fakeEncrypt(plaintext: string): { ciphertext: string; iv: string; tag: string } {
  const keyHex = process.env.CREDENTIAL_ENCRYPTION_KEY;
  if (!keyHex || keyHex.length !== 64) {
    throw new Error(
      'seed: CREDENTIAL_ENCRYPTION_KEY must be a 64-char hex string (AES-256 key). Set it in tools/db-migrate/.env.',
    );
  }
  const masterKey = Buffer.from(keyHex, 'hex');
  const iv = randomBytes(12);
  const cipher = createCipheriv('aes-256-gcm', masterKey, iv, { authTagLength: 16 });
  const encrypted = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()]);
  const tag = cipher.getAuthTag();
  return { ciphertext: encrypted.toString('hex'), iv: iv.toString('hex'), tag: tag.toString('hex') };
}
