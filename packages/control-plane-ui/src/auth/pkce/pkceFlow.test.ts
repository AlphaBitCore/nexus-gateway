import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../test/msw-server';
import {
  buildAuthorizeUrl,
  consumeCallback,
  startLogin,
  OAUTH_CLIENT_ID,
  OAUTH_CALLBACK_PATH,
  __setStoredStateForTests,
  __resetConsumeCallbackCacheForTests,
} from '../pkce/pkceFlow';

describe('buildAuthorizeUrl', () => {
  it('includes all required PKCE parameters', () => {
    const url = buildAuthorizeUrl({
      state: 'st-1',
      challenge: 'ch-1',
      origin: 'http://localhost:3000',
    });
    const u = new URL(url);
    expect(u.pathname).toBe('/oauth/authorize');
    expect(u.searchParams.get('response_type')).toBe('code');
    expect(u.searchParams.get('client_id')).toBe(OAUTH_CLIENT_ID);
    expect(u.searchParams.get('redirect_uri')).toBe(`http://localhost:3000${OAUTH_CALLBACK_PATH}`);
    expect(u.searchParams.get('scope')).toBe('admin openid profile email');
    expect(u.searchParams.get('state')).toBe('st-1');
    expect(u.searchParams.get('code_challenge')).toBe('ch-1');
    expect(u.searchParams.get('code_challenge_method')).toBe('S256');
  });
});

describe('startLogin', () => {
  const originalLocation = window.location;
  let assignSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    sessionStorage.clear();
    // JSDOM's window.location is not configurable, so swap the whole property
    // for a stub with an assign() spy. This lets us assert the navigation
    // target without triggering a real page load.
    assignSpy = vi.fn();
    delete (window as unknown as { location?: Location }).location;
    (window as unknown as { location: Partial<Location> }).location = {
      assign: assignSpy as unknown as Location['assign'],
      origin: originalLocation.origin,
      href: originalLocation.href,
      pathname: originalLocation.pathname,
      search: originalLocation.search,
    };
  });

  afterEach(() => {
    (window as unknown as { location: Location }).location = originalLocation;
  });

  it('persists a PKCE blob and navigates to /oauth/authorize', async () => {
    await startLogin('/dashboard?tab=providers');

    // Blob stashed under the documented key.
    const raw = sessionStorage.getItem('nexus_pkce_state');
    expect(raw).toBeTruthy();
    const blob = JSON.parse(raw!) as { verifier: string; state: string; postAuthPath: string };
    expect(blob.verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(blob.state).toMatch(/^[A-Za-z0-9_-]+$/);
    expect(blob.postAuthPath).toBe('/dashboard?tab=providers');

    // Navigation happened.
    expect(assignSpy).toHaveBeenCalledTimes(1);
    const navUrl = new URL(assignSpy.mock.calls[0][0] as string);
    expect(navUrl.pathname).toBe('/oauth/authorize');
    expect(navUrl.searchParams.get('state')).toBe(blob.state);
    expect(navUrl.searchParams.get('code_challenge_method')).toBe('S256');
    // Challenge must not leak the verifier.
    expect(navUrl.searchParams.get('code_challenge')).not.toBe(blob.verifier);
  });
});

describe('consumeCallback', () => {
  beforeEach(() => {
    sessionStorage.clear();
    __resetConsumeCallbackCacheForTests();
  });

  it('throws when callback carries ?error=access_denied', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 's', postAuthPath: '/' });
    await expect(consumeCallback('?error=access_denied&error_description=denied%20by%20user')).rejects.toThrow(
      /access_denied: denied by user/,
    );
    // Clears stored blob to prevent replay.
    expect(sessionStorage.getItem('nexus_pkce_state')).toBeNull();
  });

  it('throws when code or state is missing', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 's', postAuthPath: '/' });
    await expect(consumeCallback('?state=abc')).rejects.toThrow(/missing required code or state/);
    expect(sessionStorage.getItem('nexus_pkce_state')).toBeNull();
  });

  it('throws when no stored PKCE state exists', async () => {
    await expect(consumeCallback('?code=c&state=s')).rejects.toThrow(/No pending OAuth flow/);
  });

  it('throws on state mismatch (CSRF guard)', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 'stored-state', postAuthPath: '/' });
    await expect(consumeCallback('?code=c&state=different-state')).rejects.toThrow(/state mismatch/);
    // Stored blob cleared even on mismatch.
    expect(sessionStorage.getItem('nexus_pkce_state')).toBeNull();
  });

  it('exchanges code for tokens and returns the postAuthPath', async () => {
    __setStoredStateForTests({
      verifier: 'verifier-xyz',
      state: 'state-abc',
      postAuthPath: '/providers',
    });

    let capturedBody: string | null = null;
    server.use(
      http.post('/oauth/token', async ({ request }) => {
        capturedBody = await request.text();
        return HttpResponse.json({
          access_token: 'at-1',
          refresh_token: 'rt-1',
          token_type: 'Bearer',
          expires_in: 3600,
          scope: 'admin openid profile email',
        });
      }),
    );

    const result = await consumeCallback('?code=auth-code&state=state-abc');

    expect(result.tokens).toEqual({ accessToken: 'at-1', refreshToken: 'rt-1' });
    expect(result.postAuthPath).toBe('/providers');
    // Body is form-encoded with the PKCE verifier + client_id + redirect_uri.
    expect(capturedBody).not.toBeNull();
    const form = new URLSearchParams(capturedBody!);
    expect(form.get('grant_type')).toBe('authorization_code');
    expect(form.get('code')).toBe('auth-code');
    expect(form.get('code_verifier')).toBe('verifier-xyz');
    expect(form.get('client_id')).toBe('cp-ui');
    expect(form.get('redirect_uri')).toBe(`${window.location.origin}${OAUTH_CALLBACK_PATH}`);
    // Stored blob cleared after successful exchange.
    expect(sessionStorage.getItem('nexus_pkce_state')).toBeNull();
  });

  it('throws when /oauth/token returns non-2xx', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 's', postAuthPath: '/' });
    server.use(
      http.post('/oauth/token', () =>
        HttpResponse.json({ error: 'invalid_grant' }, { status: 400 }),
      ),
    );
    await expect(consumeCallback('?code=c&state=s')).rejects.toThrow(/Token exchange failed \(400\)/);
  });

  it('throws when /oauth/token response omits access_token', async () => {
    __setStoredStateForTests({ verifier: 'v', state: 's', postAuthPath: '/' });
    server.use(
      http.post('/oauth/token', () =>
        HttpResponse.json({ token_type: 'Bearer' }),
      ),
    );
    await expect(consumeCallback('?code=c&state=s')).rejects.toThrow(/missing access_token or refresh_token/);
  });
});
