#!/usr/bin/env node
/**
 * Lint: forbid concrete secret values inside committed yaml config.
 *
 * Binding rule (CLAUDE.md → Mandatory rules → "Secrets are env-only"):
 * No secret field (auth tokens, HMAC keys, credential-encryption keys,
 * passphrases, internal-service tokens, runtime-API bearers, DB
 * passwords) may appear in *.dev.yaml / *.prod.yaml.example / *.config.yaml.
 * Secrets ride in env (loaded from .env in dev, systemd EnvironmentFile
 * in prod) and the per-service config.Load env-override path.
 *
 * Detection shape:
 *   - Scan every line of every tracked yaml in scope.
 *   - Identify keys whose name (case-insensitive) ends in one of:
 *       password, passwd, passphrase, secret, token, salt, apikey,
 *       hmac, bearer, encryptionkey, authkey, privatekey
 *     and whose value is non-empty, not a `${ENV_VAR}` placeholder,
 *     and not obviously a file path.
 *   - Keys ending in Path/File/Url/URL/Source/Channel/Endpoint are
 *     skipped — they're references TO a credential, not the credential
 *     itself (e.g. `keyFile: "./dev-certs/ca.key"` is a path lookup).
 *   - Comment lines (`#` prefix) are skipped.
 *
 * Scope: yaml files at every service root (*.config.yaml, *.dev.yaml,
 * *.prod.yaml*, *.example) plus repo-root yaml. Tracked by `git ls-files`
 * so untracked test fixtures are out of scope.
 *
 * Opt-out: append `# nolint:yaml-secrets` on the same line with a brief
 * justification (e.g. "harmless public default constant").
 *
 * Usage:
 *   scripts/check-no-yaml-secrets.mjs            # warn (non-strict)
 *   scripts/check-no-yaml-secrets.mjs --strict   # exit 1 on hits (CI)
 *   scripts/check-no-yaml-secrets.mjs --staged   # only staged yaml (pre-commit)
 */

import { execSync } from 'node:child_process';

// Case-insensitive — yaml convention varies (camelCase in Go services,
// snake_case in legacy configs). Match against the LAST word of the key.
const SECRET_TOKEN_TAILS = [
  'password',
  'passwd',
  'passphrase',
  'secret',
  'token',
  'salt',
  'apikey',
  'hmac',
  'hmackey',
  'hmacsecret',
  'bearer',
  'bearertoken',
  'encryptionkey',
  'authkey',
  'authtoken',
  'privatekey',
];

// Keys whose tail is one of the SECRET_TOKEN_TAILS but whose suffix is
// in this list — these reference a credential location, not a value.
const REFERENCE_SUFFIXES = ['path', 'file', 'url', 'source', 'channel', 'endpoint', 'env', 'envvar'];

// Build a single regex that matches key portion ending in a secret tail,
// optionally with a separator+value.
const KEY_RE = new RegExp(
  `^(\\s*)([a-zA-Z_][a-zA-Z0-9_-]*?(?:${SECRET_TOKEN_TAILS.join('|')}))\\s*:\\s*(.*)$`,
  'i',
);

const OPT_OUT_RE = /#\s*nolint:yaml-secrets\b/;

// Patterns that mark the VALUE as a non-secret placeholder.
const PLACEHOLDER_VALUE_RE = /^(\s*$|""|''|null|~|\$\{[^}]+\}|<[^>]+>)/;

