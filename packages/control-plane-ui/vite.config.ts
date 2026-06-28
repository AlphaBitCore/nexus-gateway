import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import fs from 'node:fs';
import path from 'node:path';

// Dev TLS: load PEM cert + key from .certs/ when present so we serve over
// HTTPS. Required for any non-loopback origin (LAN IP, custom hostname),
// because browsers gate Web Crypto and Secure cookies behind a secure
// context. A fresh checkout without certs still falls back to HTTP on
// localhost, so contributors who only need localhost dev are unaffected.
const certDir = path.resolve(__dirname, '.certs');
const certFile = path.join(certDir, 'cert.pem');
const keyFile = path.join(certDir, 'key.pem');
const httpsConfig =
  fs.existsSync(certFile) && fs.existsSync(keyFile)
    ? { cert: fs.readFileSync(certFile), key: fs.readFileSync(keyFile) }
    : undefined;

export default defineConfig({
  plugins: [react()],
  // Set base to './' so the built index.html emits ./assets/… relative paths.
  // nginx injects <base href="/nexus/"> at serve time for sub-path deployments;
  // the relative assets resolve correctly regardless of the mount prefix.
  base: './',
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    css: { modules: { classNameStrategy: 'non-scoped' } },
    setupFiles: ['./src/test/setup.ts'],
    exclude: ['e2e/**', 'node_modules/**'],
    // Frontend coverage gate — the Vitest parallel of scripts/check-go-coverage.sh.
    // Target (same policy as Go): core business logic 100%, overall 95%. The SPA
    // is mid-backfill, so the enforced thresholds below are a regression-guard
    // RATCHET at the current baseline (raise them as src/api, src/pages, and
    // src/components are backfilled — never lower). Burn-down + methodology:
    // docs/developers/workflow/coverage-allowlist-methodology.md (frontend section).
    coverage: {
      provider: 'v8',
      reporter: ['text-summary', 'json-summary'],
      include: ['src/**'],
      // The allowlist: genuinely un-coverable-in-unit-scope surfaces.
      exclude: [
        'src/main.tsx', // app bootstrap (ReactDOM.createRoot on a real DOM)
        'src/**/*.stories.{ts,tsx}', // Storybook stories, not unit logic
        'src/test/**', // test harness / MSW handlers / fixtures
        'src/**/*.d.ts',
        'src/vite-env.d.ts',
        '**/*.test.{ts,tsx}', // tests live in tests/ (mirrored); never count them as source
        'src/i18n/locales/**', // translation data (not executable logic); parity is enforced by the i18n parity guard, not unit tests
        'src/routes/lazyPages.tsx', // code-splitting wiring only (111 `lazy(() => import(...))` exports, zero branching); the real pages are tested directly — asserting "X is a lazy component" is coverage padding
        // Presentational chart renderers — recharts <ResponsiveContainer> renders
        // a 0×0 SVG in jsdom (no layout), and LatencyMini draws via runtime DOM
        // measurement (useLayoutEffect + getBoundingClientRect); their bodies
        // cannot execute under unit scope. Both are pure renderers with no
        // exported business helper (cf. TimeSeriesChart.formatValue, which stays
        // counted + is unit-tested). Asserting SVG geometry would be padding.
        'src/components/charts/LatencyMini.tsx',
        'src/components/charts/LatencyWaterfall.tsx',
        // Pure icon-glyph map (icon-name → SVG path), extracted out of Sidebar.tsx
        // so the nav/permission logic stays counted + tested. Presentational data
        // like the locale JSON — asserting glyph SVG paths is test-for-test padding.
        'src/components/ui/Sidebar/Sidebar.icons.tsx',
      ],
      thresholds: {
        // Global ratchet floor at the honest all-files baseline (32.8% stmts /
        // 27.4 br / 25.2 fn / 34.9 ln over 19k statements). The presentational
        // bulk (src/pages 27.7%, src/components 37.5%) is the burn-down toward
        // the 95% target — raise these as it lands, never lower.
        statements: 68,
        branches: 56,
        functions: 58,
        lines: 71,
        // Core business-logic dirs — hold the higher line they already meet
        // (statements-only floors; burn-down target 100%).
        'src/hooks/**': { statements: 95 },
        'src/auth/**': { statements: 90 }, // ratcheted: guards + tokenStore + AuthContext flows
        'src/lib/**': { statements: 95 }, // core dir at target after lib backfill
        'src/api/**': { statements: 92 }, // full api/services contract suite (iam/rulepacks/system/compliance/alerts/providers)
        'src/context/**': { statements: 95 }, // TimeRange + Toast contexts backfilled
        'src/components/**': { statements: 76 }, // ratcheted as component smokes + behavior tests land
        'src/pages/**': { statements: 64 }, // burn-down surface — raise as page tests land
      },
    },
  },
  server: {
    host: '0.0.0.0',
    port: 3000,
    https: httpsConfig,
    // Vite rejects requests whose Host header is not in this list. Add the
    // dev domains we expect to reach this server with; localhost / 127.0.0.1
    // remain so contributors can also use plain http://localhost:3000 when
    // certs are not provisioned.
    allowedHosts: ['localhost', '127.0.0.1', 'console.dev.nexus.ai'],
    proxy: {
      '/api': {
        target: process.env.VITE_ADMIN_API_TARGET ?? 'http://localhost:3001',
        changeOrigin: true,
      },
      // OAuth + hosted login surfaces are mounted on the Control Plane root
      // alongside /api. In dev the SPA and the authserver run on different
      // ports (3000 vs 3001), so we proxy these through the same origin the
      // SPA reports as its redirect_uri — otherwise the browser would start
      // the PKCE flow on :3000, get 404s for /oauth/authorize, and the
      // redirect_uri that eventually round-trips would not match what the
      // seeded cp-ui client has registered.
      '/oauth': {
        target: process.env.VITE_ADMIN_API_TARGET ?? 'http://localhost:3001',
        changeOrigin: true,
      },
      '/.well-known': {
        target: process.env.VITE_ADMIN_API_TARGET ?? 'http://localhost:3001',
        changeOrigin: true,
      },
      // /authserver/* carries the SPA-facing JSON endpoints (idps + password).
      // /login is owned by the SPA and must NOT be proxied — a hard reload on
      // /login?authctx=... has to resolve to the React bundle, not the
      // backend.
      '/authserver': {
        target: process.env.VITE_ADMIN_API_TARGET ?? 'http://localhost:3001',
        changeOrigin: true,
      },
      '/idp': {
        target: process.env.VITE_ADMIN_API_TARGET ?? 'http://localhost:3001',
        changeOrigin: true,
      },
    },
  },
});
