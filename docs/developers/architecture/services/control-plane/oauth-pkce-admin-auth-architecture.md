# OAuth + PKCE admin auth

The Control Plane embeds an OAuth 2.0 authorization server with mandatory PKCE.
It issues the bearer tokens the admin API runs on: a short-lived signed access
JWT and a rotating opaque refresh token. This doc covers the issuing side — the
authorization-code + PKCE flow, token minting, refresh rotation, and revocation.
The verifying side (how the access JWT is checked) is in
[jwt-verifier-architecture.md](jwt-verifier-architecture.md); the login methods
that authenticate the user mid-flow (local password, external OIDC) are in
[idp-sso-architecture.md](idp-sso-architecture.md); and what the access token's
subject and scope authorise is in
[iam-identity-architecture.md](iam-identity-architecture.md).

## Endpoints and discovery

The auth server mounts its endpoints at the Echo root, separate from the admin
API groups: `/oauth/authorize`, `/oauth/token`, `/oauth/introspect`,
`/oauth/revoke`, and `/oauth/device-binding`, plus `/.well-known/jwks.json` (the
RS256 public key set) and `/.well-known/openid-configuration`. The discovery
document advertises the authorization and token endpoints, the JWKS URI, the
supported grant types, the token-endpoint auth methods, and the supported PKCE
code-challenge methods.

## Authorization-code flow with PKCE

`/oauth/authorize` is the front door. It requires `response_type=code`, looks up
the client, and rejects a `redirect_uri` that is not registered for that client.
Because a bad redirect target is untrusted, an invalid `redirect_uri` is rendered
as an error by the auth server rather than reflected back via a redirect. The
request's `code_challenge` is captured with the challenge method restricted to
`S256` — the `plain` method is rejected unconditionally. The pending request is
stored server-side under a short-lived handle, and the browser is redirected to
the hosted login page carrying that handle.

After the user authenticates, the server mints a single-use authorization code
bound to the PKCE challenge, the client, the redirect URI, the resolved user, and
the device, scope, email, IdP, and authentication-method context. The code lives
in a single-use store: once consumed, any later presentation fails and the client
must restart at `/oauth/authorize`.

## Token endpoint

`/oauth/token` supports exactly two grant types — `authorization_code` and
`refresh_token`; any other grant returns `unsupported_grant_type`.

For `authorization_code`, the handler requires the code, the PKCE
`code_verifier`, the client id, and the redirect URI; it looks the code up in the
single-use store, checks the client id and redirect URI match, and verifies the
verifier against the stored challenge with `S256`. For the agent-desktop client
the access token is additionally bound to the mTLS peer certificate: the TLS
layer resolves the device, and the handler requires that device to match the one
the authorize step locked in. It then mints the refresh chain first (so the
access token can carry the new session id in its `sid` claim) and issues the
access token. For an agent device a `DeviceAssignment` is recorded
fire-and-forget so a write failure never blocks the token response.

For `refresh_token`, the handler rotates the incoming refresh token, re-loads the
user to honour a disabled account (a disabled user cannot extend a session by
rotating) and to refresh the email claim, and mints a new access token on the
same session. Both grants return the RFC 6749 success body with a `Bearer` token
type and set `Cache-Control: no-store`. Access tokens default to a one-hour
lifetime and refresh tokens to twenty-four hours.

## Access token issuance

An access token is an RS256 JWT signed by the keystore
(`packages/control-plane/internal/identity/authserver/token/`). The keystore owns
a set of 2048-bit RSA keys persisted as PEM files, each keyed by a `kid`; the
JWKS endpoint serves their public halves with `RS256` as the advertised
algorithm. The claim set is the registered claims (issuer, subject, audience,
issued-at, expiry, and a random `jti`) plus `client_id`, `scope`, the session id
`sid`, `device_id`, `email`, the issuing IdP `idp`, and the authentication
methods `amr`. The audience is the fixed resource-server identifier `cp-admin` —
it does not vary per client.

## Refresh rotation and replay detection

A refresh token is a 32-byte random opaque value; only its SHA-256 hash is
stored, never the raw token. Minting a chain allocates a fresh session id and a
root row; rotation marks the presented row used and inserts a successor linked by
parent id that inherits the session, user, client, and device. The used-flag flip
is atomic, so two concurrent rotations of the same token race and the loser is
treated as a replay.

