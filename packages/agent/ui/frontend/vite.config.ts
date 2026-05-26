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
  },
});
