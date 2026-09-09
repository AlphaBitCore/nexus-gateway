import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { renderWithRouter, server } from '@/test/test-utils';
import { useLocation } from 'react-router-dom';
import { LoginPage } from '../../../src/auth/pages/LoginPage';

// "No password form" is satisfied by a page that navigated away AND by a page
// that never rendered, so it cannot tell the two apart. This reports where the
// router actually went.
function Where() {
  const l = useLocation();
  return <span data-testid="where">{`${l.pathname}${l.search}`}</span>;
}
import { clearTokens, setTokens } from '../../../src/auth/tokens/tokenStore';

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

  it('navigates to /authserver/idp/{id}/start?authctx=… when an external provider is chosen', async () => {
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
    expect(target.pathname).toBe('/authserver/idp/okta-prod/start');
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

  it('shows an inline load error when the IdP list fails to load (non-expiry)', async () => {
    server.use(http.get('/authserver/idps', () => HttpResponse.error()));
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });
    expect(await screen.findByText(/unable to load sign-in methods/i)).toBeInTheDocument();
  });

  it('self-heals by restarting login when the authctx expires at submit time', async () => {
    server.use(
      http.get('/authserver/idps', () =>
        HttpResponse.json({ providers: [{ id: 'local-id', type: 'local', name: 'Nexus Local' }] }),
      ),
      http.post('/authserver/password', () => HttpResponse.json({ error: 'authctx_expired' }, { status: 400 })),
    );
    renderWithRouter(<LoginPage />, { route: '/login?authctx=test-ctx' });
    await userEvent.type(await screen.findByLabelText(/email/i), 'admin@nexus.ai');
    await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');
    await userEvent.click(screen.getByRole('button', { name: /^sign in$/i }));
    // expired authctx → silent OAuth restart (assign to /oauth/authorize), no error text
    await waitFor(() => expect(assignCalls.some((u) => new URL(String(u)).pathname === '/oauth/authorize')).toBe(true));
  });

  // The already-signed-in arm: `nexus login` opens a browser tab at /login with
  // an authctx while the console session is still live. Nothing here was tested.
  // The whole point of the arm is that the CLI's loopback listener is waiting on
  // a redirect that only this page can produce — navigate to "/" instead and the
  // CLI hangs with no error on either side, which is what it did before this arm
  // existed.
  describe('LoginPage — an operator who is already signed in', () => {
    const signIn = () => setTokens({ accessToken: 'at', refreshToken: 'rt' });

    it('approves the pending authorize with the live session and hands the CLI its redirect', async () => {
      let sentAuthctx = '';
      let sentAuth = '';
      server.use(
        http.post('/authserver/approve', async ({ request }) => {
          sentAuth = request.headers.get('Authorization') ?? '';
          sentAuthctx = ((await request.json()) as { authctx: string }).authctx;
          return HttpResponse.json({ redirectUri: 'http://127.0.0.1:51234/cb?code=abc&state=xyz' });
        }),
      );
      signIn();
      renderWithRouter(<LoginPage />, { route: '/login?authctx=cli-ctx' });

      await waitFor(() => expect(assignCalls).toHaveLength(1));
      expect(assignCalls[0]).toBe('http://127.0.0.1:51234/cb?code=abc&state=xyz');
      // The existing session is what authorises the approval — sending the
      // authctx without it would make this endpoint mint a code for an
      // unauthenticated caller.
      expect(sentAuth).toBe('Bearer at');
      expect(sentAuthctx).toBe('cli-ctx');
      // No login form: the operator is signed in and is not asked again.
      expect(screen.queryByLabelText(/password/i)).toBeNull();
    });

    it('goes to the app rather than the CLI when there is no authorize to approve', async () => {
      // A signed-in operator who simply navigates to /login. There is nothing to
      // approve, so the page must get out of the way instead of showing a form
      // to someone who is already authenticated.
      let approveCalls = 0;
      server.use(
        http.post('/authserver/approve', () => {
          approveCalls += 1;
          return HttpResponse.json({ redirectUri: 'http://unwanted.invalid/' });
        }),
      );
      signIn();
      renderWithRouter(
        <>
          <LoginPage />
          <Where />
        </>,
        { route: '/login' },
      );

      // The probe mounting is what proves the page rendered at all, so the
      // navigation below is a navigation rather than a blank screen.
      expect(screen.getByTestId('where').textContent).toBe('/login');
      await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/'));
      expect(approveCalls).toBe(0);
      expect(assignCalls).toHaveLength(0);
    });

    it('restarts the whole dance when the authctx has already been consumed', async () => {
      // An authctx is one-shot and short-lived. Reloading the tab replays a
      // spent one; stranding the operator on /login with a dead context is a
      // dead end they cannot get out of by trying again.
      server.use(
        http.post('/authserver/approve', () =>
          HttpResponse.json({ error: 'authctx_expired', message: 'expired' }, { status: 400 }),
        ),
      );
      signIn();
      renderWithRouter(<LoginPage />, { route: '/login?authctx=spent-ctx' });

      await waitFor(() => expect(assignCalls).toHaveLength(1));
      expect(String(assignCalls[0])).toContain('/oauth/authorize');
    });

    it('sends the operator home on any other approval failure instead of retrying forever', async () => {
      // A 500 from the authserver is not something a fresh authctx fixes. The
      // page must not restart the dance into the same error; the operator
      // re-runs `nexus login` when they are ready.
      server.use(
        http.post('/authserver/approve', () =>
          HttpResponse.json({ error: 'internal_error', message: 'boom' }, { status: 500 }),
        ),
      );
      signIn();
      renderWithRouter(
        <>
          <LoginPage />
          <Where />
        </>,
        { route: '/login?authctx=cli-ctx' },
      );

      await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/'));
      // Home, not another trip through /oauth/authorize — a fresh authctx does
      // not fix a 500, and restarting would loop the operator through it.
      expect(assignCalls.filter((u) => String(u).includes('/oauth/authorize'))).toHaveLength(0);
    });
  });
});
