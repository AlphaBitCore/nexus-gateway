import { describe, it, expect } from 'vitest';
import { decodeAccessToken, isExpired } from '../tokens/claims';

function encodeSegment(obj: unknown): string {
  const json = JSON.stringify(obj);
  const b64 = btoa(json);
  return b64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function makeJwt(payload: unknown): string {
  const header = encodeSegment({ alg: 'RS256', typ: 'JWT' });
  const body = encodeSegment(payload);
  return `${header}.${body}.sig-ignored`;
}

describe('decodeAccessToken', () => {
  it('returns null for empty / null input', () => {
    expect(decodeAccessToken('')).toBeNull();
    expect(decodeAccessToken(null)).toBeNull();
    expect(decodeAccessToken(undefined)).toBeNull();
  });

  it('returns null for tokens with wrong segment count', () => {
    expect(decodeAccessToken('only.two')).toBeNull();
    expect(decodeAccessToken('a.b.c.d')).toBeNull();
  });

  it('returns null for non-base64 payload', () => {
    expect(decodeAccessToken('hdr.!!!.sig')).toBeNull();
  });

  it('returns null when payload is not a JSON object', () => {
    const jwt = `hdr.${encodeSegment('just a string')}.sig`;
    expect(decodeAccessToken(jwt)).toBeNull();
  });

  it('returns parsed claims for a valid JWT payload', () => {
    const jwt = makeJwt({
      sub: 'usr-1',
      email: 'alice@nexus.ai',
      exp: 1_800_000_000,
      sid: 'sid-x',
      client_id: 'cp-ui',
      amr: ['pwd'],
    });
    const claims = decodeAccessToken(jwt);
    expect(claims).toEqual({
      sub: 'usr-1',
      email: 'alice@nexus.ai',
      exp: 1_800_000_000,
      sid: 'sid-x',
      client_id: 'cp-ui',
      amr: ['pwd'],
    });
  });
});

describe('isExpired', () => {
  it('treats missing claims as expired', () => {
    expect(isExpired(null)).toBe(true);
    expect(isExpired({})).toBe(true);
  });

  it('returns true for past exp', () => {
    const past = Math.floor(Date.now() / 1000) - 60;
    expect(isExpired({ exp: past })).toBe(true);
  });

  it('returns false for comfortably future exp', () => {
    const future = Math.floor(Date.now() / 1000) + 3600;
    expect(isExpired({ exp: future })).toBe(false);
  });

  it('applies the skew cushion (exp within skew window is expired)', () => {
    const nowPlus10 = Math.floor(Date.now() / 1000) + 10;
    expect(isExpired({ exp: nowPlus10 }, 30)).toBe(true);
  });
});
