---
doc: jwt-verifier-architecture
area: service
service: control-plane
tier: 1
---

# JWT Verifier Architecture

> **Tier 2 architecture doc.** Read when touching `packages/control-plane/internal/identity/jwt/`. Used by both the local OAuth+PKCE AS (verifying its own bearer tokens) and the external-IdP federation flow (verifying IdP ID tokens). Sister docs: `oauth-pkce-admin-auth-architecture.md`, `idp-sso-architecture.md`.

The verifier is the single seam for JWT validation in Nexus. Every admin API call routes through it. Drift in this code path produces silent-403 incidents or, worse, accepted forgeries.

---

## 1. The verifier surface

```go
type Config struct {
    Issuer    string
    JWKSURL   string
    Audience  string
    ClockSkew time.Duration     // default 5 min when zero
    RevCheck  RevocationChecker // default AlwaysAllow when nil
    Logger    *slog.Logger
}

type Verifier struct{ /* unexported */ }

func New(cfg Config) *Verifier

func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error)
```

A `Verifier` is a concrete struct (not an interface) and is **single-issuer per instance**: each instance carries one `Issuer` + one `JWKSURL` + one `Audience`. There is no `AddIssuer` / `RemoveIssuer` API. Multi-IdP federation — e.g. one Okta tenant plus one Azure AD tenant plus the local AS — uses **multiple Verifier instances** keyed by the inbound token's `iss`, or a thin router upstream of the verify call.

The Go package name is `jwtverifier`; the directory is `packages/control-plane/internal/identity/jwt/`.

## 2. Validation order

`Verify` runs each check in order; the first failure returns a typed sentinel error from `errors.go` (`ErrMalformed`, `ErrExpired`, `ErrNotYetValid`, `ErrInvalidSignature`, `ErrWrongIssuer`, `ErrWrongAudience`, `ErrRevoked`, `ErrJWKSUnavailable`).

1. **Algorithm + JWT shape** — `jwt.Parse(...)` is invoked with `jwt.WithValidMethods([]string{"RS256"})` and `jwt.WithLeeway(cfg.ClockSkew)`. Only **RS256** is accepted. `alg: none`, `alg: HS256` (HMAC-public-key forgery), `ES256`, and every other algorithm are rejected at parse time.
2. **Signature** — verified against the JWKS-published RSA public key matching the token's `kid` (see §3).
3. **Time window** — `iat`, `nbf`, `exp` validated by jwt/v5 with the configured leeway. Default clock skew when `cfg.ClockSkew == 0` is **5 minutes** (`verifier.go:38-39`).
4. **Issuer match** — `c.Issuer == cfg.Issuer` (exact equality; the Verifier is single-issuer). Mismatch → `ErrWrongIssuer`.
5. **Audience match** — `cfg.Audience` must appear in the token's `aud` list. `cfg.Audience` has **no default** — callers must provide it explicitly; an empty string would silently match an empty `aud`, so misconfiguration is caught at wiring time.
6. **Ghost-principal defence** — a structurally valid token with empty `sub` is rejected (`ErrMalformed`). An empty `sub` would propagate `""` into every downstream IAM/DB lookup as a phantom principal id; the verifier is the trust boundary, so this is enforced here rather than trusted to the minter.
7. **Revocation** — `cfg.RevCheck.IsRevoked(ctx, c)` is called with the parsed claims (and `c.Raw` populated so introspection-based checkers can forward the compact JWT). When `cfg.RevCheck == nil`, `New` substitutes `AlwaysAllow{}` — revocation is opt-in and must be wired explicitly in production.

Failure at any step returns the corresponding sentinel; the calling middleware translates to 401 with a non-leaky `WWW-Authenticate` reason.

## 3. JWKS cache

`packages/control-plane/internal/identity/jwt/jwks.go`. The cache is **lazy + singleflight-coalesced + stale-while-revalidate**:

- **TTL** — `defaultJWKSTTL = 15 * time.Minute` (`jwks.go:23`).
- **HTTP timeout** — `defaultJWKSHTTPTimeout = 5 * time.Second`.
- **Coalescing** — concurrent refreshes share one upstream call via `golang.org/x/sync/singleflight`.
- **Stale-while-revalidate** — within TTL, `KeyByKID` serves from memory; past TTL, the next caller triggers a refresh. On transient refresh failure, the previous snapshot is returned (so a brief IdP outage does not break in-flight verifies).
- **Cold miss on a kid** — a token signed with a `kid` not in the current snapshot triggers a refresh + retry, handling short JWKS rotations gracefully.
- **Both fresh and stale miss the kid** — `ErrJWKSUnavailable` is returned and the middleware surfaces 401.

## 4. Caching the verification result

