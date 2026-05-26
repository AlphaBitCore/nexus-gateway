import { describe, it, expect, beforeEach } from 'vitest';
import { clearTokens, getAccessToken, getRefreshToken, setTokens } from '../tokens/tokenStore';

describe('tokenStore', () => {
  beforeEach(() => {
    sessionStorage.clear();
  });

  it('returns undefined when no tokens are stored', () => {
    expect(getAccessToken()).toBeUndefined();
    expect(getRefreshToken()).toBeUndefined();
  });

  it('stores + retrieves tokens', () => {
    setTokens({ accessToken: 'at-1', refreshToken: 'rt-1' });
    expect(getAccessToken()).toBe('at-1');
    expect(getRefreshToken()).toBe('rt-1');
  });

  it('overwrites on repeated setTokens', () => {
    setTokens({ accessToken: 'at-1', refreshToken: 'rt-1' });
    setTokens({ accessToken: 'at-2', refreshToken: 'rt-2' });
    expect(getAccessToken()).toBe('at-2');
    expect(getRefreshToken()).toBe('rt-2');
  });

  it('clearTokens removes both entries', () => {
    setTokens({ accessToken: 'at', refreshToken: 'rt' });
    clearTokens();
    expect(getAccessToken()).toBeUndefined();
    expect(getRefreshToken()).toBeUndefined();
  });

  it('clearTokens is safe when nothing is stored', () => {
    expect(() => clearTokens()).not.toThrow();
  });
});