Replay and expiry are distinguished. A token whose hash is unknown, or whose row
is already used, or that loses the rotation race, is a replay; an otherwise-valid
row past its expiry is an expiry. Both surface to the token endpoint as
`invalid_grant`, but a replay against a known row additionally fires a replay
hook wired to the session-revocation service — a replay is treated as a
compromise signal, so the whole refresh chain and its outstanding access tokens
for that session are torn down.

## Revocation and introspection

`/oauth/revoke` implements RFC 7009: the `token_type_hint` is advisory, and the
handler hashes the presented token the same way the refresh helper does to find
and mark the matching row used. `/oauth/introspect` reports token state.

Session revocation is owned by the revocation service
(`packages/control-plane/internal/identity/authserver/revocation/`). Its ordering
invariant is that the revocation row is persisted before the revocation event is
published to MQ, so a publish failure never leaves a revoked session unrecorded.
The event carries the target session id; the refresh replay hook publishes
through the same path so a detected compromise revokes the entire session.

## Client registration

`OAuthClient` is the admin-managed inventory of which third-party applications
may speak OAuth to the platform — what `client_id` values the auth server will
accept on `/oauth/authorize` and `/oauth/token`. The Control Plane exposes a
CRUD + rotate-secret admin API under `/api/admin/oauth-clients` so the inventory
is operable from the admin UI without psql; the seeded clients
(`web-console`, `agent-desktop`, `cp-ui`, the operator-toolkit TUI) co-exist
with admin-managed registrations in the same table.

A client is one of two `type` values. **Public** clients (browser SPAs, native
CLIs) authenticate by PKCE alone; the token endpoint rejects any presented
`client_secret` for them so a misconfigured caller fails loudly rather than
silently downgrading the security model. **Confidential** clients (server-side
backends) carry a `client_secret`; the token endpoint accepts it via the RFC
6749 §2.3.1 preferred `Authorization: Basic` header and falls through to the
`client_secret` form field. The handler bcrypt-compares against
`OAuthClient.clientSecretHash`; a missing or empty hash is treated as auth
failure rather than bypass. For confidential clients, client authentication
runs *before* the auth-code single-use store is touched, so a wrong secret
never burns a valid authorization code.

The plaintext secret is returned exactly once — on create and on each
rotate-secret call — and then unrecoverable. The admin UI surfaces it through
a hard-gated reveal dialog (an explicit acknowledgement checkbox must be
ticked before the modal can be closed); the API itself never echoes the
plaintext in any other response, audit row, or log line. Rotating does **not**
revoke active refresh tokens — they continue to grant access until they
expire — so the admin UI surfaces the live `activeRefreshTokenCount` in the
rotate-confirm dialog. Deleting a client cascades to its `RefreshToken` rows
via the FK `onDelete: Cascade`, immediately invalidating any session that was
issued under that registration.

The IAM resource is `oauth-client`, exposing `read`/`create`/`update`/`delete`
plus a dedicated `rotate` verb (a Service IAM `crud() + VerbRotate`). The
admin route group is mounted by
`iamBundle.RegisterOAuthClientRoutes(g, iamMW)` alongside the rest of the IAM
admin surface; every endpoint is `iamMW`-gated against the matching verb so a
holder of `oauth-client.read` cannot rotate, and vice versa.

## Device binding

`/oauth/device-binding` runs behind the agent mTLS middleware, so the peer
certificate is validated and the device resolved before the handler runs. It is
part of the agent enrollment path — see
[idp-sso-architecture.md](idp-sso-architecture.md) for agent SSO enrollment.

## References

- `packages/control-plane/internal/identity/authserver/mount.go` — endpoint mounting
- `packages/control-plane/internal/identity/authserver/oauth/` — authorize, token, introspect, revoke, device-binding, PKCE, JWKS, discovery
- `packages/control-plane/internal/identity/authserver/token/` — RS256 keystore, signer, access-token claims, refresh rotation
- `packages/control-plane/internal/identity/authserver/store/` — auth-code, refresh, pending-authz, client stores
- `packages/control-plane/internal/identity/users/handler/oauth_client.go` — admin CRUD + rotate-secret handler
- `packages/control-plane/internal/identity/authserver/revocation/` — session revocation service + MQ publisher
- `packages/shared/identity/pkce/` — S256 verifier
- `packages/control-plane/cmd/control-plane/wiring/authserver.go` — auth-server wiring