// Patterns that mark the VALUE as a file path (path/file lookup), not the
// secret itself. Some configs name the field `keyPath` (caught by
// REFERENCE_SUFFIXES) but others use `key:` with a path value, hence this.
const FILE_PATH_VALUE_RE = /^"?(?:\.{1,2}\/|\/(?:etc|var|opt|tmp|usr|home)\/|[A-Z]:\\)|\.(?:pem|crt|key|cer|p12|pfx|json|yaml|yml|toml|conf)"?\s*(?:#|$)/i;

const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';
const STAGED = process.argv.includes('--staged');

// Paths excluded from the scan. OpenAPI specs under docs/users/api/openapi/ embed
// `example:` / `examples:` blocks that contain illustrative secret-shaped
// strings — those are documentation, not credentials. Test fixtures and
// node_modules are similarly out of scope.
const EXCLUDE_PATH_RE = /^(docs\/users\/api\/openapi\/|tests\/|.*\/node_modules\/)/;

function listYamlFiles() {
  let cmd;
  if (STAGED) {
    cmd = 'git diff --cached --name-only --diff-filter=ACM';
  } else {
    cmd = 'git ls-files "*.yaml" "*.yml" "*.yaml.example" "packages/**/*.yaml" "packages/**/*.yaml.example"';
  }
  try {
    const out = execSync(cmd, { encoding: 'utf-8' });
    return out
      .split('\n')
      .map((s) => s.trim())
      .filter((s) => /\.(ya?ml(\.example)?)$/.test(s))
      .filter((s) => !EXCLUDE_PATH_RE.test(s));
  } catch {
    return [];
  }
}

function readFile(path) {
  try {
    return execSync(`cat "${path}"`, { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] });
  } catch {
    return null;
  }
}

function isReferenceField(key) {
  const lower = key.toLowerCase();
  return REFERENCE_SUFFIXES.some((suffix) => lower.endsWith(suffix));
}

function stripInlineComment(value) {
  // YAML inline comment: `# ` (space-padded) outside of a string. The
  // exact rules are subtle; this is good enough for our regex-level scan.
  // We keep characters until we see ` #` outside a quoted segment.
  let inSingle = false;
  let inDouble = false;
  for (let i = 0; i < value.length; i++) {
    const c = value[i];
    if (c === '"' && !inSingle) inDouble = !inDouble;
    else if (c === "'" && !inDouble) inSingle = !inSingle;
    else if (c === '#' && !inSingle && !inDouble) {
      if (i === 0 || /\s/.test(value[i - 1])) return value.slice(0, i).trimEnd();
    }
  }
  return value.trimEnd();
}

function main() {
  const files = listYamlFiles();
  if (files.length === 0) {
    console.log(
      STAGED
        ? '[check:no-yaml-secrets] no staged yaml — skipping.'
        : '[check:no-yaml-secrets] no yaml files found.',
    );
    return;
  }

  const hits = [];
  for (const f of files) {
    const text = readFile(f);
    if (text === null) continue;
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const raw = lines[i];
      const stripped = raw.replace(/^\s+/, '');
      if (stripped.startsWith('#')) continue; // whole-line comment
      if (OPT_OUT_RE.test(raw)) continue;
      const m = KEY_RE.exec(raw);
      if (!m) continue;
      const [, , key, rawValue] = m;
      if (isReferenceField(key)) continue;
      const value = stripInlineComment(rawValue).trim();
      if (PLACEHOLDER_VALUE_RE.test(value)) continue;
      if (FILE_PATH_VALUE_RE.test(value)) continue;
      hits.push({ file: f, line: i + 1, key, value: value.slice(0, 80), text: raw.trim().slice(0, 140) });
    }
  }

  if (hits.length === 0) {
    console.log(`[check:no-yaml-secrets] OK -- ${files.length} yaml file(s) scanned, 0 inline secrets.`);
    return;
  }

  const label = STRICT ? 'FAILED' : 'WARN';
  const ws = STRICT ? console.error : console.warn;
  ws(`[check:no-yaml-secrets] ${label} -- ${hits.length} inline secret value(s):`);
  for (const h of hits) {
    ws(`  - ${h.file}:${h.line}  [${h.key}]  ${h.text}`);
  }
  ws('');
  ws('Secrets must live in env (.env in dev, systemd EnvironmentFile in prod), not yaml.');
  ws('Move the value to .env (and .env.example as `KEY=` placeholder) and replace the');
  ws('yaml entry with `key: ""` so the service-side env override binds it at boot.');
  ws('Binding rule: CLAUDE.md "Secrets are env-only — never yaml".');
  if (STRICT) process.exit(1);
  ws('[check:no-yaml-secrets] non-strict mode; passing despite warnings. Run with --strict to fail.');
}

main();
