/**
 * PKCE (RFC 7636) utilities for the Authorization Code + PKCE S256 flow.
 *
 * The browser side of PKCE boils down to three primitives:
 *  - generateCodeVerifier:  cryptographically random 43-128 char string
 *  - computeCodeChallenge:  SHA-256 → base64url (no padding) of the verifier
 *  - randomState:           opaque CSRF state for the /oauth/authorize redirect
 *
 * All output strings are unpadded base64url per §4.1 of the RFC. The verifier
 * is sized at 32 bytes → 43 base64url chars, which comfortably fits the
 * 43-char minimum while staying well under the 128-char maximum.
 */

const base64UrlAlphabet = /[+/=]/g;

/** Encode bytes as unpadded base64url (RFC 7636 §4.1). */
export function base64UrlEncode(bytes: Uint8Array): string {
  let binary = '';
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  // btoa → std base64; swap alphabet + strip padding for base64url.
  return btoa(binary).replace(base64UrlAlphabet, (ch) => {
    if (ch === '+') return '-';
    if (ch === '/') return '_';
    return ''; // '=' padding
  });
}

function randomBytes(n: number): Uint8Array {
  const buf = new Uint8Array(n);
  // crypto.getRandomValues is required by spec in secure contexts; tests run
  // under jsdom which polyfills it via node crypto in test setup.
  crypto.getRandomValues(buf);
  return buf;
}

/** Generate a PKCE code verifier: 32 random bytes → base64url (43 chars). */
export function generateCodeVerifier(): string {
  return base64UrlEncode(randomBytes(32));
}

/**
 * Compute the S256 code challenge for a given verifier.
 *
 * Returns `base64url(SHA-256(ASCII(verifier)))` with no padding. Uses the
 * SubtleCrypto API; returns a Promise because digest is async.
 */
export async function computeCodeChallenge(verifier: string): Promise<string> {
  const encoded = new TextEncoder().encode(verifier);
  const digest = await crypto.subtle.digest('SHA-256', encoded);
  return base64UrlEncode(new Uint8Array(digest));
}

/** Generate an opaque state parameter for CSRF protection on /oauth/authorize. */
export function randomState(): string {
  return base64UrlEncode(randomBytes(32));
}
