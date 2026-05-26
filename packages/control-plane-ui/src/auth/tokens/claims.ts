/**
 * Client-side decoding of access-token JWTs.
 *
 * The server is the source of truth for signature and expiry validation; this
 * module only extracts display-friendly claims (`sub`, `email`, `exp`, …) so
 * the UI can show an identity while `/api/admin/me` is in flight and so
 * `isExpired` can short-circuit an obvious-expired refresh before hitting the
 * network. Anything security-critical must round-trip to the server.
 */

/** Claims surfaced by the authserver in cp-admin access tokens. */
export interface AccessTokenClaims {
  sub?: string;
  email?: string;
  exp?: number;
  sid?: string;
  client_id?: string;
  amr?: string[];
  device_id?: string;
  idp?: string;
}

/**
 * Decode a JWT's payload without verifying the signature.
 *
 * Returns null for any malformed input: wrong segment count, invalid base64url,
 * non-JSON payload, or payload that is not a JSON object. Never throws.
 */
export function decodeAccessToken(jwt: string | undefined | null): AccessTokenClaims | null {
  if (!jwt) return null;
  const parts = jwt.split('.');
  if (parts.length !== 3) return null;
  const payload = parts[1];
  try {
    // base64url → base64 → binary string → UTF-8.
    const padded = payload + '==='.slice((payload.length + 3) % 4);
    const b64 = padded.replace(/-/g, '+').replace(/_/g, '/');
    const binary = atob(b64);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    const json = new TextDecoder().decode(bytes);
    const parsed = JSON.parse(json) as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    return parsed as AccessTokenClaims;
  } catch {
    return null;
  }
}

/**
 * Returns true when `claims.exp` is in the past (or missing).
 *
 * A `skewSeconds` cushion lets us treat "about to expire" as "already expired"
 * so that in-flight requests aren't fired with a token the server will reject.
 */
export function isExpired(claims: AccessTokenClaims | null, skewSeconds = 30): boolean {
  if (!claims || typeof claims.exp !== 'number') return true;
  const nowSec = Math.floor(Date.now() / 1000);
  return claims.exp - skewSeconds <= nowSec;
}
