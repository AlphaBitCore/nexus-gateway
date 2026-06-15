package attestation

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeKey generates a fresh Ed25519 private key, stores it (PKCS8 PEM, the
// shape the signer expects) in an in-memory keystore under
// keystore.AttestationKeyName, and returns (store, public-key). Tests use
// the in-memory store so they never touch the real platform Keychain/DPAPI
// (SEC-M4-02 moved the attestation key off disk into the keystore).
func writeKey(t *testing.T) (keystore.Store, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pemBytes, err := MarshalEd25519PrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return storeWith(t, pemBytes), pub
}

// storeWith returns an in-memory keystore holding raw under the attestation
// key label — used to plant malformed / wrong-algorithm key material.
func storeWith(t *testing.T, raw []byte) keystore.Store {
	t.Helper()
	st := keystore.NewMemoryStore()
	if err := st.Set(keystore.AttestationKeyName, raw); err != nil {
		t.Fatalf("store set: %v", err)
	}
	return st
}

// errGetStore is a keystore whose Get always errors — used to drive the
// signer's keystore-error path (distinct from the not-found path, which
// returns ErrAttestationNotEnrolled).
type errGetStore struct{}

func (errGetStore) Get(string) ([]byte, error) { return nil, errors.New("keystore unavailable") }
func (errGetStore) Set(string, []byte) error   { return nil }
func (errGetStore) Delete(string) error        { return nil }

func TestSigner_HappyPath_HeaderVerifies(t *testing.T) {
	st, pub := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "550e8400-e29b-41d4-a716-446655440000",
		func() bool { return true }, newTestLogger())

	hdr, err := s.Sign()
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parsed, err := tlsbump.ParseAttestationHeader(hdr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.AgentID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("AgentID = %q", parsed.AgentID)
	}
	if parsed.Hash != tlsbump.HashEmptyBody() {
		t.Errorf("Hash = %q; want HashEmptyBody", parsed.Hash)
	}
	if len(parsed.Nonce) != 32 {
		t.Errorf("Nonce length = %d; want 32", len(parsed.Nonce))
	}

	sig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	if !ed25519.Verify(pub, parsed.SignatureInput(), sig) {
		t.Fatal("Ed25519 verify failed — signer header drift")
	}
}

func TestSigner_DisabledOmitsHeader(t *testing.T) {
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return false }, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v; want ErrAttestationDisabled", err)
	}
}

func TestSigner_NilEnabledLookupOmits(t *testing.T) {
	// Defensive guard: passing a nil getter must not panic — it
	// translates to "attestation disabled" so the request still flows.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", nil, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v; want ErrAttestationDisabled", err)
	}
}

func TestSigner_EmptyAgentIDRejected(t *testing.T) {
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if err == nil || !strings.Contains(err.Error(), "empty agent_id") {
		t.Errorf("err = %v; want empty agent_id error", err)
	}
}

func TestSigner_MissingKey_FailOpen(t *testing.T) {
	// Key absent from the keystore (agent not yet enrolled for attestation).
	// Must return ErrAttestationNotEnrolled — the caller maps this to "omit
	// header". An empty in-memory store models the not-found contract.
	s := NewSigner(keystore.NewMemoryStore(), keystore.AttestationKeyName,
		"x", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationNotEnrolled) {
		t.Errorf("err = %v; want ErrAttestationNotEnrolled", err)
	}
}

func TestSigner_MalformedKeyPEM(t *testing.T) {
	s := NewSigner(storeWith(t, []byte("not pem")), keystore.AttestationKeyName,
		"x", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if err == nil {
		t.Fatal("expected error on malformed PEM")
	}
}

func TestSigner_WrongAlgorithm_ECDSA(t *testing.T) {
	// A valid PKCS8 PEM whose inner key is ECDSA P-256 (the agent's
	// existing mTLS key shape) must be rejected: the attestation
	// signer is Ed25519-only. Generated dynamically so the PEM is
	// always valid; the rejection happens at the type assertion
	// inside loadKey.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("p256 keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s := NewSigner(storeWith(t, pemBytes), keystore.AttestationKeyName,
		"x", func() bool { return true }, newTestLogger())
	_, err = s.Sign()
	if err == nil {
		t.Fatal("expected rejection of non-Ed25519 key")
	}
	if !strings.Contains(err.Error(), "Ed25519") {
		t.Errorf("err should mention Ed25519: %v", err)
	}
}

