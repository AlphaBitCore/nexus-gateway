#!/usr/bin/env node
/**
 * Lint: forbid hardcoded filesystem paths in packages/agent/internal/.
 *
 * Binding rule (agent-runtime-invariants.mdc Rule 1): all agent paths must
 * come from platform.DefaultPaths(). Hardcoded paths like "/Library/...",
 * "/var/...", "/etc/...", "/tmp/...", "C:\\..." are forbidden because they
 * break cross-platform builds (the 2026-05-13 QuitFlag incident).
 *
 * Heuristic regex scan over packages/agent/internal/**\/*.go (excluding test
 * files + platform-specific dirs which legitimately encode platform paths).
 */

import { execSync } from 'node:child_process';

const FORBIDDEN_PATTERNS = [
  /"\/Library\//,
  /"\/var\//,
  /"\/etc\//,
  /"\/tmp\//,
  /"\/usr\//,
  /"C:\\\\/,
  /"C:\//,
];

// Platform-specific path files (linux.go, darwin.go, windows.go) are
// EXEMPT — they legitimately encode platform paths.
const PLATFORM_FILES_RE = /\/platform\/(linux|darwin|windows)\.go$|_(linux|darwin|windows)\.go$/;
// Test files are also exempt.
const TEST_FILES_RE = /_test\.go$/;

const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';

function gitTrackedAgentGoFiles() {
  try {
    const out = execSync('git ls-files "packages/agent/internal/*.go" "packages/agent/internal/**/*.go"', { encoding: 'utf-8' });
    return out
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean)
      .filter((p) => !PLATFORM_FILES_RE.test(p) && !TEST_FILES_RE.test(p));
  } catch {
    return [];
  }
}

function main() {
  const files = gitTrackedAgentGoFiles();
  const hits = [];

  for (const f of files) {
    let text;
    try {
      text = execSync(`git show :${f}`, { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] });
    } catch {
      continue;
    }
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      // Skip pure-comment lines.
      if (/^\s*\/\//.test(line)) continue;
      for (const re of FORBIDDEN_PATTERNS) {
        if (re.test(line)) {
          hits.push({ file: f, line: i + 1, text: line.trim().slice(0, 120) });
          break;
        }
      }
    }
  }

  if (hits.length > 0) {
    const label = STRICT ? 'FAILED' : 'WARN';
    const ws = STRICT ? console.error : console.warn;
    ws(`[check:hardcoded-agent-paths] ${label} -- ${hits.length} hardcoded path(s) in packages/agent/internal/:`);
    for (const h of hits) {
      ws(`  - ${h.file}:${h.line}  ${h.text}`);
    }
    ws('');
    ws('All agent paths must come from platform.DefaultPaths(). See:');
    ws('  - .cursor/rules/agent-runtime-invariants.mdc Rule 1');
    ws('  - docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md');
    ws('Incident scar: 2026-05-13 QuitFlag hardcoded macOS path.');
    if (STRICT) process.exit(1);
    ws('[check:hardcoded-agent-paths] non-strict mode; passing despite warnings. Run with --strict to fail.');
    return;
  }

  console.log('[check:hardcoded-agent-paths] OK -- no hardcoded paths detected.');
}

main();
