import { describe, it, expect, beforeEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../test/msw-server';
import { api, ApiError, __testing__ } from './client';
import { clearTokens, getAccessToken, getRefreshToken, setTokens } from '../auth/tokens/tokenStore';

describe('api client — Bearer authentication', () => {
  beforeEach(() => {
    clearTokens();
    setTokens({ accessToken: 'at-fresh', refreshToken: 'rt-fresh' });
  });

  it('attaches Authorization: Bearer <access_token> on every request', async () => {
    let observed: string | null = null;
    server.use(
      http.get('/api/admin/ping', ({ request }) => {
        observed = request.headers.get('authorization');
        return HttpResponse.json({ ok: true });
      }),
    );
    const out = await api.get<{ ok: boolean }>('/api/admin/ping');
    expect(out).toEqual({ ok: true });
    expect(observed).toBe('Bearer at-fresh');
  });

  it('returns undefined for 204 No Content', async () => {
    server.use(http.delete('/api/admin/thing/1', () => new HttpResponse(null, { status: 204 })));
    const out = await api.delete('/api/admin/thing/1');
    expect(out).toBeUndefined();
  });

  it('sends JSON body + Content-Type on POST', async () => {
    let body: unknown = null;
    let ct: string | null = null;
    server.use(
      http.post('/api/admin/things', async ({ request }) => {
        ct = request.headers.get('content-type');
        body = await request.json();
        return HttpResponse.json({ id: 'x' }, { status: 201 });
      }),
    );
    const out = await api.post<{ id: string }>('/api/admin/things', { name: 'hello' });
    expect(out).toEqual({ id: 'x' });
    expect(ct).toContain('application/json');
    expect(body).toEqual({ name: 'hello' });
  });
});

describe('api client — 403 IAM error details', () => {
  beforeEach(() => {
    clearTokens();
    setTokens({ accessToken: 'at-fresh', refreshToken: 'rt-fresh' });
  });

  it('maps IAM 403 details into ApiError message and forbiddenDetails', async () => {
    server.use(
      http.get('/api/admin/providers/p1', () =>
        HttpResponse.json(
          {
            error: {
              message: 'Access denied by IAM policy',
              type: 'authorization_error',
              code: 'IAM_ACCESS_DENIED',
              details: {
                action: 'admin:provider.delete',
                resource: 'nrn:nexus:gateway:*:provider/x',
                reason: 'ExplicitDeny',
              },
            },
          },
          { status: 403 },
        ),
      ),
    );

    let caught: unknown;
    try {
      await api.get('/api/admin/providers/p1');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(ApiError);
    const err = caught as ApiError;
    expect(err.status).toBe(403);
    expect(err.code).toBe('IAM_ACCESS_DENIED');
    expect(err.message).toContain('admin:provider.delete');
    expect(err.message).toContain('nrn:nexus:gateway:*:provider/x');
    expect(err.forbiddenDetails?.action).toBe('admin:provider.delete');
    expect(err.forbiddenDetails?.reason).toBe('ExplicitDeny');
  });
});

describe('api client — refresh-token rotation on 401', () => {
  beforeEach(() => {
    clearTokens();
    setTokens({ accessToken: 'at-stale', refreshToken: 'rt-fresh' });
  });

  it('rotates the refresh token and retries the original request once', async () => {
    let getCallCount = 0;
    server.use(
      http.get('/api/admin/protected', ({ request }) => {
        getCallCount += 1;
        const auth = request.headers.get('authorization');
        if (auth === 'Bearer at-new') {
          return HttpResponse.json({ data: 'ok' });
        }
        return HttpResponse.json({ error: { code: 'UNAUTHORIZED' } }, { status: 401 });
      }),
      http.post('/oauth/token', async ({ request }) => {
        const form = new URLSearchParams(await request.text());
        expect(form.get('grant_type')).toBe('refresh_token');
        expect(form.get('refresh_token')).toBe('rt-fresh');
        expect(form.get('client_id')).toBe('cp-ui');
        return HttpResponse.json({
          access_token: 'at-new',
          refresh_token: 'rt-new',
          token_type: 'Bearer',
          expires_in: 3600,
        });
      }),
    );

    const res = await api.get<{ data: string }>('/api/admin/protected');
    expect(res).toEqual({ data: 'ok' });
    // Original call + retry = 2 hits.
    expect(getCallCount).toBe(2);
    // Rotated tokens were stored.
    expect(getAccessToken()).toBe('at-new');
    expect(getRefreshToken()).toBe('rt-new');
  });

  it('serializes concurrent 401s into a single /oauth/token call', async () => {
    let tokenCallCount = 0;
    let getCallCount = 0;
    server.use(
      http.get('/api/admin/a', ({ request }) => {
        getCallCount += 1;
        const auth = request.headers.get('authorization');
        if (auth === 'Bearer at-rotated') return HttpResponse.json({ which: 'a' });
        return HttpResponse.json({ error: { code: 'UNAUTHORIZED' } }, { status: 401 });
      }),
      http.get('/api/admin/b', ({ request }) => {
        getCallCount += 1;
        const auth = request.headers.get('authorization');
        if (auth === 'Bearer at-rotated') return HttpResponse.json({ which: 'b' });
        return HttpResponse.json({ error: { code: 'UNAUTHORIZED' } }, { status: 401 });
      }),
      http.post('/oauth/token', () => {
        tokenCallCount += 1;
        return HttpResponse.json({
          access_token: 'at-rotated',
          refresh_token: 'rt-rotated',
          token_type: 'Bearer',
          expires_in: 3600,
        });
      }),
    );

    const [a, b] = await Promise.all([
      api.get<{ which: string }>('/api/admin/a'),
      api.get<{ which: string }>('/api/admin/b'),
    ]);
    expect(a.which).toBe('a');
    expect(b.which).toBe('b');
    // Both endpoints: 401 then retry = 4 total GETs.
    expect(getCallCount).toBe(4);
    // Refresh token was rotated exactly once — concurrent 401s share the
    // in-flight refresh promise.
    expect(tokenCallCount).toBe(1);
  });

  it('clears tokens and rejects with 401 when refresh itself fails', async () => {
    server.use(
      http.get('/api/admin/dead', () =>
        HttpResponse.json({ error: { code: 'UNAUTHORIZED' } }, { status: 401 }),
      ),
      http.post('/oauth/token', () =>
        HttpResponse.json({ error: 'invalid_grant' }, { status: 400 }),
      ),
    );

    // Stub window.location so the client's /login redirect doesn't crash JSDOM.
    const origLoc = window.location;
    delete (window as unknown as { location?: Location }).location;
    (window as unknown as { location: Partial<Location> }).location = {
      ...origLoc,
      pathname: '/dashboard',
      assign: () => {},
    };

    try {
      let caught: unknown;
      try {
        await api.get('/api/admin/dead');
      } catch (e) {
        caught = e;
      }
      expect(caught).toBeInstanceOf(ApiError);
      expect((caught as ApiError).status).toBe(401);
      // Tokens wiped after refresh failure.
      expect(getAccessToken()).toBeUndefined();
      expect(getRefreshToken()).toBeUndefined();
    } finally {
      (window as unknown as { location: Location }).location = origLoc;
    }
  });
});

describe('filenameFromContentDisposition', () => {
  const parse = __testing__.filenameFromContentDisposition;

  it('returns null for missing header', () => {
    expect(parse(null)).toBeNull();
    expect(parse('')).toBeNull();
  });

  it('parses quoted filename', () => {
    expect(parse('attachment; filename="compliance-events.csv"')).toBe('compliance-events.csv');
  });

  it('parses unquoted filename', () => {
    expect(parse('attachment; filename=report.csv')).toBe('report.csv');
  });

  it('prefers RFC 5987 filename* over plain filename', () => {
    expect(parse("attachment; filename=fallback.csv; filename*=UTF-8''%E6%8A%A5%E5%91%8A.csv"))
      .toBe('报告.csv');
  });

  it('returns null when no filename token present', () => {
    expect(parse('attachment')).toBeNull();
  });
});

describe('api.download — authenticated file download', () => {
  beforeEach(() => {
    clearTokens();
    setTokens({ accessToken: 'at-dl', refreshToken: 'rt-dl' });
  });

  it('attaches Bearer token and returns blob + filename from Content-Disposition', async () => {
    let observed: string | null = null;
    server.use(
      http.get('/api/admin/export', ({ request }) => {
        observed = request.headers.get('authorization');
        return new HttpResponse('a,b,c\n1,2,3\n', {
          status: 200,
          headers: {
            'Content-Type': 'text/csv; charset=utf-8',
            'Content-Disposition': 'attachment; filename="rows.csv"',
          },
        });
      }),
    );
    const { blob, filename } = await api.getBlob('/api/admin/export');
    expect(observed).toBe('Bearer at-dl');
    expect(filename).toBe('rows.csv');
    expect(await blob.text()).toBe('a,b,c\n1,2,3\n');
  });

  it('propagates ApiError on 4xx so the caller can toast the reason', async () => {
    server.use(
      http.get('/api/admin/export', () =>
        HttpResponse.json(
          { error: { code: 'FORBIDDEN', message: 'nope', type: 'authorization_error' } },
          { status: 403 },
        ),
      ),
    );
    let caught: unknown;
    try {
      await api.getBlob('/api/admin/export');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(ApiError);
    expect((caught as ApiError).status).toBe(403);
  });

  it('download() triggers a programmatic click on a synthetic anchor', async () => {
    server.use(
      http.get('/api/admin/export', () =>
        new HttpResponse('x,y\n1,2\n', {
          status: 200,
          headers: {
            'Content-Type': 'text/csv',
            'Content-Disposition': 'attachment; filename="data.csv"',
          },
        }),
      ),
    );

    // jsdom does not implement URL.createObjectURL; stub before spying.
    if (typeof URL.createObjectURL === 'undefined') {
      (URL as unknown as { createObjectURL: () => string }).createObjectURL = () => '';
    }
    if (typeof URL.revokeObjectURL === 'undefined') {
      (URL as unknown as { revokeObjectURL: () => void }).revokeObjectURL = () => {};
    }
    const createSpy = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock');
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const clicks: HTMLAnchorElement[] = [];
    const realCreateElement = document.createElement.bind(document);
    const createElSpy = vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const el = realCreateElement(tag) as HTMLElement;
      if (tag.toLowerCase() === 'a') {
        const a = el as HTMLAnchorElement;
        a.click = () => clicks.push(a);
      }
      return el as unknown as HTMLAnchorElement;
    });

    try {
      await api.download('/api/admin/export', undefined, 'fallback.csv');
      expect(createSpy).toHaveBeenCalledOnce();
      expect(revokeSpy).toHaveBeenCalledWith('blob:mock');
      expect(clicks).toHaveLength(1);
      expect(clicks[0].download).toBe('data.csv');
      expect(clicks[0].href).toBe('blob:mock');
    } finally {
      createSpy.mockRestore();
      revokeSpy.mockRestore();
      createElSpy.mockRestore();
    }
  });
});
