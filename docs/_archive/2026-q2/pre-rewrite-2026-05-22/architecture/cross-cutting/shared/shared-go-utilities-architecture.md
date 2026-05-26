---
doc: shared-go-utilities-architecture
area: cross-cutting
service: shared
tier: 1
updated: 2026-05-20
---

# `packages/shared/*` Go Utility Libraries

> **Tier 3 architecture doc.** A combined reference for five small `packages/shared/*` libraries: `identity/pkce`, `identity/rstokenauth`, `transport/responseio`, `transport/http`, `core/logging` (paired with the `core/diag/slog_sink.go` handler). Each is small enough that a separate doc is overkill; this doc gives each a section.

---

## 1. `shared/identity/pkce`

PKCE primitives consumed by `oauth-pkce-admin-auth-architecture.md` flow.

```go
func GenerateVerifier() (string, error)              // 43-128 random chars
func ChallengeFromVerifier(verifier string) string   // base64url(sha256(verifier))
func VerifyChallenge(verifier, challenge string) bool
```

All primitives are stateless and constant-time where security-relevant.

**Used by:** Control Plane authserver. Agent SSO flow.

## 2. `shared/identity/rstokenauth`

RS256 token issue / verify on top of `crypto/rsa`. Backs the JWT issued by the local OAuth+PKCE AS and the JWT verifier.

```go
func IssueToken(privateKey *rsa.PrivateKey, claims Claims, kid string) (string, error)
func VerifyToken(token string, jwks JWKS) (*Claims, error)
type JWKS interface { LookupKey(kid string) (*rsa.PublicKey, error) }
```

Supports multi-kid JWKS for key rotation (`oauth-pkce-admin-auth-architecture.md` §8).

**Used by:** Control Plane authserver + jwtverifier. Hub for service-Thing tokens (rare; mTLS is the primary path).

## 3. `shared/transport/responseio`

JSON / SSE / error response writers for Go HTTP handlers. Keeps response shape consistent across services.

```go
func WriteJSON(w http.ResponseWriter, status int, body interface{})
func WriteSSE(w http.ResponseWriter, event string, data interface{})
func WriteError(w http.ResponseWriter, status int, code, message string)
func StreamSSE(ctx context.Context, w http.ResponseWriter, source <-chan SSEEvent) error
```

Error shape:

```json
{ "error": { "code": "<canonical-code>", "type": "<class>", "message": "..." } }
```

Matches the contract expected by the admin UI and the `/v1/*` clients. Cross-ref `error-taxonomy-architecture.md`.

**Used by:** AI Gateway. Control Plane. Hub. Compliance Proxy.

## 4. `shared/transport/http`

Telemetry-aware HTTP client + helpers. Wraps `net/http` with:

- OTel `traceparent` injection (cross-ref `otel-pipeline-architecture.md` §3).
- Retry with exponential backoff per `ErrorClass` policy.
- Circuit breaker (cross-ref `error-taxonomy-architecture.md` §5).
- Outbound log fields (`outbound http` debug log).
- Per-request timeout.

```go
func New(logger *slog.Logger, opts ...Option) *Client

c.Do(ctx, req)                  // returns canonical response
c.Get(ctx, url, opts ...Option) // convenience
c.Post(ctx, url, body, opts ...Option)
```

Service code never imports `net/http` directly for outbound calls; it goes through `transport/http`.

**Used by:** All services for outbound calls (provider HTTPS, Hub HTTP, SIEM webhook, etc.).

## 5. `shared/core/logging` + the slog sink in `shared/core/diag/`

Structured logging on top of `log/slog`.

`core/logging`:

```go
func New(cfg Config) *slog.Logger
// cfg includes level, format (json/text), file path (tee), stack-on-error
```

The diag pipeline sink lives **inside** `core/diag/` (file `slog_sink.go`) — it is **not** a separate top-level subpackage. The sink is a custom `slog.Handler` that fans logs to:

- stdout (always).
- on-disk file (per cfg).
- the diag pipeline (cross-ref `diag-event-triage-architecture.md` §2).

The DI invariant: after wiring `SlogSink + slog.SetDefault(...)`, services MUST reassign module-scope `logger = slog.Default()` so DI-injected loggers reflect the diag pipeline. Memory: `feedback_server_slog_sink_di_bypass`.

**Used by:** all services. The DI pattern is binding (cross-ref `.cursor/rules/go-conventions.mdc`).

## 6. Cross-references

- `oauth-pkce-admin-auth-architecture.md` — `identity/pkce` + `identity/rstokenauth` consumer.
- `jwt-verifier-architecture.md` — `identity/rstokenauth` consumer.
- `otel-pipeline-architecture.md` — `transport/http` `traceparent` injection.
- `error-taxonomy-architecture.md` — `transport/http` retry + circuit breaker; `transport/responseio` error shape.
- `diag-event-triage-architecture.md` — `core/diag/slog_sink.go` diag pipeline.
- `shared-package-architecture.md` — package catalogue (8-bucket structure).
