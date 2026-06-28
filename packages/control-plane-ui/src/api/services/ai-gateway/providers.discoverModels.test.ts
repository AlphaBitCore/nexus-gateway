/**
 * Unit tests for providerApi.discoverModels.
 *
 * Verifies:
 *  - POSTs to the correct path with the correct body
 *  - Returns parsed success result ({success:true, models:[...]})
 *  - Returns parsed failure result ({success:false, error, code})
 */

import { describe, it, expect, afterEach, afterAll, beforeAll, beforeEach } from 'vitest';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import { providerApi } from './providers';
import { setTokens, clearTokens } from '@/auth/tokens/tokenStore';

// Minimal non-empty access token so the Authorization header is set
// (tokenStore requires both access + refresh; clearTokens between tests).
const FAKE_TOKEN = 'eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEifQ.sig';

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  clearTokens();
});
afterAll(() => server.close());

beforeEach(() => {
  setTokens({ accessToken: FAKE_TOKEN, refreshToken: 'rt' });
});

const DISCOVER_PATH = '/api/admin/providers/discover-models';

describe('providerApi.discoverModels', () => {
  it('POSTs to /api/admin/providers/discover-models with the supplied body', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(DISCOVER_PATH, async ({ request }) => {
        // Clone before reading so MSW can consume the original for internal logging.
        capturedBody = await request.clone().json();
        return HttpResponse.json({
          success: true,
          models: [{ id: 'gpt-4o', suggestedType: 'chat' }],
        });
      }),
    );

    await providerApi.discoverModels({
      adapterType: 'openai',
      baseUrl: 'https://api.openai.com',
      apiKey: 'sk-test',
    });

    expect(capturedBody).toEqual({
      adapterType: 'openai',
      baseUrl: 'https://api.openai.com',
      apiKey: 'sk-test',
    });
  });

  it('returns success:true with models array when backend reports success', async () => {
    server.use(
      http.post(DISCOVER_PATH, () =>
        HttpResponse.json({
          success: true,
          models: [
            { id: 'gpt-4o', suggestedType: 'chat' },
            { id: 'text-embedding-3-small', suggestedType: 'embedding' },
          ],
        }),
      ),
    );

    const result = await providerApi.discoverModels({
      adapterType: 'openai',
      baseUrl: 'https://api.openai.com',
      apiKey: 'sk-test',
    });

    expect(result.success).toBe(true);
    if (result.success) {
      expect(result.models).toHaveLength(2);
      expect(result.models[0]).toEqual({ id: 'gpt-4o', suggestedType: 'chat' });
      expect(result.models[1]).toEqual({ id: 'text-embedding-3-small', suggestedType: 'embedding' });
    }
  });

  it('returns success:false with error and code when adapter is not supported', async () => {
    server.use(
      http.post(DISCOVER_PATH, () =>
        HttpResponse.json({
          success: false,
          error: 'adapter does not support model discovery',
          code: 'discovery_unsupported',
        }),
      ),
    );

    const result = await providerApi.discoverModels({
      adapterType: 'anthropic',
      baseUrl: 'https://api.anthropic.com',
      apiKey: 'sk-ant',
    });

    expect(result.success).toBe(false);
    if (!result.success) {
      expect(result.error).toBe('adapter does not support model discovery');
      expect(result.code).toBe('discovery_unsupported');
    }
  });

  it('returns success:false with error when backend reports a generic failure', async () => {
    server.use(
      http.post(DISCOVER_PATH, () =>
        HttpResponse.json({
          success: false,
          error: 'connection refused',
        }),
      ),
    );

    const result = await providerApi.discoverModels({
      adapterType: 'openai',
      baseUrl: 'https://broken.example.com',
      apiKey: 'sk-bad',
    });

    expect(result.success).toBe(false);
    if (!result.success) {
      expect(result.error).toBe('connection refused');
      expect(result.code).toBeUndefined();
    }
  });
});
