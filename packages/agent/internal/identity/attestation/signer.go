// Package attestation implements the agent-side traffic attestation signer.
// The signer holds the per-agent Ed25519 private key (issued by Nexus Hub
// at enrollment, held in the platform keystore — macOS Keychain / Windows
// DPAPI / Linux 0600 file — NOT a plaintext PEM on disk) and
// writes the X-Nexus-Attestation header value on every outbound CONNECT
// to the compliance-proxy.
//
// Architecture: docs/developers/architecture/services/agent/agent-attestation-architecture.md
// Wire format: packages/shared/transport/tlsbump/attestation.go
//
// Fail-open contract: every Sign() error path causes the caller to OMIT the
// header — the request still flows to CP through the normal MITM path. Never
// block the request because of an attestation problem.
package attestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// signerRandReader is the entropy source the signer uses for nonce
// generation. Production never reassigns; tests substitute a failing
// reader to exercise the entropy-error fallback branches without
// touching crypto/rand. Same package-seam pattern as
// packages/shared/identity/pkce and agentca.
var signerRandReader = rand.Reader

// Signer writes the X-Nexus-Attestation header for outbound CONNECTs.
// One instance is shared across the whole agent process; it is safe
// for concurrent use.
//
// The signer reads the agent's Ed25519 private key from the platform
// keystore lazily the first time Sign is called, then caches the key in
// memory. The fleet-default "attestationEnabled" toggle is read fresh on
// every Sign call (the agent's applied-config snapshot may flip the value
// at any time), so disabling attestation in CP UI takes effect on the
// next outbound request.
type Signer struct {
	store         keystore.Store // platform keystore the key is held in
	keyName       string         // keystore label, e.g. keystore.AttestationKeyName
	agentID       string         // immutable for the agent's lifetime
	enabledLookup func() bool    // returns the current AttestationEnabled toggle
	logger        *slog.Logger
	failedWarnAt  atomic.Int64 // unix-second of last fail-open warn (rate limiter)

	mu        sync.RWMutex
	cachedKey ed25519.PrivateKey
}

// NewSigner constructs a Signer. store is the platform keystore holding
// the agent's Ed25519 attestation private key (written by enrollment under
// keyName, typically keystore.AttestationKeyName) — the key lives in the
// OS-protected store, never a plaintext on-disk PEM.
// agentID is the Hub-assigned Thing UUID; enabledLookup returns the
// current value of the fleet attestationEnabled flag from
// AppliedConfig.DeviceDefaults — read on every Sign call so admin
// changes propagate without restarting the agent.
//
// A logger is required: signer-failure paths emit structured warns so
// operators can see attestation regressing without affecting traffic.
func NewSigner(store keystore.Store, keyName, agentID string, enabledLookup func() bool, logger *slog.Logger) *Signer {
	return &Signer{
		store:         store,
		keyName:       keyName,
		agentID:       agentID,
		enabledLookup: enabledLookup,
		logger:        logger,
	}
}

// ErrAttestationDisabled is returned by Sign when the fleet toggle is
// off. Callers translate this to "omit the header" — there is no error
// log, no metric, no anomaly. Pure absence-of-header.
var ErrAttestationDisabled = errors.New("attestation: disabled by fleet config")

// ErrAttestationNotEnrolled is returned by Sign when the agent has no
// Ed25519 key in the keystore yet. Callers translate to "omit the header";
// the next enrollment refresh will issue the key.
var ErrAttestationNotEnrolled = errors.New("attestation: no Ed25519 key in keystore")

// Sign returns the wire-format X-Nexus-Attestation header value
// committing to the empty body (sha256("")). Use SignForBody when
// the actual inner HTTP request body is known — the header rides on
// the inner request and CP computes the same hash post-bump for
// strict-mode verification (v2). v1 CP default mode does not verify
// the hash field but the agent still signs over the real body when it
// has it, so the audit trail is internally consistent.
//
// On any error the caller MUST omit the header — fail-open is the
// architecture contract. The signer rate-limits failure logs to one
// per minute so a missing key file does not flood the agent log.
func (s *Signer) Sign() (string, error) {
	return s.signOver(nil)
}

// SignForBody returns the wire-format header value committing to
// `sha256(body)`. Preferred over Sign for outbound HTTPS requests
// where the inner body is buffered. body may be nil or empty — both
// produce the canonical empty-body hash.
func (s *Signer) SignForBody(body []byte) (string, error) {
	return s.signOver(body)
}

