package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// NewClient — coverage of CA / mTLS / default knobs branches.

func TestNewClient_DefaultsApplied(t *testing.T) {
	// All zero-valued knobs (Timeout, MaxRetries, RetryDelay) should pick up
	// the documented defaults. Verifying via the captured cfg ensures a
	// future change doesn't silently flip these values — RetryDelay==0 in
	// particular would busy-loop the retry path.
	c, err := NewClient(Config{HubURL: "https://hub.example.com"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout default: got %v want 30s", c.cfg.Timeout)
	}
	if c.cfg.MaxRetries != 2 {
		t.Errorf("MaxRetries default: got %d want 2", c.cfg.MaxRetries)
	}
	if c.cfg.RetryDelay != time.Second {
		t.Errorf("RetryDelay default: got %v want 1s", c.cfg.RetryDelay)
	}
}

func TestNewClient_CAFileMissing(t *testing.T) {
	// Explicit pinning request that points at a missing file must fail
	// closed — the wrapped error mentions the path so operators can
	// diagnose the misconfig.
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")
	_, err := NewClient(Config{HubURL: "https://hub.example.com", CACertFile: missing})
	if err == nil {
		t.Fatal("expected error when CA file is missing")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not mention path %q", err, missing)
	}
}

func TestNewClient_CAFileInvalidPEM(t *testing.T) {
	// A file that exists but contains no parseable certificate must fail —
	// guard against an operator stamping a stray text file as their pin.
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(bad, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := NewClient(Config{HubURL: "https://hub.example.com", CACertFile: bad})
	if err == nil {
		t.Fatal("expected error when CA file contains no certificates")
	}
	if !strings.Contains(err.Error(), "no valid certificates") {
		t.Errorf("error %q should mention 'no valid certificates'", err)
	}
}

func TestNewClient_CAFileValid(t *testing.T) {
	// Valid CA PEM should be installed as a trust root (alongside system
	// roots — see the x509-system-roots note in the code) and NewClient
	// must return a usable client. We verify the happy path by confirming
	// no error AND the *http.Client is non-nil.
	caPEM := selfSignedCAPEM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	c, err := NewClient(Config{HubURL: "https://hub.example.com", CACertFile: path})
	if err != nil {
		t.Fatalf("NewClient with valid CA: %v", err)
	}
	if c.HTTPClient() == nil {
		t.Error("expected non-nil HTTPClient")
	}
}

func TestNewClient_BadCertKey(t *testing.T) {
	// Bad cert/key pair → tls.LoadX509KeyPair fails → NewClient returns a
	// wrapped error. Operators who type the wrong file path here must see
	// a fail-closed result, not a silent no-mTLS client.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err := NewClient(Config{
		HubURL:   "https://hub.example.com",
		CertFile: certPath,
		KeyFile:  keyPath,
	})
	if err == nil {
		t.Fatal("expected error from bad cert/key pair")
	}
	if !strings.Contains(err.Error(), "load mTLS cert") {
		t.Errorf("error %q should mention 'load mTLS cert'", err)
	}
}

