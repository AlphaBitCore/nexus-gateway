import { defineConfig, devices } from '@playwright/test';
import * as dotenv from 'dotenv';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Single source of truth: tests/.env.<target> (target ∈ {local,dev,prod}).
// Defaults to "local" so a developer can `npx playwright test` against the
// local Vite dev server without setting NEXUS_TEST_TARGET. Mirrors the
// loader contract in tests/lib/loadenv.{sh,py} + the binding rule in
// CLAUDE.md "Test / skill env files".
const _target = process.env.NEXUS_TEST_TARGET ?? 'local';
dotenv.config({ path: path.resolve(__dirname, '..', `.env.${_target}`) });

// The workstation HTTP_PROXY (127.0.0.1:10080) intercepts localhost calls.
// Clear it for the test process so Chromium can reach localhost directly.
delete process.env.HTTP_PROXY;
delete process.env.HTTPS_PROXY;
delete process.env.http_proxy;
delete process.env.https_proxy;

export default defineConfig({
  testDir: './specs',
  // Sequential: auth state shared via storageState; some specs mutate fixtures.
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 2 : 0,
  timeout: 30_000,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL: process.env.NEXUS_UI_URL ?? 'http://localhost:3000',
    // Bypass workstation HTTP_PROXY (127.0.0.1:10080) and ignore the
    // self-signed Vite dev cert.
    launchOptions: { args: ['--no-proxy-server'] },
    ignoreHTTPSErrors: true,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    storageState: path.resolve(__dirname, '.auth', 'admin.json'),
  },
  globalSetup: path.resolve(__dirname, 'global-setup.ts'),
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