func (s *Signer) signOver(body []byte) (string, error) {
	if s == nil {
		return "", errors.New("attestation: nil signer")
	}
	if s.enabledLookup == nil || !s.enabledLookup() {
		return "", ErrAttestationDisabled
	}
	if s.agentID == "" {
		return "", errors.New("attestation: empty agent_id")
	}

	key, err := s.loadKey()
	if err != nil {
		s.logFailure("attestation key load failed", err)
		return "", err
	}

	nonceBytes := make([]byte, 16)
	if _, err := signerRandReader.Read(nonceBytes); err != nil {
		s.logFailure("attestation nonce generation failed", err)
		return "", fmt.Errorf("attestation: nonce: %w", err)
	}

	hashField := tlsbump.HashEmptyBody()
	if len(body) > 0 {
		hashField = tlsbump.HashBody(body)
	}

	fields := tlsbump.AttestationFields{
		Version: tlsbump.AttestationHeaderVersion,
		TS:      time.Now().Unix(),
		Nonce:   hex.EncodeToString(nonceBytes),
		Hash:    hashField,
		AgentID: s.agentID,
	}
	sig := ed25519.Sign(key, fields.SignatureInput())
	fields.Signature = base64.RawURLEncoding.EncodeToString(sig)
	return fields.FormatHeader(), nil
}

// loadKey returns the cached Ed25519 private key, lazily loading from the
// platform keystore on the first call. The keystore read does not happen
// under any external lock so a slow keystore backend cannot wedge the
// request path; the key is cached after the first successful load.
func (s *Signer) loadKey() (ed25519.PrivateKey, error) {
	s.mu.RLock()
	if s.cachedKey != nil {
		key := s.cachedKey
		s.mu.RUnlock()
		return key, nil
	}
	s.mu.RUnlock()

	if s.store == nil {
		return nil, ErrAttestationNotEnrolled
	}
	raw, err := s.store.Get(s.keyName)
	if err != nil {
		return nil, fmt.Errorf("attestation: keystore get: %w", err)
	}
	if raw == nil {
		// Not-found is the "attestation not available yet" signal —
		// the keystore Get contract returns nil for a missing key.
		return nil, ErrAttestationNotEnrolled
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("attestation: key PEM decode failed")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("attestation: PKCS8 parse: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("attestation: key is not Ed25519 (got %T)", parsed)
	}

	s.mu.Lock()
	s.cachedKey = key
	s.mu.Unlock()
	return key, nil
}

// InvalidateCachedKey forces the next Sign call to re-read the key
// from the keystore. Used by the enrollment refresh path after a cert
// rotation — without this the agent would keep signing with the
// previous key until restart.
func (s *Signer) InvalidateCachedKey() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cachedKey = nil
	s.mu.Unlock()
}

// logFailure emits a structured warn no more than once every 60 s so
// a missing key or a broken keystore backend cannot drown the agent
// log. ErrAttestationDisabled and ErrAttestationNotEnrolled are
// always-quiet paths handled at the caller; this method covers the
// genuinely-unexpected branches.
func (s *Signer) logFailure(msg string, err error) {
	if s.logger == nil {
		return
	}
	if errors.Is(err, ErrAttestationDisabled) || errors.Is(err, ErrAttestationNotEnrolled) {
		return
	}
	now := time.Now().Unix()
	prev := s.failedWarnAt.Load()
	if now-prev < 60 {
		return
	}
	if s.failedWarnAt.CompareAndSwap(prev, now) {
		s.logger.Warn(msg, "error", err)
	}
}

// MarshalEd25519PrivateKeyPEM serialises a freshly-generated Ed25519
// private key for keystore persistence. Used by the enrollment manager
// after Hub returns a signed Ed25519 cert — we keep the key in the
// same PKCS8 PEM shape Go's x509 stdlib reads back without custom
// parsing (the keystore stores these PEM bytes as the secret value).
// Public function so the enrollment path can call it without depending
// on internals.
func MarshalEd25519PrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal Ed25519 private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// HeaderPair is a small convenience type returned by SignHeader for
// callers that need both the canonical header name + value in one
// shot (e.g. http.Transport.GetProxyConnectHeader).
type HeaderPair struct {
	Name  string
	Value string
}

// SignHeader is a thin wrapper around Sign that returns (name, value)
// suitable for stamping into an http.Header. On error the second
// return is empty — callers MUST NOT add a header in that case.
func (s *Signer) SignHeader() (HeaderPair, error) {
	v, err := s.Sign()
	if err != nil {
		return HeaderPair{}, err
	}
	return HeaderPair{Name: tlsbump.AttestationHeaderName, Value: v}, nil
}

// EnabledLookupFromString returns a no-op closure when label is empty.
// Mainly a helper for tests; production calls NewSigner with a real
// AppliedConfig.DeviceDefaults.AttestationEnabled getter.
func EnabledLookupFromString(label string) func() bool {
	if strings.EqualFold(label, "always") {
		return func() bool { return true }
	}
	return func() bool { return false }
}

