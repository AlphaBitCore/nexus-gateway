import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  parseSSEBuffer,
  runChat,
  interruptChat,
  newSessionId,
  listSessions,
  getSession,
  deleteSession,
  downloadFile,
  fileIdsIn,
  listModels,
  confirmDecision,
} from './streamChat';
import { clearTokens, setTokens } from '@/auth/tokens/tokenStore';

// sseStream builds a ReadableStream-bearing Response (the GET .../stream side).
function sseStream(body: string): Response {
  const enc = new TextEncoder();
  return {
    ok: true,
    status: 200,
    body: new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(enc.encode(body));
        c.close();
      },
    }),
  } as unknown as Response;
}

// mockSplit routes the two-call P2b protocol: POST .../chat (start) and GET .../stream
// (the SSE body). start defaults to 202-ok; stream yields the given SSE text. Captures
// every request's (url, init) for assertions.
function mockSplit(streamBody: string, start?: { ok?: boolean; status?: number; json?: () => Promise<unknown> }): {
  calls: { url: string; init?: RequestInit }[];
} {
  const calls: { url: string; init?: RequestInit }[] = [];
  global.fetch = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
    calls.push({ url, init });
    if (url.includes('/chat')) {
      return Promise.resolve(
        start ?? ({ ok: true, status: 202, json: async () => ({ sessionId: 's1', seq: 0 }) } as Response),
      );
    }
    return Promise.resolve(sseStream(streamBody)); // /stream
  }) as unknown as typeof fetch;
  return { calls };
}

describe('parseSSEBuffer', () => {
  it('splits complete frames and keeps the trailing remainder', () => {
    const { frames, rest } = parseSSEBuffer(
      'event: text\ndata: {"delta":"hi"}\n\nevent: done\ndata: {}\n\nevent: par',
    );
    expect(frames).toEqual([
      { event: 'text', data: '{"delta":"hi"}' },
      { event: 'done', data: '{}' },
    ]);
    expect(rest).toBe('event: par');
  });

  it('skips frames with no data line', () => {
    const { frames } = parseSSEBuffer(': keep-alive\n\nevent: text\ndata: {"delta":"x"}\n\n');
    expect(frames).toEqual([{ event: 'text', data: '{"delta":"x"}' }]);
  });
});

