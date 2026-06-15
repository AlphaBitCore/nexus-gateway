/// <reference types="vitest" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// Wails serves the built assets out of ./dist via its embed.FS. We
// compile to a `relative` base so the assets resolve under the
// Wails asset-server's synthetic URL space.
export default defineConfig({
  plugins: [react()],
  base: './',
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    // Used by `wails dev` when proxying the Vite dev server into the
    // Wails window. Port is auto-picked by Wails per wails.json.
    strictPort: false,
  },
  test: {
    environment: 'jsdom',
    globals: false,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
    // The frontend uses the @nexus-gateway/ui-shared workspace package for
    // shared i18n bundles. Vitest needs to resolve those at runtime; the
    // root npm workspace symlink + vite's default resolver handles this,
    // but make it explicit so the test runner doesn't fall back to node
    // module resolution that doesn't know about workspace:* entries.
    deps: { optimizer: { web: { include: ['@nexus-gateway/ui-shared'] } } },
    // Coverage gate — see docs/developers/workflow/coverage-allowlist-methodology.md
    // (frontend section). Target core 100% / overall 95% (same as Go). The Agent
    // dashboard is the least-tested UI surface; the floors below are a
    // regression-guard ratchet — the bulk of the backfill (pages/panels)
    // is the documented burn-down. Raise these as it lands, never lower
    // (re-pinning under a coverage-instrument change is the one exception —
    // see the methodology doc).
    coverage: {
      provider: 'v8',
      reporter: ['text-summary', 'json-summary'],
      include: ['src/**'],
      exclude: ['src/main.tsx', 'src/test/**', 'src/**/*.d.ts', 'src/vite-env.d.ts', '**/*.test.{ts,tsx}', 'src/**/*.json'],
      thresholds: {
        // Tests live in tests/ (mirrored), so the denominator is source-only.
        // Floors are pinned at the honest baseline as measured by the
        // current Vitest major's V8 remapping; a Vitest major upgrade
        // re-measures the same code to different numbers and re-pins these
        // in the same PR (see the methodology doc's instrument-change rule).
        statements: 65,
        branches: 51,
        functions: 68,
        lines: 66,
      },
    },
  },
});
