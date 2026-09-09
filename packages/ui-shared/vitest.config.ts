import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// Vitest config for @nexus-gateway/ui-shared. Mirrors the relevant
// bits of control-plane-ui's vite.config.ts test block (jsdom env,
// non-scoped CSS modules so class assertions work in tests, global
// jest-dom matchers via the setup file) — but without the MSW + auth
// scaffolding that CP UI's setup adds.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    css: { modules: { classNameStrategy: 'non-scoped' } },
    setupFiles: ['./src/test/setup.ts'],
    // Coverage gate — see docs/developers/workflow/coverage-allowlist-methodology.md
    // (frontend section). Target core 100% / overall 95% (same as Go). This
    // package is fully backfilled: theme engine (themeLoader/chartColors/
    // completeness/ThemeContext), the locator resolver and media predicates,
    // shared components, and barrels all covered.
    //
    // The flat 100% floor stopped being met when the media work landed
    // MediaCard and resolveLocator, and stayed unmet: this gate runs only in
    // check:all, not in pre-commit, so it went red on main and on develop
    // without blocking anything. A floor nothing can pass is a floor nobody
    // reads. Everything reachable has since been backfilled — the resolver's
    // body/json/sse arms and every not-found path, the media predicates, the
    // no-DOM base-prefix guard, the size formatter and the video preview arm —
    // which is what brought the package back to 100% lines and functions.
    //
    // src/components carries its own floor for what is left: two defensive
    // branches in MediaCard that the render makes unreachable. `load` narrows
    // an optional `resolve` that `canFetch` has already required, and `save`
    // reads a cached objectUrl in a path only the button can enter — and the
    // button is replaced by an anchor the moment that url exists. Neither can
    // be reached without changing the component, and changing a live surface
    // to move a coverage number is the padding this policy forbids. Raise
    // these if MediaCard is simplified; never lower them.
    coverage: {
      provider: 'v8',
      reporter: ['text-summary', 'json-summary'],
      include: ['src/**'],
      // *.json: i18n resource bundles — no executable statements; vitest 4's
      // v8 provider counts imported JSON modules, which only adds 0% noise.
      exclude: ['src/test/**', 'src/**/*.d.ts', 'src/**/*.stories.{ts,tsx}', '**/*.test.{ts,tsx}', 'src/**/*.json'],
      thresholds: {
        // Global, at what is reachable. A glob threshold in vitest is checked
        // IN ADDITION to the global — matching files stay in the global
        // denominator — so the two unreachable branches show up here too and
        // a flat 100 cannot be met while they exist. Measured, not assumed:
        // the first attempt to carve them out with a glob left this exact
        // pair of errors standing.
        statements: 99,
        branches: 98,
        functions: 100,
        lines: 100,
        // Every non-presentational directory is AT 100 and pinned there, so
        // the softer global above cannot become room for business logic to
        // regress into. These are the floors that matter.
        'src/lib/**': { statements: 100, branches: 100, functions: 100, lines: 100 },
        'src/theme/**': { statements: 100, branches: 100, functions: 100, lines: 100 },
        'src/types/**': { statements: 100, branches: 100, functions: 100, lines: 100 },
        'src/shadcn/**': { statements: 100, branches: 100, functions: 100, lines: 100 },
        // The one directory with slack, and it is bounded and named.
        'src/components/**': { statements: 97, branches: 95, functions: 100, lines: 100 },
      },
    },
  },
});