// InjectInto stamps the wire-format X-Nexus-Attestation header onto
// the given outbound HTTPS request. Designed to plug into
// tlsbump.UpstreamOptions.RequestInjector so the agent can stamp the
// header on every outbound inner-request without knowing anything about
// CP — CP detects the header after its own transparent TLS bump. Agent
// is unaware of CP topology; CP is the detector, not a routed-through
// hop.
//
// Body handling: the injector consumes req.Body fully into memory,
// hashes it, and rewraps the request with a bytes.Reader so the
// downstream wire send sees identical bytes. Honors GetBody-style
// rewinding by reinstating GetBody to return a fresh reader.
// Fail-open: ANY error (read failure, sign failure, disabled,
// missing key) returns nil so the surrounding ForwardRequest
// swallows it. The request still forwards, just without the
// attestation header; CP that receives a request without a valid
// header runs its normal MITM pipeline.
//
// Streaming bodies: a request whose body is unbounded (e.g.,
// long-lived SSE stream from a client doing chunked upload) is
// signed with the empty-body hash, because the injector cannot
// know the body without consuming it. Future strict_mode v2 will
// re-anchor; v1 CP default mode does not verify the hash field.
func (s *Signer) InjectInto(req *http.Request) error {
	if s == nil || req == nil {
		return nil
	}

	var bodyBytes []byte
	// A streaming (unknown-length, ContentLength < 0) request body must NOT be
	// consumed here. A connect-RPC / gRPC bidi client holds its request stream
	// open — sending more only after it reads server responses — so reading the
	// body to EOF to hash it deadlocks exactly like buffering would (and would
	// hand the upstream a drained body). Sign the empty-body hash and leave
	// req.Body untouched for the streaming relay. CP v1 default mode does not
	// verify the hash field, so this degrades cleanly. Known-length bodies are
	// safe to buffer-and-hash (the client commits to sending exactly N bytes).
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength >= 0 {
		// Bound body reads to avoid OOM on a runaway client. AI
		// traffic bodies are typically <1 MiB; cap at 8 MiB so a
		// pathological client can't make the injector spike memory.
		const maxBodyForHash = 8 * 1024 * 1024
		buf, err := io.ReadAll(io.LimitReader(req.Body, maxBodyForHash+1))
		_ = req.Body.Close()
		switch {
		case err != nil:
			// Treat read failure as "no body to hash" and continue
			// with empty-body hash. Fail-open: never block the
			// request because hashing failed.
			bodyBytes = nil
		case len(buf) > maxBodyForHash:
			// Body exceeded the cap — we read past the limit. Treat
			// as a streaming body (sign empty-body hash) and rewrap
			// the bytes we did consume + the rest of the original.
			// The cap is conservative; AI request bodies rarely hit
			// it. Tagged for follow-up to handle large bodies via a
			// streaming-hash io.TeeReader if real traffic shows the
			// limit fires.
			bodyBytes = nil
			// Rewrap so the wire send still gets the full body.
			// io.MultiReader rejoins the buffered prefix with the
			// remaining stream.
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), req.Body))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(buf)), nil
			}
		default:
			// Normal path: body fully buffered + hashable.
			bodyBytes = buf
			req.Body = io.NopCloser(bytes.NewReader(buf))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(buf)), nil
			}
		}
	}

	value, err := s.SignForBody(bodyBytes)
	if err != nil {
		// Fail-open: a body-signing failure must never block the request;
		// proceed without the attestation header (CP MITMs normally).
		return nil //nolint:nilerr // intentional fail-open, see func doc
	}
	req.Header.Set(tlsbump.AttestationHeaderName, value)
	return nil
}

// GetProxyConnectHeader is the legacy adapter for Go's stdlib
// http.Transport.GetProxyConnectHeader callback — kept available
// for callers that route through an explicit HTTP proxy. The agent
// uses InjectInto on the inner request instead; CP no longer relies
// on the CONNECT-line variant.
//
// Fail-open contract (load-bearing): every signing failure path
// returns (nil, nil) — never a non-nil error. Returning a non-nil
// error from this callback aborts the request at the stdlib layer
// (the customer sees a transport failure); attestation is a perf
// optimization, never a security gate — any signer problem must
// degrade gracefully to "no header, CP MITMs normally".
//
// The proxyURL + target arguments are ignored by this v1 signer
// (the header doesn't bind to the CP URL or the dest host; the agent
// signs the canonical pre-image which already includes ts/nonce/
// hash/agent_id). They're part of the callback signature so a future
// strict-mode v2 can include destination-binding fields.
func (s *Signer) GetProxyConnectHeader(_ context.Context, _ *url.URL, _ string) (http.Header, error) {
	if s == nil {
		return nil, nil
	}
	pair, err := s.SignHeader()
	if err != nil {
		// Fail-open: omit the header. CP sees a regular CONNECT
		// with no attestation → runs its usual MITM path. The
		// signer already rate-limits its warn-logs so a missing
		// key file doesn't flood the daemon log.
		return nil, nil //nolint:nilerr // intentional fail-open, see func doc
	}
	return http.Header{pair.Name: []string{pair.Value}}, nil
}