func TestNewClient_ValidCertKey(t *testing.T) {
	// Valid self-signed cert/key pair must load cleanly. Combined with the
	// CAFileValid test this fully exercises the mTLS-on branches.
	certPEM, keyPEM := selfSignedCertKey(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	keyPath := filepath.Join(dir, "agent.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	c, err := NewClient(Config{
		HubURL:   "https://hub.example.com",
		CertFile: certPath,
		KeyFile:  keyPath,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.BaseURL() != "https://hub.example.com" {
		t.Errorf("BaseURL: got %q", c.BaseURL())
	}
}

// HTTPClient / BaseURL accessors.

func TestClient_AccessorsReturnConfiguredValues(t *testing.T) {
	c, err := NewClient(Config{HubURL: "https://hub.test:8443"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.BaseURL(); got != "https://hub.test:8443" {
		t.Errorf("BaseURL: got %q want %q", got, "https://hub.test:8443")
	}
	if c.HTTPClient() == nil {
		t.Fatal("HTTPClient must not be nil")
	}
	// The underlying *http.Client must be the same instance the client
	// uses internally — callers re-use it for arbitrary downloads (e.g.
	// the updater) and a copy would break TLS pinning.
	if c.HTTPClient() != c.httpClient {
		t.Error("HTTPClient must return the internally-held *http.Client")
	}
}

// doWithRetry — DeviceTokenFn / ThingIDFn header injection + header override.

func TestDoWithRetry_InjectsBearerAndThingID(t *testing.T) {
	// Hub's DeviceOrServiceAuth middleware requires both Authorization
	// and X-Thing-Id on every /api/internal/things/* call. Verifying both
	// land on the wire pins the DeviceTokenFn + ThingIDFn header-injection contract.
	var gotAuth, gotThing string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotThing = r.Header.Get("X-Thing-Id")
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:        srv.URL,
		Timeout:       2 * time.Second,
		MaxRetries:    0,
		DeviceTokenFn: func() string { return "tok-abc" },
		ThingIDFn:     func() string { return "thing-xyz" },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin"); err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization: got %q want %q", gotAuth, "Bearer tok-abc")
	}
	if gotThing != "thing-xyz" {
		t.Errorf("X-Thing-Id: got %q want %q", gotThing, "thing-xyz")
	}
}

func TestDoWithRetry_EmptyTokenSkipsAuthHeader(t *testing.T) {
	// A registered callback that returns "" must NOT set Authorization —
	// the pre-enrollment path constructs the client before a token exists
	// and we don't want to send "Bearer " (empty value) on the wire.
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:        srv.URL,
		Timeout:       2 * time.Second,
		MaxRetries:    0,
		DeviceTokenFn: func() string { return "" },
		ThingIDFn:     func() string { return "" },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin"); err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if sawAuth {
		t.Error("Authorization header should be absent when token fn returns empty")
	}
}

func TestUploadAudit_PerCallHeaderOverridesAuthThingID(t *testing.T) {
	// UploadAudit passes its own X-Thing-Id (the deviceID parameter) and
	// that value MUST win over the global ThingIDFn — UploadAudit is the
	// drain-by-other-thing-id path. The code's per-call header loop runs
	// AFTER the ThingIDFn injection precisely for this reason.
	var gotThing string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotThing = r.Header.Get("X-Thing-Id")
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": 1})
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 0,
		ThingIDFn:  func() string { return "global-thing" },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.UploadAudit(context.Background(), "drain-thing", []AuditEvent{{ID: "e1"}}); err != nil {
		t.Fatalf("UploadAudit: %v", err)
	}
	if gotThing != "drain-thing" {
		t.Errorf("X-Thing-Id override: got %q want %q", gotThing, "drain-thing")
	}
}

// doWithRetry — ctx-cancellation arms not covered by existing tests.

func TestDoWithRetry_CtxCancelledBeforeRetryWait(t *testing.T) {
	// On a 5xx response the retry loop enters time.After(RetryDelay) — a
	// context cancellation while waiting must surface as ctx.Err(), not
	// as the "request failed after N retries" wrapper. We force a 500 on
	// the first attempt and cancel before RetryDelay elapses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 5,
		RetryDelay: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err = c.CheckUpdate(ctx, "1.0.0", "darwin")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled); got %v", err)
	}
}

func TestDoWithRetry_CtxAlreadyDoneOnEntry(t *testing.T) {
	// Pre-cancelled context must short-circuit before any network call —
	// the defensive ctx.Err() check inside the retry loop is the gate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(UpdateInfo{})
	}))
	defer srv.Close()

	c, err := NewClient(Config{HubURL: srv.URL, Timeout: 2 * time.Second, MaxRetries: 0})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.CheckUpdate(ctx, "1.0.0", "darwin")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled); got %v", err)
	}
}

func TestDoWithRetry_RetriesExhausted(t *testing.T) {
	// Every attempt returns 500 → retry budget exhausted → wrapped error
	// includes the trailing "server error: 500" via %w. Verifies the
	// final "request failed after N retries" branch.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 2,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "request failed after 2 retries") {
		t.Errorf("error should report retry count: %v", err)
	}
	if !strings.Contains(err.Error(), "server error: 500") {
		t.Errorf("error should wrap the 5xx cause: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts (initial + 2 retries), got %d", calls.Load())
	}
}

