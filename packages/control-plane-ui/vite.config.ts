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
  },
  server: {
    host: '0.0.0.0',
    port: 3000,
    https: httpsConfig,
    // Vite rejects requests whose Host header is not in this list. Add the
    // dev domains we expect to reach this server with; localhost / 127.0.0.1
    // remain so contributors can also use plain http://localhost:3000 when
    // certs are not provisioned.
    allowedHosts: ['localhost', '127.0.0.1', 'console.dev.taoecho.io'],
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
