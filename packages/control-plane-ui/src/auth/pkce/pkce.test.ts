import { describe, it, expect } from 'vitest';
import { base64UrlEncode, computeCodeChallenge, generateCodeVerifier, randomState } from '../pkce/pkce';

describe('base64UrlEncode', () => {
  it('encodes without padding and with URL-safe alphabet', () => {
    const bytes = new Uint8Array([0xff, 0xff, 0xff]);
    // std base64 → //// (with no padding); url-safe → ____
    expect(base64UrlEncode(bytes)).toBe('____');
  });

  it('strips = padding for non-3-aligned inputs', () => {
    const bytes = new Uint8Array([0x01, 0x02]);
    const encoded = base64UrlEncode(bytes);
    expect(encoded.endsWith('=')).toBe(false);
  });
});

describe('generateCodeVerifier', () => {
  it('returns a 43-char base64url string', () => {
    const v = generateCodeVerifier();
    expect(v).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it('is unique across calls', () => {
    const a = generateCodeVerifier();
    const b = generateCodeVerifier();
    expect(a).not.toBe(b);
  });
});

describe('computeCodeChallenge', () => {
  // RFC 7636 Appendix B test vector: the canonical {verifier, challenge} pair.
  // Locking this down means an accidental algorithm change (wrong digest,
  // base64 vs base64url, padding) is caught immediately rather than after a
  // login regression.
  it('matches RFC 7636 Appendix B vector', async () => {
    const verifier = 'dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk';
    const expected = 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM';
    const challenge = await computeCodeChallenge(verifier);
    expect(challenge).toBe(expected);
  });

  it('is deterministic for a given verifier', async () => {
    const verifier = generateCodeVerifier();
    const a = await computeCodeChallenge(verifier);
    const b = await computeCodeChallenge(verifier);
    expect(a).toBe(b);
  });
});

describe('randomState', () => {
  it('returns a URL-safe base64 string of non-trivial length', () => {
    const s = randomState();
    expect(s).toMatch(/^[A-Za-z0-9_-]+$/);
    expect(s.length).toBeGreaterThanOrEqual(43);
  });

  it('is unique across calls', () => {
    expect(randomState()).not.toBe(randomState());
  });
});