func TestSigner_ValidPEMGarbageBytes_PKCS8ParseFails(t *testing.T) {
	// Hits the x509.ParsePKCS8PrivateKey error branch in loadKey: a
	// well-formed PEM block whose inner DER bytes are not a valid
	// PKCS8 structure. Real-world incident this guards: a partially-
	// written key where the BEGIN/END frames are correct but the body
	// is zero-padded — must fail the load, not panic.
	s := NewSigner(
		storeWith(t, []byte("-----BEGIN PRIVATE KEY-----\nAAAAAAAA\n-----END PRIVATE KEY-----\n")),
		keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if err == nil {
		t.Fatal("expected PKCS8 parse error on garbage bytes")
	}
	if !strings.Contains(err.Error(), "PKCS8") {
		t.Errorf("err should mention PKCS8: %v", err)
	}
}

func TestSigner_NonceEntropyFailure_FailOpen(t *testing.T) {
	// Starve the nonce RNG. Signer must return a wrapped error so the
	// caller can omit the header (fail-open) — must not panic.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	orig := signerRandReader
	signerRandReader = failReader{err: errors.New("entropy starved")}
	t.Cleanup(func() { signerRandReader = orig })

	_, err := s.Sign()
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("err = %v; want wrapped nonce-entropy error", err)
	}
}

func TestSigner_NilReceiverSafe(t *testing.T) {
	// Nil Signer ↔ "attestation feature not wired in this build". Sign
	// must return an error, not panic.
	var s *Signer
	_, err := s.Sign()
	if err == nil {
		t.Fatal("expected error on nil receiver")
	}
}

func TestSigner_InvalidateCachedKey_NilSafe(t *testing.T) {
	var s *Signer
	s.InvalidateCachedKey() // must not panic
}

func TestSigner_SecondSignHitsCachedKey(t *testing.T) {
	// First Sign loads + caches the key; second Sign must return from
	// the cached-key fast path without re-reading the keystore. We can't
	// directly observe a keystore-read avoidance from outside the package,
	// so we delete the key between calls — if the cache works, Sign still
	// succeeds; if not, the keystore Get returns not-found and Sign fails.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	if _, err := s.Sign(); err != nil {
		t.Fatalf("first Sign: %v", err)
	}
	if err := st.Delete(keystore.AttestationKeyName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sign(); err != nil {
		t.Errorf("second Sign should hit cached key after keystore delete: %v", err)
	}
}

func TestSigner_NilLoggerDoesNotPanicOnFailure(t *testing.T) {
	// A Signer created with nil logger must survive a failure path —
	// no panic, just a quiet return. The not-found path is quiet; the
	// keystore-error path runs logFailure, which must be nil-logger safe.
	s := NewSigner(keystore.NewMemoryStore(), keystore.AttestationKeyName, "x",
		func() bool { return true }, nil)
	if _, err := s.Sign(); err == nil {
		t.Fatal("expected ErrAttestationNotEnrolled")
	}
	// Force the non-quiet failure path so logFailure actually runs with a
	// nil logger.
	s2 := NewSigner(errGetStore{}, keystore.AttestationKeyName, "x", func() bool { return true }, nil)
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected keystore-get error")
	}
}

func TestSigner_InvalidateForcesReload(t *testing.T) {
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())

	if _, err := s.Sign(); err != nil {
		t.Fatalf("Sign 1: %v", err)
	}
	// Replace the stored key with a new Ed25519 key.
	_, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	pem2, _ := MarshalEd25519PrivateKeyPEM(newPriv)
	if err := st.Set(keystore.AttestationKeyName, pem2); err != nil {
		t.Fatal(err)
	}
	// Without Invalidate, signer would still use the cached key. Invalidate
	// and confirm the next call reloads + succeeds (covers the path).
	s.InvalidateCachedKey()
	if _, err := s.Sign(); err != nil {
		t.Fatalf("Sign 2 after invalidate: %v", err)
	}
}

func TestSignHeader_HappyAndFail(t *testing.T) {
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	hp, err := s.SignHeader()
	if err != nil {
		t.Fatalf("SignHeader: %v", err)
	}
	if hp.Name != tlsbump.AttestationHeaderName {
		t.Errorf("Name = %q", hp.Name)
	}
	if !strings.HasPrefix(hp.Value, "v1;") {
		t.Errorf("Value missing v1 prefix: %q", hp.Value)
	}

	// Disabled path returns empty pair + ErrAttestationDisabled.
	s2 := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return false }, newTestLogger())
	hp2, err := s2.SignHeader()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v", err)
	}
	if hp2.Value != "" || hp2.Name != "" {
		t.Errorf("disabled SignHeader returned non-empty pair: %+v", hp2)
	}
}