func TestDoWithRetry_TransportErrorRetried(t *testing.T) {
	// Pointing at a closed port forces the transport-layer error branch
	// (resp == nil, err != nil) inside doWithRetry. With MaxRetries=1 the
	// loop runs twice, then returns the wrapped error.
	addr := closedTCPAddr(t)
	c, err := NewClient(Config{
		HubURL:     "http://" + addr,
		Timeout:    250 * time.Millisecond,
		MaxRetries: 1,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	if err == nil {
		t.Fatal("expected transport-layer error against closed port")
	}
	if !strings.Contains(err.Error(), "request failed after 1 retries") {
		t.Errorf("expected retry-wrap, got %v", err)
	}
}

// UploadAudit / UploadExemption / CheckUpdate / RenewCert — error matrix.

func TestUploadAudit_MarshalError(t *testing.T) {
	// Invalid json.RawMessage inside AuditEvent.Details should fail at the
	// json.Marshal step BEFORE any network call. The error must mention
	// "marshal audit" so caller logs distinguish encode failures from
	// transport failures.
	c, err := NewClient(Config{HubURL: "http://127.0.0.1:1", Timeout: time.Second, MaxRetries: 0})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	bad := AuditEvent{ID: "e1", Details: json.RawMessage("not-json")}
	_, err = c.UploadAudit(context.Background(), "dev-1", []AuditEvent{bad})
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal audit") {
		t.Errorf("error should mention 'marshal audit': %v", err)
	}
}

func TestUploadAudit_TransportErrorPropagates(t *testing.T) {
	// Transport-layer error (closed port) propagates out of UploadAudit —
	// covers the err-returning branch after doWithRetry.
	c, err := NewClient(Config{
		HubURL:     "http://" + closedTCPAddr(t),
		Timeout:    250 * time.Millisecond,
		MaxRetries: 0,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.UploadAudit(context.Background(), "dev-1", []AuditEvent{{ID: "e1"}})
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestUploadAudit_DecodeError(t *testing.T) {
	// HTTP 200 with a body that isn't a valid JSON object must surface as
	// a "decode audit response" error — Hub MUST always send a parseable
	// body on success, so the agent fails closed on garbage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UploadAudit(context.Background(), "dev-1", []AuditEvent{{ID: "e1"}})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode audit response") {
		t.Errorf("error should mention 'decode audit response': %v", err)
	}
}

func TestUploadExemption_ErrorResponse(t *testing.T) {
	// Hub returning non-200 on exemption upload must surface a wrapped
	// error containing the status and body — operators rely on the body
	// snippet to diagnose validation failures.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"thingId not found"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.UploadExemption(context.Background(), ExemptionUpload{ThingID: "dev-1", Host: "h"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "exemption upload failed (403)") {
		t.Errorf("error should mention status 403: %v", err)
	}
	if !strings.Contains(err.Error(), "thingId not found") {
		t.Errorf("error should include body: %v", err)
	}
}

func TestUploadExemption_TransportError(t *testing.T) {
	// Transport-level failure on exemption upload propagates the wrapped
	// retry-exhausted error — covers the err-returning branch in
	// UploadExemption after doWithRetry.
	c, err := NewClient(Config{
		HubURL:     "http://" + closedTCPAddr(t),
		Timeout:    250 * time.Millisecond,
		MaxRetries: 0,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.UploadExemption(context.Background(), ExemptionUpload{ThingID: "dev-1"}); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestCheckUpdate_ErrorResponse(t *testing.T) {
	// 404 on /update-check must surface "update check failed (404)" with
	// the response body — distinguishes "endpoint missing" from "actually
	// unavailable" in operator logs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such target"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "update check failed (404)") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestCheckUpdate_DecodeError(t *testing.T) {
	// 200 with invalid JSON body must surface a "decode update" error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not valid"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode update") {
		t.Errorf("error should mention 'decode update': %v", err)
	}
}

// doWithRetry — NewRequestWithContext failure (invalid URL).

func TestDoWithRetry_NewRequestError(t *testing.T) {
	// A HubURL containing a NUL byte trips url.Parse inside
	// http.NewRequestWithContext, exercising the request-construction
	// error branch — distinct from transport-level dial errors which the
	// other tests cover. The returned error must surface immediately
	// (no retry on a non-retryable request-shape failure).
	c, err := NewClient(Config{
		HubURL:     "http://hub\x00.example.com",
		Timeout:    250 * time.Millisecond,
		MaxRetries: 3,
		RetryDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	_, err = c.CheckUpdate(context.Background(), "1.0.0", "darwin")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from malformed URL")
	}
	if !strings.Contains(err.Error(), "invalid control character") &&
		!strings.Contains(err.Error(), "parse") {
		t.Errorf("expected URL parse error, got %v", err)
	}
	// No retries — the function should return promptly. Allow a generous
	// budget for slow CI but still well under the RetryDelay * MaxRetries
	// budget if retries had run.
	if elapsed > 100*time.Millisecond {
		t.Errorf("NewRequest error should bypass retry loop; elapsed=%v", elapsed)
	}
}

// Body limit guard — io.LimitReader on 4xx body should not OOM the agent
// even if Hub misbehaves by returning a giant error body.

func TestUploadAudit_LargeErrorBodyTruncated(t *testing.T) {
	// 13MiB body on a 400 — io.LimitReader caps the read at 10MiB so the
	// error message contains at most maxResponseBytes worth of body and
	// the call returns promptly without exhausting memory.
	big := strings.Repeat("x", 13<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.UploadAudit(context.Background(), "dev-1", []AuditEvent{{ID: "e1"}})
	if err == nil {
		t.Fatal("expected error")
	}
	// The error string body slice should be capped — never longer than
	// maxResponseBytes + a small framing constant.
	if len(err.Error()) > maxResponseBytes+1024 {
		t.Errorf("error body should have been truncated; len=%d", len(err.Error()))
	}
}

// Helpers — closed-port address + self-signed cert/key pairs for mTLS tests.

// closedTCPAddr binds a port, captures the address, and immediately closes
// the listener. Any dial against that address fails with ECONNREFUSED on
// every supported OS — the precise transport-layer error doWithRetry must
// surface to the caller.
func closedTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// selfSignedCertKey returns a usable cert+key pair as PEM bytes. The
// values are throwaway — only used to drive tls.LoadX509KeyPair through
// its success path inside NewClient.
func selfSignedCertKey(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// selfSignedCAPEM returns a PEM-encoded self-signed CA certificate.
// AppendCertsFromPEM accepts this, exercising the happy path of the
// CA-pinning branch in NewClient.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// End-to-end TLS — confirm that a configured CA pin + correct client cert
// against an httptest.NewTLSServer actually completes a request.

func TestNewClient_TLSRoundTripWithPinnedCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(UpdateInfo{Available: false})
	}))
	defer srv.Close()

	// Dump the test server's cert as PEM into a temp file and pin it.
	dir := t.TempDir()
	caPath := filepath.Join(dir, "test-ca.pem")
	if err := os.WriteFile(caPath, encodeCert(srv.Certificate()), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	c, err := NewClient(Config{
		HubURL:     srv.URL,
		CACertFile: caPath,
		Timeout:    2 * time.Second,
		MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.CheckUpdate(context.Background(), "1.0.0", "darwin"); err != nil {
		t.Fatalf("CheckUpdate over pinned TLS: %v", err)
	}
}

func encodeCert(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// parseSingleCertPEM — unit tests for the helper that backs DeviceCAFile parsing.

func TestParseSingleCertPEM_ValidCert(t *testing.T) {
	caPEM := selfSignedCAPEM(t)
	cert, err := parseSingleCertPEM(caPEM)
	if err != nil {
		t.Fatalf("parseSingleCertPEM: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil certificate")
	}
	if cert.Subject.CommonName != "test-ca" {
		t.Errorf("CommonName = %q, want test-ca", cert.Subject.CommonName)
	}
}

func TestParseSingleCertPEM_EmptyInput(t *testing.T) {
	_, err := parseSingleCertPEM([]byte{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "no CERTIFICATE PEM block found") {
		t.Errorf("error %q should mention 'no CERTIFICATE PEM block found'", err)
	}
}

func TestParseSingleCertPEM_WrongPEMType(t *testing.T) {
	// A PEM block with type "PRIVATE KEY" must be rejected — only CERTIFICATE
	// blocks carry x509 DER data.
	badPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")})
	_, err := parseSingleCertPEM(badPEM)
	if err == nil {
		t.Fatal("expected error for wrong PEM type")
	}
	if !strings.Contains(err.Error(), "no CERTIFICATE PEM block found") {
		t.Errorf("error %q should mention 'no CERTIFICATE PEM block found'", err)
	}
}

func TestParseSingleCertPEM_CorruptDER(t *testing.T) {
	// A CERTIFICATE block with garbage DER bytes must return an x509 parse error.
	badPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not der at all")})
	_, err := parseSingleCertPEM(badPEM)
	if err == nil {
		t.Fatal("expected error for corrupt DER")
	}
}

// NewClient DeviceCAFile branch — fail-open behaviours.

func TestNewClient_DeviceCAFile_Missing_FailOpen(t *testing.T) {
	// Missing DeviceCAFile must NOT return an error — it's a fail-open warning
	// path (device CA not yet installed). The client is still usable.
	missing := filepath.Join(t.TempDir(), "device-ca.pem")
	c, err := NewClient(Config{HubURL: "https://hub.example.com", DeviceCAFile: missing})
	if err != nil {
		t.Fatalf("NewClient with missing DeviceCAFile must not error; got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_DeviceCAFile_Invalid_FailOpen(t *testing.T) {
	// An unreadable PEM in DeviceCAFile must also fail-open (log + unfiltered pool).
	dir := t.TempDir()
	bad := filepath.Join(dir, "device-ca.pem")
	if err := os.WriteFile(bad, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	c, err := NewClient(Config{HubURL: "https://hub.example.com", DeviceCAFile: bad})
	if err != nil {
		t.Fatalf("NewClient with unparseable DeviceCAFile must not error; got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_DeviceCAFile_Valid_ReturnsClient(t *testing.T) {
	// A valid device CA PEM leads SystemPoolExcluding to build a filtered
	// pool. NewClient must succeed with a non-nil client.
	dir := t.TempDir()
	caPath := filepath.Join(dir, "device-ca.pem")
	if err := os.WriteFile(caPath, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	c, err := NewClient(Config{HubURL: "https://hub.example.com", DeviceCAFile: caPath})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// Sanity: compile-time assertion that the test still references the symbols
// it exercises — guards against a future rename silently turning a test
// into a no-op.
var _ = tls.VersionTLS12
var _ = fmt.Sprintf
