/**
 * Vitest global setup — starts MSW server before all tests and seeds a fake
 * access token so the AuthProvider boot path treats the session as
 * authenticated and calls GET /api/admin/me.
 *
 * Tests that need to exercise the unauthenticated path should clear
 * sessionStorage in a `beforeEach`.
 *
 * Referenced from vite.config.ts → test.setupFiles.
 */
import { afterAll, afterEach, beforeAll, beforeEach, expect } from 'vitest';
// Vitest 4 changed setup-file expect-instance scoping; the side-effect
// `import '@testing-library/jest-dom/vitest'` no longer reliably
// registers matchers across all test files. Explicit expect.extend
// works under v4.
import * as jestDomMatchers from '@testing-library/jest-dom/matchers';
expect.extend(jestDomMatchers);

import { server } from './msw-server';

// jsdom polyfills for Radix UI primitives that rely on browser APIs
// jsdom doesn't ship. Without these the @radix-ui/react-* components
// (Select, Popover, Tabs, Tooltip, Dialog, …) throw on first render
// inside any test that mounts a page using them.
if (typeof globalThis.ResizeObserver === 'undefined') {
  class ResizeObserverPolyfill {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
   
  (globalThis as any).ResizeObserver = ResizeObserverPolyfill;
}
if (typeof Element !== 'undefined' && !Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
   
  Element.prototype.setPointerCapture = (() => {}) as any;
   
  Element.prototype.releasePointerCapture = (() => {}) as any;
}
if (typeof Element !== 'undefined' && !Element.prototype.scrollIntoView) {
   
  Element.prototype.scrollIntoView = (() => {}) as any;
}
// jsdom does not implement window.matchMedia — ThemeProvider needs it to
// read the prefers-color-scheme system preference.
if (typeof window !== 'undefined' && !window.matchMedia) {
   
  (window as any).matchMedia = (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  });
}

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
beforeEach(() => {
  // Seed a fake Bearer access token + refresh token so AuthProvider.bootstrap
  // takes the authenticated path by default. Individual auth-boundary tests
  // (LoginPage, pkceFlow, tokenStore, etc.) clear sessionStorage themselves.
  sessionStorage.setItem('nexus_access_token', 'test-access-token');
  sessionStorage.setItem('nexus_refresh_token', 'test-refresh-token');
});
afterEach(() => {
  server.resetHandlers();
  sessionStorage.clear();
});
afterAll(() => server.close());