Verifying a JWT is cheap (RS256 sig check is microseconds), but pulling the JWKS over HTTP is expensive. The JWKS cache is the only cache here — verification results themselves are not cached (would invalidate revocation effects).

## 5. Multi-issuer / multi-IdP

A single `Verifier` is single-issuer. Federating multiple IdPs means instantiating one `Verifier` per IdP — for example:

- A `Verifier` for `https://okta.acme.com/` against Okta's JWKS.
- A `Verifier` for `https://login.microsoftonline.com/<tenant>/` against Azure AD's JWKS.
- A `Verifier` for `https://nexus.<tenant>/` against the local AS's JWKS.

A small router (today's federation layer in `identity/authserver/login/oidc.go` + the IdP store) looks up the right Verifier from the inbound token's `iss` before calling `Verify`. If the IdP rotates keys, that Verifier's JWKS polling picks up the new keys within the TTL window (per `oauth-pkce-admin-auth-architecture.md` §8).

## 6. Revocation

`RevocationChecker` is an interface in `packages/control-plane/internal/identity/jwt/revocation.go`:

```go
type RevocationChecker interface {
    IsRevoked(ctx context.Context, c *Claims) (bool, error)
}
```

The production implementation is `MQRevocationChecker` (`mqrevocation.go`): it consumes revocation events published by `internal/identity/authserver/revocation/` on the MQ stream, maintains an in-memory bloom filter for revoked `jti`s plus per-subject, per-device, and per-session sets, and falls back to a direct `/oauth/introspect` call on bloom false positives and in strict mode. **This is not a Redis SISMEMBER lookup** — Redis is not in the verify path.

The default `AlwaysAllow{}` is used only in tests and harnesses that don't wire MQ; production wiring in `cmd/control-plane/` always installs `MQRevocationChecker`.

## 7. Service-to-service auth surfaces

The JWT verifier in this package is admin-auth-only. Service-to-service authentication uses two different surfaces:

- **Internal CP endpoints** (`/api/internal/*`) — guarded by `rstokenauth.Middleware(cfg.Auth.InternalServiceToken)` (`packages/control-plane/cmd/control-plane/wiring/routes.go:179`). The token is a shared secret distributed via environment to every service that calls these endpoints.
- **Agent → CP runtime** — `middleware.AgentMTLSAuth` (`packages/control-plane/internal/platform/middleware/agentmtls.go:32`) resolves the agent identity from an mTLS client cert via a `ThingNodeLookup` seam.

The agent's enrollment path uses an opaque enrollment token (not a JWT) verified against the Hub's enrollment-token table directly. See `service-call-framework.md` for the canonical service-to-service auth map.

## 8. Forgery defences

- **Algorithm pinning** — only `RS256` accepted (`verifier.go:54`). `alg: none`, `HS256` (HMAC-with-public-key attack), `ES256`, and every other algorithm are rejected.
- **`kid` required** — a token with no matching `kid` in JWKS fails verification.
- **`iss` strict matching** — `iss` must equal the configured `Issuer`; the verifier never looks up the JWKS URL from the token's own `.well-known` discovery (some implementations do — Nexus does NOT; `JWKSURL` is admin-configured per Verifier).
- **`aud` mandatory** — no default; must be provided in `Config.Audience`.
- **Ghost-principal rejection** — empty `sub` is `ErrMalformed`.

The "alg: none" + "alg: HS256 with public key as HMAC secret" classic forgery attacks are blocked by algorithm pinning.

## 9. Sources

- `packages/control-plane/internal/identity/jwt/verifier.go` — `New`, `Verify`, validation order.
- `packages/control-plane/internal/identity/jwt/jwks.go` — JWKS cache (15-min TTL, singleflight, stale-while-revalidate).
- `packages/control-plane/internal/identity/jwt/revocation.go` + `mqrevocation.go` — revocation interface + MQ-backed implementation.
- `packages/control-plane/internal/identity/jwt/claims.go`, `errors.go` — sentinel error set + claim shape.
- `packages/shared/identity/rstokenauth/` — token issue side (cross-references the same JWKS).
- `packages/control-plane/internal/identity/authserver/` — local AS that produces tokens this verifier accepts.

<!-- 💡 harvest: algorithm-pinning (RS256 only) is binding. Could be a Cursor rule for any new JWT-related code. -->

## 10. Cross-references

- `oauth-pkce-admin-auth-architecture.md` — local AS that issues tokens this verifies.
- `idp-sso-architecture.md` — external IdPs that issue tokens this verifies.
- `iam-identity-architecture.md` — what claims downstream IAM uses.
- `service-call-framework.md` — mTLS-based service-to-service auth (distinct from JWT).