describe('runChat (command/data-stream split)', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('starts the turn, then streams text deltas and signals done with the turnId', async () => {
    const { calls } = mockSplit(
      'id: 1\nevent: text\ndata: {"delta":"All "}\n\nid: 2\nevent: text\ndata: {"delta":"good"}\n\nid: 3\nevent: done\ndata: {"sessionId":"s1"}\n\n',
    );
    let text = '';
    let doneSid: string | undefined;
    await runChat('s1', 'hi', {
      onText: (d) => (text += d),
      onDone: (s) => {
        doneSid = s;
      },
    });
    expect(text).toBe('All good');
    expect(doneSid).toBe('s1');
    // The POST starts the turn at the session path; the GET observes it.
    expect(calls[0].url).toContain('/sessions/s1/chat');
    expect(calls[0].init?.method).toBe('POST');
    expect(calls[1].url).toContain('/sessions/s1/stream?lastSeq=0');
    // The body carries only the message (the session id is the path param now).
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ message: 'hi' });
  });

  it('reports tool activity from the stream', async () => {
    mockSplit(
      'event: tool_start\ndata: {"name":"observe_health"}\n\nevent: tool_end\ndata: {"name":"observe_health","isError":false}\n\nevent: done\ndata: {}\n\n',
    );
    const started: string[] = [];
    let ended = '';
    await runChat('s1', 'hi', { onToolStart: (n) => started.push(n), onToolEnd: (n) => (ended = n) });
    expect(started).toEqual(['observe_health']);
    expect(ended).toBe('observe_health');
  });

  it('surfaces a structured file event via onFile', async () => {
    mockSplit(
      'event: file\ndata: {"id":"deadbeef12ab","downloadPath":"/api/admin/assistant/files/deadbeef12ab"}\n\nevent: done\ndata: {}\n\n',
    );
    const files: { id: string; downloadPath: string }[] = [];
    await runChat('s1', 'hi', { onFile: (f) => files.push(f) });
    expect(files).toEqual([{ id: 'deadbeef12ab', downloadPath: '/api/admin/assistant/files/deadbeef12ab' }]);
  });

  it('ignores a file event with no id', async () => {
    mockSplit('event: file\ndata: {"downloadPath":"/x"}\n\nevent: done\ndata: {}\n\n');
    let called = false;
    await runChat('s1', 'hi', { onFile: () => (called = true) });
    expect(called).toBe(false);
  });

  it('surfaces a non-ok START response via onError + onDone (no stream opened)', async () => {
    const { calls } = mockSplit('', {
      ok: false,
      status: 409,
      json: async () => ({ error: { message: 'a turn is already running' } }),
    });
    let err = '';
    let done = false;
    await runChat('s1', 'hi', { onError: (m) => (err = m), onDone: () => (done = true) });
    expect(err).toContain('already running');
    expect(done).toBe(true);
    expect(calls.every((c) => !c.url.includes('/stream'))).toBe(true); // never opened the stream
  });

  it('surfaces a network throw on start via onError', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('offline')) as unknown as typeof fetch;
    let err = '';
    await runChat('s1', 'hi', { onError: (m) => (err = m) });
    expect(err).toBe('offline');
  });

  it('treats turn_aborted as a non-error terminal (onAborted, then done)', async () => {
    mockSplit('event: turn_aborted\ndata: {"sessionId":"s1"}\n\n');
    let aborted = false;
    let errored = false;
    await runChat('s1', 'hi', { onAborted: () => (aborted = true), onError: () => (errored = true) });
    expect(aborted).toBe(true);
    expect(errored).toBe(false);
  });

  it('includes a chosen model in the start body', async () => {
    const { calls } = mockSplit('event: done\ndata: {}\n\n');
    await runChat('s1', 'hi', {}, undefined, 'big-m');
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ message: 'hi', model: 'big-m' });
  });

  it('reconnects with the last seq after a stream that ends without a terminal event', async () => {
    const enc = new TextEncoder();
    let streamCall = 0;
    const urls: string[] = [];
    global.fetch = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/chat')) {
        return Promise.resolve({ ok: true, status: 202, json: async () => ({}) } as Response);
      }
      urls.push(url); // /stream
      streamCall++;
      // First connect: deliver seq 1 then end WITHOUT done (server dropped us).
      // Second connect: deliver done.
      const body =
        streamCall === 1
          ? 'id: 1\nevent: text\ndata: {"delta":"part"}\n\n'
          : 'id: 2\nevent: done\ndata: {"sessionId":"s1"}\n\n';
      return Promise.resolve(sseStream(body));
    }) as unknown as typeof fetch;

    let text = '';
    let done = false;
    await runChat('s1', 'hi', { onText: (d) => (text += d), onDone: () => (done = true) });
    expect(text).toBe('part');
    expect(done).toBe(true);
    expect(urls[0]).toContain('lastSeq=0');
    expect(urls[1]).toContain('lastSeq=1'); // resumed from the last seq it saw
  });
});

describe('interruptChat + newSessionId', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('newSessionId returns a non-empty unique-ish string', () => {
    const a = newSessionId();
    const b = newSessionId();
    expect(a).toBeTruthy();
    expect(a).not.toBe(b);
  });

  it('interruptChat POSTs to the interrupt endpoint and swallows errors', async () => {
    const calls: string[] = [];
    global.fetch = vi.fn().mockImplementation((url: string, init: RequestInit) => {
      calls.push(`${init.method} ${url}`);
      return Promise.resolve({ ok: true, status: 204 });
    }) as unknown as typeof fetch;
    await interruptChat('s1');
    expect(calls[0]).toBe('POST /api/admin/assistant/sessions/s1/interrupt');

    global.fetch = vi.fn().mockRejectedValue(new Error('offline')) as unknown as typeof fetch;
    await expect(interruptChat('s1')).resolves.toBeUndefined(); // best-effort, no throw
  });
});

