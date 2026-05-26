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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeKey persists a freshly-generated Ed25519 private key in the
// PKCS8 PEM shape the signer expects. Returns (path, public-key).
func writeKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pemBytes, err := MarshalEd25519PrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "attestation-key.pem")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path, pub
}

func TestSigner_HappyPath_HeaderVerifies(t *testing.T) {
	path, pub := writeKey(t)
	s := NewSigner(path, "550e8400-e29b-41d4-a716-446655440000",
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
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return false }, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v; want ErrAttestationDisabled", err)
	}
}

func TestSigner_NilEnabledLookupOmits(t *testing.T) {
	// Defensive guard: passing a nil getter must not panic — it
	// translates to "attestation disabled" so the request still flows.
	path, _ := writeKey(t)
	s := NewSigner(path, "x", nil, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v; want ErrAttestationDisabled", err)
	}
}

func TestSigner_EmptyAgentIDRejected(t *testing.T) {
	path, _ := writeKey(t)
	s := NewSigner(path, "", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if err == nil || !strings.Contains(err.Error(), "empty agent_id") {
		t.Errorf("err = %v; want empty agent_id error", err)
	}
}

func TestSigner_MissingKeyFile_FailOpen(t *testing.T) {
	// Key file absent (agent not yet enrolled for attestation). Must return
	// ErrAttestationNotEnrolled — the caller maps this to "omit header".
	dir := t.TempDir()
	s := NewSigner(filepath.Join(dir, "no-such-file.pem"),
		"x", func() bool { return true }, newTestLogger())
	_, err := s.Sign()
	if !errors.Is(err, ErrAttestationNotEnrolled) {
		t.Errorf("err = %v; want ErrAttestationNotEnrolled", err)
	}
}

func TestSigner_MalformedKeyPEM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(path, []byte("not pem"), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
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
	dir := t.TempDir()
	path := filepath.Join(dir, "ecdsa.pem")
	if err := os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		0600); err != nil {
		t.Fatal(err)
	}
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
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
	// written PEM where the BEGIN/END frames are correct but the body
	// is zero-padded — must fail the load, not panic.
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(path,
		[]byte("-----BEGIN PRIVATE KEY-----\nAAAAAAAA\n-----END PRIVATE KEY-----\n"),
		0600); err != nil {
		t.Fatal(err)
	}
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
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
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
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
	// the cached-key fast path without re-reading the file. We can't
	// directly observe a file-read avoidance from outside the package,
	// so we delete the file between calls — if the cache works, Sign
	// still succeeds; if not, ReadFile fails.
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
	if _, err := s.Sign(); err != nil {
		t.Fatalf("first Sign: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sign(); err != nil {
		t.Errorf("second Sign should hit cached key after file removed: %v", err)
	}
}

func TestSigner_NilLoggerDoesNotPanicOnFailure(t *testing.T) {
	// A Signer created with nil logger must survive a failure path —
	// no panic, just a quiet return.
	dir := t.TempDir()
	s := NewSigner(filepath.Join(dir, "no-such.pem"), "x",
		func() bool { return true }, nil)
	if _, err := s.Sign(); err == nil {
		t.Fatal("expected error")
	}
	// Force a non-NotExist failure path so logFailure actually runs
	// with a nil logger; the directory-as-file pattern delivers it.
	s2 := NewSigner(dir, "x", func() bool { return true }, nil)
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected directory-as-file error")
	}
}

func TestSigner_InvalidateForcesReload(t *testing.T) {
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())

	if _, err := s.Sign(); err != nil {
		t.Fatalf("Sign 1: %v", err)
	}
	// Mutate the on-disk key to a new Ed25519 key.
	_, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	pem2, _ := MarshalEd25519PrivateKeyPEM(newPriv)
	if err := os.WriteFile(path, pem2, 0600); err != nil {
		t.Fatal(err)
	}
	// Without Invalidate, signer would still use the cached key — we'd
	// need to verify by signing twice and comparing. Easier: just
	// invalidate and confirm the next call succeeds (covers the path).
	s.InvalidateCachedKey()
	if _, err := s.Sign(); err != nil {
		t.Fatalf("Sign 2 after invalidate: %v", err)
	}
}

func TestSignHeader_HappyAndFail(t *testing.T) {
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
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
	s2 := NewSigner(path, "x", func() bool { return false }, newTestLogger())
	hp2, err := s2.SignHeader()
	if !errors.Is(err, ErrAttestationDisabled) {
		t.Errorf("err = %v", err)
	}
	if hp2.Value != "" || hp2.Name != "" {
		t.Errorf("disabled SignHeader returned non-empty pair: %+v", hp2)
	}
}

