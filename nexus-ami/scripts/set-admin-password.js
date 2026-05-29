// set-admin-password.js — generate a scrypt password hash compatible with the
// NexusUser.passwordHash column. Reads NEW_PASSWORD from env, prints the hash
// to stdout in "salt_hex:hash_hex" format.
//
// Parameters MUST match tools/db-migrate/seed/lib.ts hashPassword():
//   N = 16384, r = 8, p = 1
//   salt = 32 bytes (random)
//   key  = 64 bytes
//
// Used by first-boot-db.sh to replace the seeded admin@nexus.ai password
// with a per-instance random one. This file is shipped to /opt/nexus/prisma/
// alongside the schema and seed code.

'use strict';

const { scryptSync, randomBytes } = require('crypto');

const SALT_LENGTH = 32;
const KEY_LENGTH = 64;
const SCRYPT_OPTIONS = { N: 16384, r: 8, p: 1 };

const password = process.env.NEW_PASSWORD;
if (!password || password.length < 8) {
  process.stderr.write('set-admin-password: NEW_PASSWORD env must be set and >= 8 chars\n');
  process.exit(1);
}

const salt = randomBytes(SALT_LENGTH);
const hash = scryptSync(password, salt, KEY_LENGTH, SCRYPT_OPTIONS);
process.stdout.write(`${salt.toString('hex')}:${hash.toString('hex')}`);