describe('session reuse + CRUD APIs', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('listSessions returns the array, or [] on error', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ sessions: [{ id: 's1', title: 'hi', updatedAt: 't' }] }),
    }) as unknown as typeof fetch;
    expect(await listSessions()).toEqual([{ id: 's1', title: 'hi', updatedAt: 't' }]);
    global.fetch = vi.fn().mockResolvedValue({ ok: false }) as unknown as typeof fetch;
    expect(await listSessions()).toEqual([]);
  });

  it('getSession returns the transcript or null', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ id: 's1', messages: [{ role: 'user', text: 'hi' }] }),
    }) as unknown as typeof fetch;
    expect((await getSession('s1'))?.messages[0].text).toBe('hi');
    global.fetch = vi.fn().mockResolvedValue({ ok: false }) as unknown as typeof fetch;
    expect(await getSession('s2')).toBeNull();
  });

  it('deleteSession reports success from the response', async () => {
    global.fetch = vi.fn().mockResolvedValue({ ok: true }) as unknown as typeof fetch;
    expect(await deleteSession('s1')).toBe(true);
    global.fetch = vi.fn().mockResolvedValue({ ok: false }) as unknown as typeof fetch;
    expect(await deleteSession('s2')).toBe(false);
  });

  it('confirmDecision sends challengeToken only when provided and returns the parsed result', async () => {
    // First Allow (no token): body omits challengeToken; backend returns a challenge.
    let captured: Record<string, unknown> = {};
    global.fetch = vi.fn().mockImplementation((_u: string, init: RequestInit) => {
      captured = JSON.parse(init.body as string);
      return Promise.resolve({
        ok: true,
        json: async () => ({ ok: true, secondConfirmRequired: true, challengeToken: 'tok-1' }),
      });
    }) as unknown as typeof fetch;

    const first = await confirmDecision('s1', 'c1', true);
    expect(captured).toEqual({ sessionId: 's1', callId: 'c1', decision: true });
    expect('challengeToken' in captured).toBe(false);
    expect(first).toEqual({ ok: true, secondConfirmRequired: true, challengeToken: 'tok-1' });

    // Second Allow: body carries the echoed token.
    global.fetch = vi.fn().mockImplementation((_u: string, init: RequestInit) => {
      captured = JSON.parse(init.body as string);
      return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
    }) as unknown as typeof fetch;
    const second = await confirmDecision('s1', 'c1', true, 'tok-1');
    expect(captured).toEqual({ sessionId: 's1', callId: 'c1', decision: true, challengeToken: 'tok-1' });
    expect(second).toEqual({ ok: true });
  });

  it('confirmDecision parses a 409 error body without throwing, and falls back on non-JSON', async () => {
    // 409 invalid_challenge / expired — the body is JSON; confirmDecision must parse it.
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      json: async () => ({ error: { message: 'expired', type: 'expired' } }),
    }) as unknown as typeof fetch;
    const r = await confirmDecision('s1', 'c1', true, 'stale');
    expect(r).toBeDefined(); // parsed without throwing
    expect((r as { ok?: boolean }).ok).toBeFalsy();

    // Non-JSON body → graceful { ok: res.ok } fallback.
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      json: async () => {
        throw new Error('not json');
      },
    }) as unknown as typeof fetch;
    expect(await confirmDecision('s1', 'c1', false)).toEqual({ ok: false });
  });

  it('confirmDecision flags a 409 restart_reissue (NFR-10 pod restart) distinctly from expired', async () => {
    // restart_reissue → reissue:true (the UI tells the user to re-issue, not "expired").
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: { message: 'the assistant restarted', type: 'restart_reissue' } }),
    }) as unknown as typeof fetch;
    expect(await confirmDecision('s1', 'c1', true)).toEqual({ ok: false, reissue: true });

    // A plain expired 409 → reissue:false.
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: { message: 'expired', type: 'expired' } }),
    }) as unknown as typeof fetch;
    expect(await confirmDecision('s1', 'c1', true)).toEqual({ ok: false, reissue: false });
  });

  it('confirmDecision flags a 421 as misrouted (affinity miss), not a generic failure', async () => {
    // The body is never read on 421 — the status alone drives the misrouted flag,
    // so the UI shows "another instance" rather than the misleading "expired".
    const jsonSpy = vi.fn();
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 421,
      json: jsonSpy,
    }) as unknown as typeof fetch;
    const r = await confirmDecision('s1', 'c1', true);
    expect(r).toEqual({ ok: false, misrouted: true });
    expect(jsonSpy).not.toHaveBeenCalled();
  });
});