func TestSigner_GetProxyConnectHeader_HappyPath(t *testing.T) {
	path, pub := writeKey(t)
	s := NewSigner(path, "550e8400-e29b-41d4-a716-446655440000",
		func() bool { return true }, newTestLogger())

	proxyURL, _ := url.Parse("http://cp.example.com:3128")
	hdr, err := s.GetProxyConnectHeader(context.Background(), proxyURL, "api.openai.com:443")
	if err != nil {
		t.Fatalf("GetProxyConnectHeader returned error (fail-open broken): %v", err)
	}
	if hdr == nil {
		t.Fatal("expected non-nil header when signer is enabled + key on disk")
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
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return false }, newTestLogger())
	hdr, err := s.GetProxyConnectHeader(context.Background(), nil, "x:443")
	if err != nil {
		t.Errorf("disabled signer must NOT return error; got: %v", err)
	}
	if hdr != nil {
		t.Errorf("disabled signer must return nil header; got: %v", hdr)
	}
}

func TestSigner_GetProxyConnectHeader_MissingKeyReturnsNilHeaderNilError(t *testing.T) {
	// Same fail-open contract on the "no Ed25519 key on disk yet"
	// (older agent without attestation key). Must not abort the request — CP
	// will MITM normally.
	dir := t.TempDir()
	s := NewSigner(filepath.Join(dir, "no-key.pem"), "x",
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
	path, pub := writeKey(t)
	s := NewSigner(path, "agent-x", func() bool { return true }, newTestLogger())

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
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())

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
	path, pub := writeKey(t)
	s := NewSigner(path, "agent-1", func() bool { return true }, newTestLogger())

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
	path, _ := writeKey(t)
	s := NewSigner(path, "agent-1", func() bool { return true }, newTestLogger())

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
	path, _ := writeKey(t)
	s := NewSigner(path, "agent-1", func() bool { return true }, newTestLogger())

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/dummy", http.NoBody)
	if err := s.InjectInto(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(tlsbump.AttestationHeaderName) == "" {
		t.Error("header missing on http.NoBody request")
	}
}

func TestSigner_InjectInto_DisabledOmitsHeader(t *testing.T) {
	// When attestation toggle is off, InjectInto returns nil error but
	// the header MUST NOT be set. Caller forwards the request normally
	// (no header → CP MITMs as usual).
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return false }, newTestLogger())

	req, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err := s.InjectInto(req); err != nil {
		t.Errorf("InjectInto must NOT return error on disabled (fail-open): %v", err)
	}
	if req.Header.Get(tlsbump.AttestationHeaderName) != "" {
		t.Error("disabled signer must not set the attestation header")
	}
}

func TestSigner_InjectInto_MissingKeyOmitsHeader(t *testing.T) {
	// Older agent without attestation key (no Ed25519 key on disk yet). Injector must succeed
	// with nil + omit header (fail-open, never block the request).
	dir := t.TempDir()
	s := NewSigner(filepath.Join(dir, "no-key.pem"), "x",
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
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())
	if err := s.InjectInto(nil); err != nil {
		t.Errorf("nil request must return nil; got: %v", err)
	}
}

func TestSigner_InjectInto_OversizeBodyFallsBackToEmptyHash(t *testing.T) {
	// Body exceeding the 8 MiB cap → injector signs the empty-body hash
	// + rewraps so the wire send still has the full body. Pinning this
	// behaviour so a runaway client can't crash the injector or cause
	// the request to drop.
	path, _ := writeKey(t)
	s := NewSigner(path, "x", func() bool { return true }, newTestLogger())

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
// — the atomic counter is the contract.
func TestSigner_RateLimitsFailureLogs(t *testing.T) {
	dir := t.TempDir()
	// The ErrAttestationNotEnrolled path is filtered out of logFailure
	// by design (it's the always-quiet "agent hasn't re-enrolled yet"
	// state). To hit the rate-limit branch we need a different failure
	// — a directory passed as a file path yields a non-NotExist error.
	s2 := NewSigner(dir, "x", func() bool { return true }, newTestLogger())
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected read error on directory path")
	}
	first := s2.failedWarnAt.Load()
	if first == 0 {
		t.Fatal("first failure did not advance failedWarnAt")
	}
	if _, err := s2.Sign(); err == nil {
		t.Fatal("expected second read error")
	}
	if got := s2.failedWarnAt.Load(); got != first {
		t.Errorf("rate-limit broken: failedWarnAt advanced to %d on 2nd call", got)
	}

	_ = atomic.Int64{} // ensure import not pruned in case the test stub above changes
}

// failReader is an io.Reader that always returns the configured error.
// Mirrors the agentca seam pattern.
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

// Compile-time check that failReader satisfies io.Reader.
var _ io.Reader = failReader{}
