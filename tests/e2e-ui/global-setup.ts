import { chromium, type FullConfig } from '@playwright/test';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';
import * as dotenv from 'dotenv';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
// Match playwright.config.ts: load tests/.env.<target>, default to local.
const _target = process.env.NEXUS_TEST_TARGET ?? 'local';
dotenv.config({ path: path.resolve(__dirname, '..', `.env.${_target}`) });

export default async function globalSetup(_config: FullConfig) {
  const authDir = path.resolve(__dirname, '.auth');
  fs.mkdirSync(authDir, { recursive: true });
  const storagePath = path.join(authDir, 'admin.json');

  const baseURL = process.env.NEXUS_UI_URL ?? 'http://localhost:3000';
  const email = process.env.NEXUS_ADMIN_EMAIL ?? 'admin@nexus.ai';
  const password = process.env.NEXUS_ADMIN_PASSWORD ?? 'admin123';

  // The workstation HTTP_PROXY env var (127.0.0.1:10080) intercepts localhost
  // requests. Clear it before launching Chromium so direct connections work.
  delete process.env.HTTP_PROXY;
  delete process.env.HTTPS_PROXY;
  delete process.env.http_proxy;
  delete process.env.https_proxy;

  // Bypass the workstation HTTP_PROXY (127.0.0.1:10080) and ignore the
  // self-signed Vite dev cert.
  const browser = await chromium.launch({ args: ['--no-proxy-server'] });
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await ctx.newPage();

  // Navigate to /login. The SPA detects no authctx and fires the PKCE flow,
  // which redirects through /oauth/authorize back to /login?authctx=<id>.
  // Then the idp list loads and the local-credential form renders.
  await page.goto(baseURL + '/login');

  // Wait for the PKCE redirect cycle to complete and the form to appear.
  await page.getByTestId('login-email').waitFor({ state: 'visible', timeout: 20_000 });

  await page.getByTestId('login-email').fill(email);
  await page.getByTestId('login-password').fill(password);
  await page.getByTestId('login-submit').click();

  // After submit the authserver 302s to /auth/callback, which exchanges the
  // code for tokens, then redirects to the post-auth URL (/).
  // Wait for the full cycle: URL must reach / (not /login and not /auth/*).
  await page.waitForURL(
    (url) => !url.pathname.startsWith('/login') && !url.pathname.startsWith('/auth'),
    { timeout: 30_000 },
  );
  // Wait until the authenticated shell is mounted (data-testid in DOM).
  await page.waitForSelector('[data-testid="shell-nav"]', { state: 'attached', timeout: 15_000 });

  await ctx.storageState({ path: storagePath });
  await browser.close();
}