func TestSigner_GetProxyConnectHeader_HappyPath(t *testing.T) {
	st, pub := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "550e8400-e29b-41d4-a716-446655440000",
		func() bool { return true }, newTestLogger())

	proxyURL, _ := url.Parse("http://cp.example.com:3128")
	hdr, err := s.GetProxyConnectHeader(context.Background(), proxyURL, "api.openai.com:443")
	if err != nil {
		t.Fatalf("GetProxyConnectHeader returned error (fail-open broken): %v", err)
	}
	if hdr == nil {
		t.Fatal("expected non-nil header when signer is enabled + key present")
	}
	got := hdr.Get(tlsbump.AttestationHeaderName)
	if got == "" {
		t.Fatal("X-Nexus-Attestation header missing from returned http.Header")
	}
	parsed, err := tlsbump.ParseAttestationHeader(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	if !ed25519.Verify(pub, parsed.SignatureInput(), sig) {
		t.Fatal("Ed25519 verify failed — signer header drift")
	}
}

func TestSigner_GetProxyConnectHeader_DisabledReturnsNilHeaderNilError(t *testing.T) {
	// Fail-open contract: when attestation toggle is off, return
	// (nil, nil) so stdlib emits a normal CONNECT with no header.
	// Returning a non-nil error would abort the request — explicitly
	// forbidden.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return false }, newTestLogger())
	hdr, err := s.GetProxyConnectHeader(context.Background(), nil, "x:443")
	if err != nil {
		t.Errorf("disabled signer must NOT return error; got: %v", err)
	}
	if hdr != nil {
		t.Errorf("disabled signer must return nil header; got: %v", hdr)
	}
}

func TestSigner_GetProxyConnectHeader_MissingKeyReturnsNilHeaderNilError(t *testing.T) {
	// Same fail-open contract on the "no Ed25519 key in keystore yet"
	// (older agent without attestation key). Must not abort the request — CP
	// will MITM normally.
	s := NewSigner(keystore.NewMemoryStore(), keystore.AttestationKeyName, "x",
		func() bool { return true }, newTestLogger())
	hdr, err := s.GetProxyConnectHeader(context.Background(), nil, "x:443")
	if err != nil {
		t.Errorf("missing key must NOT return error; got: %v", err)
	}
	if hdr != nil {
		t.Errorf("missing key must return nil header; got: %v", hdr)
	}
}

func TestSigner_GetProxyConnectHeader_NilReceiverSafe(t *testing.T) {
	// Nil Signer (e.g., feature not wired in this build) must not
	// panic when stdlib invokes the callback.
	var s *Signer
	hdr, err := s.GetProxyConnectHeader(context.Background(), nil, "x:443")
	if err != nil || hdr != nil {
		t.Errorf("nil receiver should return (nil, nil); got (%v, %v)", hdr, err)
	}
}

func TestSigner_SignForBody_HashesActualBody(t *testing.T) {
	// SignForBody should produce a header whose hash field commits
	// to sha256(body) — not the empty-body placeholder. Verifies the
	// signature over the canonical pre-image with the real body hash.
	st, pub := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "agent-x", func() bool { return true }, newTestLogger())

	body := []byte(`{"model":"gpt-4","input":"hello"}`)
	hdr, err := s.SignForBody(body)
	if err != nil {
		t.Fatalf("SignForBody: %v", err)
	}
	parsed, err := tlsbump.ParseAttestationHeader(hdr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Hash != tlsbump.HashBody(body) {
		t.Errorf("Hash = %q; want %q", parsed.Hash, tlsbump.HashBody(body))
	}
	// Sig verifies against the canonical pre-image that includes the
	// real body hash.
	sig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	if !ed25519.Verify(pub, parsed.SignatureInput(), sig) {
		t.Fatal("Ed25519 verify failed against real-body hash")
	}
}

func TestSigner_SignForBody_EmptyBodyUsesPlaceholder(t *testing.T) {
	// nil + empty body both commit to the well-known empty-body hash.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())

	for _, body := range [][]byte{nil, {}} {
		hdr, err := s.SignForBody(body)
		if err != nil {
			t.Fatalf("SignForBody: %v", err)
		}
		parsed, _ := tlsbump.ParseAttestationHeader(hdr)
		if parsed.Hash != tlsbump.HashEmptyBody() {
			t.Errorf("body=%v: Hash = %q; want HashEmptyBody", body, parsed.Hash)
		}
	}
}

