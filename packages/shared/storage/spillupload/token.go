// Package spillupload provides the HMAC-token machinery and the
// Redis-backed dedup primitive that gate the Hub-issued one-shot
// upload URLs.
//
// The token is a JWT-like compact string `<base64url(payload)>.<base64url(sig)>`
// where `payload` is a JSON object pinning the upload's expected scope
// (eventId, direction, key, size, sha256) and expiry, and `sig` is an
// HMAC-SHA256 over the payload bytes using a server-side rotating
// secret. The mint endpoint signs; the blob endpoint verifies; nothing
// else needs to talk to this package directly.
package spillupload

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"
)

// MaxTTL caps the wall-clock lifetime of any spill upload token. Five
// minutes is comfortable for a single-shot upload (a 256 MiB body at a
// modest 10 MB/s still completes in ~30 s) and short enough that a
// leaked token has minimal blast radius.
const MaxTTL = 5 * time.Minute

// DedupTTL is the lifetime of the Redis dedup record stamped after a
// blob endpoint accepts a token. Twice MaxTTL so a tardy retry that
// arrives just before token expiry still finds the record.
const DedupTTL = 2 * MaxTTL

// Direction values must mirror the audit pipeline's request/response
// split. The blob endpoint uses these to scope dedup keys and the mint
// endpoint refuses anything else with 400.
const (
	DirectionRequest  = "request"
	DirectionResponse = "response"
)

// ErrTokenInvalid covers both decode failures and signature mismatches.
// The blob endpoint maps it to HTTP 401 / 400 per the spec; callers
// outside Hub should treat it as "this token is unusable, drop the
// upload and fall back to inline-truncated capture".
var (
	ErrTokenInvalid = errors.New("spillupload: token invalid")
	ErrTokenExpired = errors.New("spillupload: token expired")
	ErrUnknownKID   = errors.New("spillupload: unknown signing-secret epoch")
)

// Claims is the JSON shape carried in the token payload. Keys are
// short to keep tokens compact (URL length matters for HTTP intermediaries
// that cap header size or that log full URLs). The wire is JSON so a
// future field addition is a one-line change instead of a binary
// version bump.
type Claims struct {
	KID       string `json:"kid"`               // signing-secret epoch
	EventID   string `json:"eid"`               // traffic_event id
	Direction string `json:"dir"`               // request | response
	Key       string `json:"key"`               // backend storage key
	SizeBytes int64  `json:"sz"`                // exact upload size in bytes
	SHA256    string `json:"h"`                 // lowercase hex sha256
	ExpiresAt int64  `json:"exp"`               // unix seconds
	Backend   string `json:"backend,omitempty"` // "localfs" | "s3"
	Mime      string `json:"mime,omitempty"`    // optional contentType hint
	// NodeID is the authenticated device (thing) identity that minted this
	// token, bound into the HMAC signature. The mint namespaces the
	// storage Key by NodeID so one node can never address another node's spill
	// object; the blob handler writes to the signed Key, so a node cannot tamper
	// with the destination. Empty for trusted service-token mints (flat key).
	NodeID string `json:"nid,omitempty"`
}

// Validate checks structural invariants the token signing/verification
// path cannot enforce on its own (size > 0, sha256 64-hex chars, etc.).
// Returns nil if the claims are well-formed.
func (c *Claims) Validate() error {
	if c.EventID == "" {
		return fmt.Errorf("%w: missing eventId", ErrTokenInvalid)
	}
	if c.Direction != DirectionRequest && c.Direction != DirectionResponse {
		return fmt.Errorf("%w: direction must be request|response", ErrTokenInvalid)
	}
	if c.Key == "" {
		return fmt.Errorf("%w: missing key", ErrTokenInvalid)
	}
	if c.SizeBytes <= 0 {
		return fmt.Errorf("%w: sizeBytes must be > 0", ErrTokenInvalid)
	}
	if len(c.SHA256) != 64 {
		return fmt.Errorf("%w: sha256 must be 64 lowercase hex chars", ErrTokenInvalid)
	}
	for _, b := range []byte(c.SHA256) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return fmt.Errorf("%w: sha256 must be lowercase hex", ErrTokenInvalid)
		}
	}
	if c.ExpiresAt <= 0 {
		return fmt.Errorf("%w: missing expiry", ErrTokenInvalid)
	}
	return nil
}

// SecretSource returns the active signing secret (kid + raw bytes) at
// mint time and resolves an arbitrary kid to its raw secret at verify
// time. Implementations are responsible for rotation; the caller does
// not see kids it has never registered.
type SecretSource interface {
	// Active returns the kid + secret bytes used to sign new tokens.
	Active() (kid string, secret []byte, err error)
	// Lookup returns the secret bytes for a previously-issued kid, or
	// ErrUnknownKID if the kid is no longer in the rotation map.
	Lookup(kid string) (secret []byte, err error)
}

// Sign produces a compact `<payload>.<sig>` token from the supplied
// claims using the active secret. Claims.KID and Claims.ExpiresAt are
// stamped by Sign so the caller does not need to pre-fill them — the
// final, signed claims are returned alongside the token so the caller
// can echo expiresAt onto the mint response without re-deriving it.
// Everything else (eventId, direction, key, sizeBytes, sha256) must
// be set on `claims` and pass Validate.
func Sign(src SecretSource, claims Claims, ttl time.Duration) (string, Claims, error) {
	if ttl <= 0 || ttl > MaxTTL {
		ttl = MaxTTL
	}
	kid, secret, err := src.Active()
	if err != nil {
		return "", Claims{}, fmt.Errorf("spillupload: load active secret: %w", err)
	}
	claims.KID = kid
	claims.ExpiresAt = time.Now().Add(ttl).Unix()
	if err := claims.Validate(); err != nil {
		return "", Claims{}, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("spillupload: encode claims: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	sig := mac.Sum(nil)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
	return token, claims, nil
}

// Verify decodes and authenticates the supplied token. On success the
// parsed claims are returned; on failure the returned error wraps one
// of ErrTokenInvalid / ErrTokenExpired / ErrUnknownKID so callers can
// map to the right HTTP status.
//
// `now` is supplied explicitly so unit tests can pin a clock. Production
// callers pass time.Now().
func Verify(src SecretSource, token string, now time.Time) (Claims, error) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return Claims{}, fmt.Errorf("%w: malformed token", ErrTokenInvalid)
	}
	payloadPart := token[:dot]
	sigPart := token[dot+1:]
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decode payload: %w", ErrTokenInvalid, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decode sig: %w", ErrTokenInvalid, err)
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: parse payload: %w", ErrTokenInvalid, err)
	}
	if claims.KID == "" {
		return Claims{}, fmt.Errorf("%w: missing kid", ErrTokenInvalid)
	}
	secret, err := src.Lookup(claims.KID)
	if err != nil {
		if errors.Is(err, ErrUnknownKID) {
			return Claims{}, err
		}
		return Claims{}, fmt.Errorf("spillupload: lookup secret: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	want := mac.Sum(nil)
	if !hmac.Equal(want, sig) {
		return Claims{}, fmt.Errorf("%w: signature mismatch", ErrTokenInvalid)
	}
	if claims.ExpiresAt < now.Unix() {
		return Claims{}, ErrTokenExpired
	}
	if err := claims.Validate(); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// GenerateSecret returns 32 cryptographically-random bytes suitable for
// HMAC-SHA256. Used by the SecretStore bootstrap to seed an initial
// `epoch-1` secret on first Hub boot.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("spillupload: random read: %w", err)
	}
	return b, nil
}
