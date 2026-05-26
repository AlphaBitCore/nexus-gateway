/**
 * MSW (Mock Service Worker) test server.
 *
 * Intercepts HTTP requests in tests so components can be tested
 * against realistic API responses without a running backend.
 *
 * Usage in test files:
 *   import { server } from '@/test/msw-server';
 *   import { http, HttpResponse } from 'msw';
 *
 *   // Override a handler for one test:
 *   server.use(
 *     http.get('/api/admin/providers', () => HttpResponse.json({ data: [] }))
 *   );
 */
import { setupServer } from 'msw/node';
import { defaultHandlers } from './msw-handlers';

export const server = setupServer(...defaultHandlers);
