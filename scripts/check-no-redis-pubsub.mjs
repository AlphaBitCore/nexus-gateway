#!/usr/bin/env node
/**
 * Lint: forbid Redis pub/sub usage anywhere in Go production code.
 *
 * Binding rule (CLAUDE.md "Redis cache only" + .cursor/rules/redis-cache-only.mdc):
 * Redis is for caching only. Cross-service event notification / config
 * invalidation goes via Hub WebSocket change-signal, NOT Redis pub/sub.
 *
 * Forbidden patterns:
 *   - .Subscribe(   (excluding test code)
 *   - .PSubscribe(
 *   - .Publish(
 *   - "nexus:config" channel literal
 *
 * Test files and existing legacy fixtures are excluded.
 */

import { execSync } from 'node:child_process';

const FORBIDDEN_PATTERNS = [
  // redis client receivers calling Publish/Subscribe.
  /\.Subscribe\s*\(/,
  /\.PSubscribe\s*\(/,
  /\.Publish\s*\(/,
];

const CHANNEL_LITERAL = /["'`]nexus:config[^"'`]*["'`]/;

// Reverse-grep for deleted packages from the pre-Hub pub/sub era. Any
// re-appearance here means somebody is re-introducing the obsolete
// architecture (CP `internal/pubsub`, `internal_registry`, or the
// `shared/heartbeat` package — all replaced by Hub WebSocket + Thing
// Registry). Detected via Go import path; this is unambiguous.
const DELETED_PACKAGE_IMPORTS = [
  /["'`]github\.com\/[^/]+\/[^/]+\/packages\/shared\/heartbeat["'`]/,
  /["'`]github\.com\/[^/]+\/[^/]+\/packages\/control-plane\/internal\/pubsub["'`]/,
  /["'`]github\.com\/[^/]+\/[^/]+\/packages\/control-plane\/internal_registry["'`]/,
];

const TEST_PATH_RE = /_test\.go$|test_helpers?\.go$|\/testharness\//;
const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';

function gitTrackedGoFiles() {
  try {
    const out = execSync('git ls-files "*.go" "packages/**/*.go"', { encoding: 'utf-8' });
    return out
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean);
  } catch {
    return [];
  }
}

function main() {
  const files = gitTrackedGoFiles();
  const hits = [];

  for (const f of files) {
    if (TEST_PATH_RE.test(f)) continue;
    let text;
    try {
      text = execSync(`git show :${f}`, { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] });
    } catch {
      continue;
    }
    const lines = text.split('\n');
    const fastText = /redis|nexus:config|shared\/heartbeat|internal\/pubsub|internal_registry/i.test(text);
    if (!fastText) continue;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      // Skip comments.
      const stripped = line.replace(/^\s*\/\/.*$/, '');
      if (!stripped) continue;

      // Reverse-grep for deleted package imports — unambiguous architectural
      // regression. See cursor rule: redis-cache-only.mdc "Deletion artefact".
      const deletedHit = DELETED_PACKAGE_IMPORTS.some((re) => re.test(line));
      if (deletedHit) {
        hits.push({ file: f, line: i + 1, kind: 'deleted-pkg-reimport', text: line.trim().slice(0, 120) });
        continue;
      }

      // Distinguish redis-receiver vs other libs by requiring a redis-ish neighbour.
      // (Heuristic — most false-positives come from non-Redis Pub/Subscribe APIs like
      //  NATS or in-memory event buses.)
      const hasRedisContext = /redis|cache:/i.test(line) || /redis/i.test(lines[Math.max(0, i - 5)] || '');
      const channelHit = CHANNEL_LITERAL.test(line);
      const apiHit = FORBIDDEN_PATTERNS.some((re) => re.test(line));

      if (channelHit) {
        hits.push({ file: f, line: i + 1, kind: 'channel-literal', text: line.trim().slice(0, 120) });
      } else if (apiHit && hasRedisContext) {
        hits.push({ file: f, line: i + 1, kind: 'pubsub-call', text: line.trim().slice(0, 120) });
      }
    }
  }

  if (hits.length > 0) {
    const label = STRICT ? 'FAILED' : 'WARN';
    const ws = STRICT ? console.error : console.warn;
    ws(`[check:no-redis-pubsub] ${label} -- ${hits.length} potential violation(s):`);
    for (const h of hits) {
      ws(`  - ${h.file}:${h.line}  [${h.kind}]  ${h.text}`);
    }
    ws('');
    ws('Redis is cache-only. Cross-service invalidation goes via Hub WebSocket.');
    ws('See .cursor/rules/redis-cache-only.mdc');
    if (STRICT) process.exit(1);
    ws('[check:no-redis-pubsub] non-strict mode; passing despite warnings. Run with --strict to fail.');
    return;
  }

  console.log('[check:no-redis-pubsub] OK -- no Redis pub/sub usage detected.');
}

main();
