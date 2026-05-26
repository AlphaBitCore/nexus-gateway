import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { renderWithRouter, server } from '@/test/test-utils';
import { LoginPage } from '../pages/LoginPage';
import { clearTokens } from '../tokens/tokenStore';

describe('LoginPage', () => {
  const originalLocation = window.location;
  let assignSpy: (url: string | URL) => void;
  const assignCalls: Array<string | URL> = [];

  beforeEach(() => {
    clearTokens();
    assignCalls.length = 0;
    assignSpy = (url: string | URL) => {
      assignCalls.push(url);
    };
    delete (window as unknown as { location?: Location }).location;
    (window as unknown as { location: Partial<Location> }).location = {
      assign: assignSpy as unknown as Location['assign'],
      origin: originalLocation.origin,
      href: originalLocation.href,
      pathname: '/login',
      search: '?authctx=test-ctx',
    };
  });

  afterEach(() => {
    (window as unknown as { location: Location }).location = originalLocation;
  });

  it('renders the password form directly when only local IdP is enabled', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [{ id: 'local-id', type: 'local', name: 'Nexus Local' }],
        }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    // Form is shown directly — no click-to-expand.
    expect(await screen.findByLabelText(/email/i)).toBeDefined();
    expect(screen.getByLabelText(/password/i)).toBeDefined();
    // No external IdP buttons seeded, so the "or" divider is hidden too.
    expect(screen.queryByRole('button', { name: /sign in with okta/i })).toBeNull();
  });

  it('submits credentials and navigates to the redirectUri on success', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [{ id: 'local-id', type: 'local', name: 'Nexus Local' }],
        }),
      ),
      http.post('/authserver/password', async ({ request }) => {
        const body = (await request.json()) as { authctx: string; email: string; password: string };
        expect(body.authctx).toBe('test-ctx');
        expect(body.email).toBe('admin@nexus.ai');
        expect(body.password).toBe('hunter2');
        return HttpResponse.json({
          redirectUri: 'http://localhost:3000/auth/callback?code=abc&state=xyz',
        });
      }),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    await userEvent.type(await screen.findByLabelText(/email/i), 'admin@nexus.ai');
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');
    await userEvent.click(screen.getByRole('button', { name: /^sign in$/i }));

    await waitFor(() => expect(assignCalls.length).toBeGreaterThan(0));
    expect(String(assignCalls[0])).toBe(
      'http://localhost:3000/auth/callback?code=abc&state=xyz',
    );
  });

  it('renders an inline error on invalid credentials without navigating', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [{ id: 'local-id', type: 'local', name: 'Nexus Local' }],
        }),
      ),
      http.post('/authserver/password', () =>
        HttpResponse.json({ error: 'invalid_credentials' }, { status: 401 }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    await userEvent.type(await screen.findByLabelText(/email/i), 'admin@nexus.ai');
    await userEvent.type(screen.getByLabelText(/password/i), 'wrong');
    await userEvent.click(screen.getByRole('button', { name: /^sign in$/i }));

    expect(await screen.findByRole('alert')).toBeDefined();
    expect(assignCalls.length).toBe(0);
    // Form stays open for retry.
    expect(screen.getByLabelText(/email/i)).toBeDefined();
  });

  it('renders rate-limit error on 429', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [{ id: 'local-id', type: 'local', name: 'Nexus Local' }],
        }),
      ),
      http.post('/authserver/password', () =>
        HttpResponse.json({ error: 'rate_limited' }, { status: 429 }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    await userEvent.type(await screen.findByLabelText(/email/i), 'admin@nexus.ai');
    await userEvent.type(screen.getByLabelText(/password/i), 'anything');
    await userEvent.click(screen.getByRole('button', { name: /^sign in$/i }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent ?? '').toMatch(/too many attempts/i);
    expect(assignCalls.length).toBe(0);
  });

  it('renders external IdP buttons above the form with an "or" divider when both are enabled', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [
            { id: 'local-id', type: 'local', name: 'Nexus Local' },
            { id: 'okta-prod', type: 'oidc', name: 'Okta' },
          ],
        }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    // Both external button and form are rendered simultaneously.
    expect(await screen.findByRole('button', { name: /sign in with okta/i })).toBeDefined();
    expect(screen.getByLabelText(/email/i)).toBeDefined();
    // Divider with "or" is present between them.
    expect(screen.getByText(/^or$/i)).toBeDefined();
  });

  it('hides the password form when only external IdPs are enabled', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [{ id: 'okta-prod', type: 'oidc', name: 'Okta' }],
        }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    expect(await screen.findByRole('button', { name: /sign in with okta/i })).toBeDefined();
    expect(screen.queryByLabelText(/email/i)).toBeNull();
    expect(screen.queryByLabelText(/password/i)).toBeNull();
  });

  it('navigates to /idp/{id}/start?authctx=… when an external provider is chosen', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({
          providers: [
            { id: 'local-id', type: 'local', name: 'Nexus Local' },
            { id: 'okta-prod', type: 'oidc', name: 'Okta' },
          ],
        }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });

    const oktaBtn = await screen.findByRole('button', { name: /sign in with okta/i });
    await userEvent.click(oktaBtn);

    await waitFor(() => expect(assignCalls.length).toBeGreaterThan(0));
    const target = new URL(String(assignCalls[0]));
    expect(target.pathname).toBe('/idp/okta-prod/start');
    expect(target.searchParams.get('authctx')).toBe('test-ctx');
  });

  it('redirects to /oauth/authorize with PKCE params when authctx is missing', async () => {
    // authctx missing → the page calls login() which redirects to /oauth/authorize.
    (window as unknown as { location: Partial<Location> }).location.search = '';
    renderWithRouter(<LoginPage />, { route: '/login' });

    await waitFor(() => expect(assignCalls.length).toBeGreaterThan(0));
    const url = new URL(String(assignCalls[0]));
    expect(url.pathname).toBe('/oauth/authorize');
    expect(url.searchParams.get('response_type')).toBe('code');
    expect(url.searchParams.get('client_id')).toBe('cp-ui');
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
  });

  it('restarts login automatically when authctx is expired', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({ error: 'authctx_expired' }, { status: 400 }),
      ),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=expired-ctx' });

    await waitFor(() => expect(assignCalls.length).toBeGreaterThan(0));
    const url = new URL(String(assignCalls[0]));
    expect(url.pathname).toBe('/oauth/authorize');
    expect(url.searchParams.get('response_type')).toBe('code');
    expect(url.searchParams.get('client_id')).toBe('cp-ui');
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
  });
});
