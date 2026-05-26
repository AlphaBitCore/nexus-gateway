import { scryptSync, randomBytes, createHmac } from 'crypto';

const SALT_LENGTH = 32;
const KEY_LENGTH = 64;
const SCRYPT_OPTIONS = { N: 16384, r: 8, p: 1 };

export function hashPassword(password: string): string {
  const salt = randomBytes(SALT_LENGTH);
  const hash = scryptSync(password, salt, KEY_LENGTH, SCRYPT_OPTIONS);
  return `${salt.toString('hex')}:${hash.toString('hex')}`;
}

const HMAC_DEV_FALLBACK = 'nexus-gateway-default-hmac-secret';

export function hashApiKey(key: string): string {
  const secret = process.env.ADMIN_KEY_HMAC_SECRET?.trim() || HMAC_DEV_FALLBACK;
  return createHmac('sha256', secret).update(key).digest('hex');
}