func TestSigner_InjectInto_HappyPath(t *testing.T) {
	st, pub := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "agent-1", func() bool { return true }, newTestLogger())

	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		bytes.NewReader(body))

	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	// Header set with a valid signature.
	hdrVal := req.Header.Get(tlsbump.AttestationHeaderName)
	if hdrVal == "" {
		t.Fatal("attestation header not set on request")
	}
	parsed, err := tlsbump.ParseAttestationHeader(hdrVal)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Hash != tlsbump.HashBody(body) {
		t.Errorf("Hash = %q; want %q", parsed.Hash, tlsbump.HashBody(body))
	}
	sigBytes, _ := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if !ed25519.Verify(pub, parsed.SignatureInput(), sigBytes) {
		t.Fatal("Ed25519 verify failed")
	}

	// Body is rewrapped — downstream wire send must see identical bytes.
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body bytes lost in rewrap: got %q want %q", got, body)
	}

	// GetBody is set and returns a fresh reader (stdlib redirect path).
	if req.GetBody == nil {
		t.Fatal("GetBody not set")
	}
	rc, _ := req.GetBody()
	got2, _ := io.ReadAll(rc)
	if string(got2) != string(body) {
		t.Errorf("GetBody returned wrong bytes: %q", got2)
	}
}

func TestSigner_InjectInto_NoBody(t *testing.T) {
	// GET request — no body. Injector must succeed with empty-body hash.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "agent-1", func() bool { return true }, newTestLogger())

	req, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	parsed, _ := tlsbump.ParseAttestationHeader(req.Header.Get(tlsbump.AttestationHeaderName))
	if parsed.Hash != tlsbump.HashEmptyBody() {
		t.Errorf("no-body Hash = %q; want HashEmptyBody", parsed.Hash)
	}
}

func TestSigner_InjectInto_NoHttpNoBodySentinel(t *testing.T) {
	// http.NoBody must not be consumed (it's the sentinel for "no body");
	// header still gets stamped with empty-body hash.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "agent-1", func() bool { return true }, newTestLogger())

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/dummy", http.NoBody)
	if err := s.InjectInto(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(tlsbump.AttestationHeaderName) == "" {
		t.Error("header missing on http.NoBody request")
	}
}

// streamReadTracker records whether its body was read or closed, to assert the
// injector leaves a streaming body untouched.
type streamReadTracker struct {
	r          io.Reader
	readCalled bool
	closed     bool
}

func (t *streamReadTracker) Read(p []byte) (int, error) { t.readCalled = true; return t.r.Read(p) }
func (t *streamReadTracker) Close() error               { t.closed = true; return nil }

func TestSigner_InjectInto_StreamingBodyNotConsumed(t *testing.T) {
	// An unknown-length (ContentLength < 0) request body is a streaming / bidi
	// body. InjectInto MUST NOT read it (reading to EOF deadlocks a connect-RPC
	// bidi call that holds its request stream open) and MUST leave it untouched
	// for the upstream streaming relay, signing the empty-body hash.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "agent-1", func() bool { return true }, newTestLogger())

	tracker := &streamReadTracker{r: strings.NewReader("streamed-bidi-request-body")}
	req, _ := http.NewRequest(http.MethodPost, "https://agentn.example/agent.v1.AgentService/Run", nil)
	req.Body = tracker
	req.ContentLength = -1

	if err := s.InjectInto(req); err != nil {
		t.Fatalf("InjectInto: %v", err)
	}
	if tracker.readCalled {
		t.Fatal("InjectInto consumed the streaming body — unknown-length bodies must be skipped to avoid the bidi deadlock")
	}
	if tracker.closed {
		t.Fatal("InjectInto closed the streaming body — it must stay open for the upstream relay")
	}
	if req.Body != io.ReadCloser(tracker) {
		t.Fatal("InjectInto replaced req.Body — the streaming body must be left untouched")
	}
	parsed, err := tlsbump.ParseAttestationHeader(req.Header.Get(tlsbump.AttestationHeaderName))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Hash != tlsbump.HashEmptyBody() {
		t.Errorf("streaming Hash = %q; want HashEmptyBody (body must not be read)", parsed.Hash)
	}
}

func TestSigner_InjectInto_DisabledOmitsHeader(t *testing.T) {
	// When attestation toggle is off, InjectInto returns nil error but
	// the header MUST NOT be set. Caller forwards the request normally
	// (no header → CP MITMs as usual).
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return false }, newTestLogger())

	req, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err := s.InjectInto(req); err != nil {
		t.Errorf("InjectInto must NOT return error on disabled (fail-open): %v", err)
	}
	if req.Header.Get(tlsbump.AttestationHeaderName) != "" {
		t.Error("disabled signer must not set the attestation header")
	}
}