describe('file sandbox helpers', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('listModels returns default + enriched models, or empty on error', async () => {
    const models = [
      { code: 'm1', label: 'Model One', provider: 'OpenAI' },
      { code: 'm2', label: 'Model Two', provider: 'Anthropic' },
    ];
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ default: 'm1', models }),
    }) as unknown as typeof fetch;
    expect(await listModels()).toEqual({ default: 'm1', models });
    global.fetch = vi.fn().mockResolvedValue({ ok: false }) as unknown as typeof fetch;
    expect(await listModels()).toEqual({ default: '', models: [] });
  });

  it('fileIdsIn extracts download ids from a reply', () => {
    expect(fileIdsIn('wrote "r.txt"; download at /api/admin/assistant/files/abc123')).toEqual(['abc123']);
    expect(fileIdsIn('no files here')).toEqual([]);
  });

  it('downloadFile returns false on a non-ok response', async () => {
    global.fetch = vi.fn().mockResolvedValue({ ok: false }) as unknown as typeof fetch;
    expect(await downloadFile('f1')).toBe(false);
  });

  it('downloadFile fetches the blob with auth and triggers a save', async () => {
    const blob = new Blob(['data']);
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      blob: async () => blob,
      headers: { get: () => 'attachment; filename="r.txt"' },
    }) as unknown as typeof fetch;
    const createURL = vi.fn(() => 'blob:x');
    Object.assign(URL, { createObjectURL: createURL, revokeObjectURL: vi.fn() });
    expect(await downloadFile('f1')).toBe(true);
    expect(createURL).toHaveBeenCalledWith(blob);
  });
});

describe('401 refresh-retry (mid-session token expiry)', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    clearTokens();
  });

  it('runChat refreshes the token and retries the chat POST instead of failing the turn', async () => {
    setTokens({ accessToken: 'at-stale', refreshToken: 'rt-fresh' });
    const sse = 'id: 1\nevent: text\ndata: {"delta":"hi"}\n\nid: 2\nevent: done\ndata: {"sessionId":"s1"}\n\n';
    const calls: { url: string; init?: RequestInit }[] = [];
    global.fetch = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
      calls.push({ url, init });
      if (url.includes('/oauth/token')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ access_token: 'at-new', refresh_token: 'rt-new', token_type: 'Bearer' }),
        } as unknown as Response);
      }
      if (url.includes('/chat')) {
        const auth = (init?.headers as Record<string, string>)?.Authorization;
        if (auth !== 'Bearer at-new') {
          return Promise.resolve({ ok: false, status: 401, json: async () => ({}) } as unknown as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 202,
          json: async () => ({ sessionId: 's1', seq: 0 }),
        } as unknown as Response);
      }
      return Promise.resolve(sseStream(sse)); // /stream
    }) as unknown as typeof fetch;

    let text = '';
    let errored = '';
    await runChat('s1', 'hello', {
      onText: (d) => (text += d),
      onError: (m) => (errored = m),
    });

    expect(errored).toBe('');
    expect(text).toBe('hi');
    // stale POST → token rotation → retried POST → stream
    expect(calls.map((c) => c.url)).toEqual([
      '/api/admin/assistant/sessions/s1/chat',
      expect.stringContaining('/oauth/token'),
      '/api/admin/assistant/sessions/s1/chat',
      '/api/admin/assistant/sessions/s1/stream?lastSeq=0',
    ]);
  });
});


