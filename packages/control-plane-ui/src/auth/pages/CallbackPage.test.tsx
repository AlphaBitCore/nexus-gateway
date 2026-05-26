import { describe, it, expect, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { renderWithRouter, server } from '@/test/test-utils';
import { CallbackPage } from '../pages/CallbackPage';
import { __setStoredStateForTests } from '../pkce/pkceFlow';
import { clearTokens, getAccessToken } from '../tokens/tokenStore';

describe('CallbackPage', () => {
  beforeEach(() => {
    clearTokens();
    sessionStorage.removeItem('nexus_pkce_state');
  });

  it('renders a spinner during exchange then stores tokens on success', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 'st', postAuthPath: '/dashboard' });
    server.use(
      http.post('/oauth/token', () =>
        HttpResponse.json({
          access_token: 'at-ok',
          refresh_token: 'rt-ok',
          token_type: 'Bearer',
          expires_in: 3600,
        }),
      ),
      http.get('/api/admin/me', () =>
        HttpResponse.json({
          keyId: 'u-cb',
          keyName: 'cb-user',
          roles: ['admins'],
          authPrincipalType: 'admin_user',
        }),
      ),
    );

    renderWithRouter(<CallbackPage />, { route: '/auth/callback?code=c&state=st' });

    // Completion message visible during pending state.
    expect(screen.getByRole('status')).toBeDefined();

    await waitFor(() => {
      expect(getAccessToken()).toBe('at-ok');
    });
  });

  it('renders an error panel with "Sign in again" when exchange fails', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 'st', postAuthPath: '/' });
    server.use(
      http.post('/oauth/token', () =>
        HttpResponse.json({ error: 'invalid_grant' }, { status: 400 }),
      ),
    );

    renderWithRouter(<CallbackPage />, { route: '/auth/callback?code=c&state=st' });

    // Wait for the error UI to appear after the failed exchange.
    expect(
      await screen.findByRole('button', { name: /sign in again/i }),
    ).toBeDefined();
    // Error message echoes the provider error.
    expect(await screen.findByRole('alert')).toBeDefined();
  });

  it('renders an error when callback carries ?error=access_denied', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 'st', postAuthPath: '/' });
    renderWithRouter(<CallbackPage />, {
      route: '/auth/callback?error=access_denied&error_description=user+canceled',
    });
    expect(
      await screen.findByRole('button', { name: /sign in again/i }),
    ).toBeDefined();
    expect((await screen.findByRole('alert')).textContent).toMatch(/access_denied/);
  });
});
