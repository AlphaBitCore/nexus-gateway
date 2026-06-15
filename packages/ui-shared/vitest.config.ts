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
    // completeness/ThemeContext), shared components, and barrels all covered.
    // Floors are pinned at the achieved 100% — never lower.
    coverage: {
      provider: 'v8',
      reporter: ['text-summary', 'json-summary'],
      include: ['src/**'],
      // *.json: i18n resource bundles — no executable statements; vitest 4's
      // v8 provider counts imported JSON modules, which only adds 0% noise.
      exclude: ['src/test/**', 'src/**/*.d.ts', 'src/**/*.stories.{ts,tsx}', '**/*.test.{ts,tsx}', 'src/**/*.json'],
      thresholds: {
        statements: 100,
        branches: 100,
        functions: 100,
        lines: 100,
      },
    },
  },
});
