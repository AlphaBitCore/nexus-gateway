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
  },
});
