// Best-effort: point git at the repo's shared hooks directory (.githooks) on
// `npm install` (the `prepare` lifecycle script).
//
// This replaces the previous shell one-liner
//   git config core.hooksPath .githooks 2>/dev/null || true
// which used bash redirection + `|| true` that cmd.exe cannot parse, so
// `npm install` (and Wails builds, which run npm install) failed on Windows
// with "'true' is not recognized". Node runs identically on cmd / sh / pwsh.
//
// Failures are swallowed by design: hooks are a developer convenience, and a
// fresh checkout without git (CI artifact, tarball) must still install deps.

import { execFileSync } from 'node:child_process';

try {
  execFileSync('git', ['config', 'core.hooksPath', '.githooks'], { stdio: 'ignore' });
} catch {
  // git missing or not a git checkout — ignore; hooks aren't required to install.
}
