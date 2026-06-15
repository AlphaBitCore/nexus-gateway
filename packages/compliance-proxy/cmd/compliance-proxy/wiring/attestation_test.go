package wiring

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestInitAttestationVerifier_DisabledReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	if got := InitAttestationVerifier(cfg, testLogger()); got != nil {
		t.Errorf("expected nil when feature disabled")
	}
}

func TestInitAttestationVerifier_NilConfigSafe(t *testing.T) {
	if got := InitAttestationVerifier(nil, testLogger()); got != nil {
		t.Errorf("nil config should return nil verifier")
	}
}

func TestInitAttestationVerifier_MissingHubURLDisables(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.AttestationEnabled = true
	cfg.Auth.InternalServiceToken = "token"
	cfg.Registry.NexusHubURL = ""
	if got := InitAttestationVerifier(cfg, testLogger()); got != nil {
		t.Error("expected nil when hub URL missing")
	}
}

func TestInitAttestationVerifier_MissingTokenDisables(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.AttestationEnabled = true
	cfg.Registry.NexusHubURL = "http://hub.local"
	cfg.Auth.InternalServiceToken = ""
	if got := InitAttestationVerifier(cfg, testLogger()); got != nil {
		t.Error("expected nil when service token missing")
	}
}

func TestInitAttestationVerifier_EnabledConstructsVerifier(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.AttestationEnabled = true
	cfg.Registry.NexusHubURL = "http://hub.local"
	cfg.Auth.InternalServiceToken = "tok"
	v := InitAttestationVerifier(cfg, testLogger())
	if v == nil {
		t.Fatal("expected non-nil verifier")
	}
	if !v.Enabled() {
		t.Error("verifier should be enabled")
	}
}

func TestFetchAttestationPubKey_HappyPath(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("auth header = %q", got)
		}
		if !strings.Contains(r.URL.Path, "/api/internal/things/agent-1/attestation-pubkey") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1","publicKey":"` + base64.StdEncoding.EncodeToString(pub) +
			`","certExpiresAt":"2099-01-02T03:04:05Z"}`))
	}))
	t.Cleanup(srv.Close)

	got, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "agent-1", "secret-token")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(got.Key) != string(pub) {
		t.Error("public key bytes differ from server response")
	}
	// SEC-M4-01: the cert expiry must be parsed off the wire so the verifier can
	// reject an expired key.
	want, _ := time.Parse(time.RFC3339, "2099-01-02T03:04:05Z")
	if !got.CertExpiresAt.Equal(want) {
		t.Errorf("CertExpiresAt = %v; want %v", got.CertExpiresAt, want)
	}
}

func TestFetchAttestationPubKey_404_UnknownAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not enrolled", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "no-such", "tok")
	if !errors.Is(err, tlsbump.ErrUnknownAgent) {
		t.Errorf("err = %v; want ErrUnknownAgent", err)
	}
}

func TestFetchAttestationPubKey_5xx_WrapsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "hub busted", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "x", "tok")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err should mention status code: %v", err)
	}
}

func TestFetchAttestationPubKey_EmptyPubkeyField_UnknownAgent(t *testing.T) {
	// Hub responds 200 OK but the publicKey field is empty — agent
	// row exists but attestation never enrolled. CP must treat the
	// same as 404 to engage the fail-open MITM path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agentId":"a","publicKey":""}`))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "a", "tok")
	if !errors.Is(err, tlsbump.ErrUnknownAgent) {
		t.Errorf("err = %v; want ErrUnknownAgent", err)
	}
}

func TestFetchAttestationPubKey_MalformedBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agentId":"a","publicKey":"!!!"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "a", "tok")
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("err = %v; want base64 decode error", err)
	}
}

func TestFetchAttestationPubKey_WrongKeySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agentId":"a","publicKey":"` +
			base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}) + `"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "a", "tok")
	if err == nil || !strings.Contains(err.Error(), "wrong size") {
		t.Errorf("err = %v; want size error", err)
	}
}

func TestFetchAttestationPubKey_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		srv.URL, "a", "tok")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v; want decode error", err)
	}
}

func TestFetchAttestationPubKey_NetworkError(t *testing.T) {
	// Connect to a closed port — surfaces as a transport error, not a
	// status code. Verifier must wrap this so the caller can log it.
	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		"http://127.0.0.1:1", "a", "tok")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "hub fetch") {
		t.Errorf("err should wrap hub fetch: %v", err)
	}
}

func TestFetchAttestationPubKey_BadRequestURL(t *testing.T) {
	// Build-request error path — a control char in the URL trips
	// http.NewRequestWithContext.
	_, err := fetchAttestationPubKey(context.Background(), http.DefaultClient,
		"http://hub\x7f/", "a", "tok")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Errorf("err = %v; want build request error", err)
	}
}