func TestSigner_InjectInto_MissingKeyOmitsHeader(t *testing.T) {
	// Older agent without attestation key (no Ed25519 key in keystore yet).
	// Injector must succeed with nil + omit header (fail-open, never block
	// the request).
	s := NewSigner(keystore.NewMemoryStore(), keystore.AttestationKeyName, "x",
		func() bool { return true }, newTestLogger())

	req, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err := s.InjectInto(req); err != nil {
		t.Errorf("InjectInto must NOT return error on missing key: %v", err)
	}
	if req.Header.Get(tlsbump.AttestationHeaderName) != "" {
		t.Error("missing key must not produce an attestation header")
	}
}

func TestSigner_InjectInto_NilReceiverSafe(t *testing.T) {
	var s *Signer
	req, _ := http.NewRequest(http.MethodGet, "https://x/", nil)
	if err := s.InjectInto(req); err != nil {
		t.Errorf("nil receiver must return nil; got: %v", err)
	}
}

func TestSigner_InjectInto_NilRequestSafe(t *testing.T) {
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	if err := s.InjectInto(nil); err != nil {
		t.Errorf("nil request must return nil; got: %v", err)
	}
}

func TestSigner_InjectInto_OversizeBodyFallsBackToEmptyHash(t *testing.T) {
	// Body exceeding the 8 MiB cap → injector signs the empty-body hash
	// + rewraps so the wire send still has the full body. Pinning this
	// behaviour so a runaway client can't crash the injector or cause
	// the request to drop.
	st, _ := writeKey(t)
	s := NewSigner(st, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())

	bigBody := bytes.Repeat([]byte("A"), 9*1024*1024) // 9 MiB > 8 MiB cap
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/dummy",
		bytes.NewReader(bigBody))
	if err := s.InjectInto(req); err != nil {
		t.Fatal(err)
	}
	parsed, _ := tlsbump.ParseAttestationHeader(req.Header.Get(tlsbump.AttestationHeaderName))
	if parsed.Hash != tlsbump.HashEmptyBody() {
		t.Errorf("oversize body should sign empty-body hash; got %q", parsed.Hash)
	}
	// Body still fully readable (rewrap preserves bytes).
	got, _ := io.ReadAll(req.Body)
	if len(got) != len(bigBody) {
		t.Errorf("body length lost: got %d want %d", len(got), len(bigBody))
	}
}

func TestEnabledLookupFromString(t *testing.T) {
	if !EnabledLookupFromString("always")() {
		t.Error(`"always" should return true`)
	}
	if EnabledLookupFromString("")() {
		t.Error(`"" should return false`)
	}
	if EnabledLookupFromString("Other")() {
		t.Error(`"Other" should return false`)
	}
}

// TestSigner_RateLimitsFailureLogs walks the warnOnce path indirectly:
// we trigger N consecutive failures and assert the failedWarnAt
// counter only advances once within the 60-second window. We don't
// observe the log output directly (slog routes through a test handler)
// — the atomic counter is the contract. A keystore-get error is used
// because it is a non-quiet failure (unlike ErrAttestationNotEnrolled,
// which logFailure filters out by design).
func TestSigner_RateLimitsFailureLogs(t *testing.T) {
	s2 := NewSigner(errGetStore{}, keystore.AttestationKeyName, "x", func() bool { return true }, newTestLogger())
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected keystore-get error")
	}
	first := s2.failedWarnAt.Load()
	if first == 0 {
		t.Fatal("first failure did not advance failedWarnAt")
	}
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected second keystore-get error")
	}
	if got := s2.failedWarnAt.Load(); got != first {
		t.Errorf("rate-limit broken: failedWarnAt advanced to %d on 2nd call", got)
	}
}

// failReader is an io.Reader that always returns the configured error.
// Mirrors the agentca seam pattern.
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

// Compile-time check that failReader satisfies io.Reader.
var _ io.Reader = failReader{}
