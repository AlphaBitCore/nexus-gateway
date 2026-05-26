import { describe, it, expect, vi, afterEach } from 'vitest';
import { buildClientErrorReportBody } from './errorReporting';

describe('buildClientErrorReportBody', () => {
  it('includes service, kind, message, and stack', () => {
    const err = new Error('test failure');
    err.stack = 'Error: test failure\n  at foo.ts:1:1';
    const body = buildClientErrorReportBody({ kind: 'react', error: err, componentStack: 'in Foo' });
    expect(body.service).toBe('nexus-dashboard');
    expect(body.kind).toBe('react');
    expect(body.message).toBe('test failure');
    expect(body.stack).toContain('test failure');
    expect(body.componentStack).toBe('in Foo');
    expect(typeof body.ts).toBe('string');
  });

  it('truncates long stacks', () => {
    const err = new Error('x');
    err.stack = 'a'.repeat(20_000);
    const body = buildClientErrorReportBody({ kind: 'window', error: err });
    expect(String(body.stack).length).toBeLessThan(20_000);
    expect(String(body.stack)).toContain('[truncated]');
  });
});

describe('reportClientError (integration)', () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  it('uses sendBeacon when URL is configured', async () => {
    vi.stubEnv('VITE_CLIENT_ERROR_REPORTING_URL', 'https://example.com/client-errors');
    const beacon = vi.fn().mockReturnValue(true);
    Object.defineProperty(globalThis.navigator, 'sendBeacon', {
      configurable: true,
      writable: true,
      value: beacon,
    });
    vi.resetModules();
    const { reportClientError } = await import('./errorReporting');
    reportClientError({ kind: 'react', error: new Error('boom') });
    expect(beacon).toHaveBeenCalledTimes(1);
    const [url, data] = beacon.mock.calls[0] as [string, Blob];
    expect(url).toBe('https://example.com/client-errors');
    expect(data).toBeInstanceOf(Blob);
    const text = await data.text();
    const parsed = JSON.parse(text) as { message: string; kind: string };
    expect(parsed.message).toBe('boom');
    expect(parsed.kind).toBe('react');
  });

  it('falls back to fetch when sendBeacon returns false', async () => {
    vi.stubEnv('VITE_CLIENT_ERROR_REPORTING_URL', 'https://example.com/client-errors');
    Object.defineProperty(globalThis.navigator, 'sendBeacon', {
      configurable: true,
      writable: true,
      value: vi.fn().mockReturnValue(false),
    });
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response());
    vi.resetModules();
    const { reportClientError } = await import('./errorReporting');
    reportClientError({ kind: 'unhandledrejection', error: new Error('rej') });
    expect(fetchSpy).toHaveBeenCalledWith(
      'https://example.com/client-errors',
      expect.objectContaining({
        method: 'POST',
        credentials: 'omit',
        keepalive: true,
      }),
    );
  });
});
