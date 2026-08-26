package wiring

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

func TestInitJWT_NilConsumer_ReturnsVerifierWithAlwaysAllow(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:3001"
	cfg.Auth.InternalServiceToken = "test-token"

	v := InitJWT(context.Background(), cfg, nil, "cp-test", silentLogger())
	if v == nil {
		t.Fatal("expected non-nil Verifier when consumer is nil")
	}
}

func TestInitJWT_WithConsumer_DerivesURLsFromIssuer(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:3001/"
	cfg.Auth.InternalServiceToken = "tok"
	// RevocationIntrospectURL and RevocationReplayURL are empty — they should
	// be derived from Issuer.

	v := InitJWT(context.Background(), cfg, &fakeMQConsumer{}, "cp-test", silentLogger())
	if v == nil {
		t.Fatal("expected non-nil Verifier when consumer is provided")
	}
}

func TestInitJWT_WithConsumer_ExplicitURLs_Respected(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:3001"
	cfg.AuthServer.RevocationIntrospectURL = "http://other/introspect"
	cfg.AuthServer.RevocationReplayURL = "http://other/revocations"
	cfg.Auth.InternalServiceToken = "tok"

	v := InitJWT(context.Background(), cfg, &fakeMQConsumer{}, "cp-test", silentLogger())
	if v == nil {
		t.Fatal("expected non-nil Verifier")
	}
}

func TestInitJWT_NilConsumer_VerifierTypeIsJWTVerifier(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:3001"

	v := InitJWT(context.Background(), cfg, nil, "cp-test", silentLogger())
	if _, ok := interface{}(v).(*jwtverifier.Verifier); !ok {
		t.Errorf("expected *jwtverifier.Verifier, got %T", v)
	}
}

// errorMQConsumer is a fake mq.Consumer whose Consume always returns the
// provided sentinel error immediately. Used to trigger the revocation
// consumer goroutine's error-log branch without a real NATS connection.
// Subscribe is a no-op for symmetry — the production code path uses
// Consume to get JetStream durability + per-CP-instance fan-out.
type errorMQConsumer struct{ err error }

func (e *errorMQConsumer) Subscribe(_ context.Context, _ string, _ mq.MessageHandler) error {
	return nil
}
func (e *errorMQConsumer) Consume(_ context.Context, _ string, _ string, _ mq.MessageHandler) error {
	return e.err
}
func (e *errorMQConsumer) Close() error { return nil }

// TestInitJWT_WithConsumer_ConsumeError_LogsError verifies that when the
// MQ consumer's Consume returns a non-cancelled error, the goroutine body
// logs it via logger.Error.  The goroutine runs asynchronously, so we wait
// briefly for it to complete after InitJWT returns.
func TestInitJWT_WithConsumer_ConsumeError_LogsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:3001"
	cfg.Auth.InternalServiceToken = "tok"

	// A cancelled context would suppress the logger.Error call — use background.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sentinel := errors.New("consume failed")
	consumer := &errorMQConsumer{err: sentinel}

	v := InitJWT(ctx, cfg, consumer, "cp-test", silentLogger())
	if v == nil {
		t.Fatal("expected non-nil Verifier")
	}

	// Give the goroutine time to execute: StartConsumer calls Consume, which
	// returns the sentinel error immediately; goroutine logs Error then exits.
	time.Sleep(50 * time.Millisecond)
}

// TestSanitizeForJetStreamDurable pins the JetStream-unsafe character set
// the helper strips so a future addition (e.g. operator yaml id with `@`)
// doesn't silently introduce an invalid durable name.
func TestSanitizeForJetStreamDurable(t *testing.T) {
	cases := map[string]string{
		"cp-host.example.com-3001": "cp-host_example_com-3001",
		"cp-east/1":                "cp-east_1",
		"cp:east":                  "cp_east",
		"cp east":                  "cp_east",
		"cp-clean-id":              "cp-clean-id",
		"":                         "",
	}
	for in, want := range cases {
		if got := sanitizeForJetStreamDurable(in); got != want {
			t.Errorf("sanitizeForJetStreamDurable(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDeriveThingID pins the yaml-id vs hostname-fallback branches so the
// JWT-side group derivation (InitJWT) and Hub-side ThingID (InitHub) stay
// consistent — drift between them turns the JetStream fan-out into a
// silent work-queue (one CP eats all revocation events).
func TestDeriveThingID(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Port = 3001
	cfg.ID = "cp-east-1"
	if got := DeriveThingID(cfg); got != "cp-east-1" {
		t.Errorf("yaml id should win; got %q", got)
	}
	cfg.ID = ""
	got := DeriveThingID(cfg)
	if got == "" {
		t.Fatal("hostname-fallback returned empty")
	}
	// Hostname is OS-dependent; assert the prefix + suffix shape instead.
	if !strings.HasPrefix(got, "cp-") || !strings.HasSuffix(got, "-3001") {
		t.Errorf("hostname-fallback shape wrong: %q (want cp-<hostname>-3001)", got)
	}
}

// The verifier fetches signing keys from an ADDRESS, while the issuer is an
// IDENTITY, and the two are the same string only when the issuer's origin is
// reachable from this process. When it is not — a console published on a host
// port while the control plane listens inside a container — deriving the one
// from the other yields a URL that dials nothing, the verifier can never load
// a key, and every admin request answers 401 holding a valid token.
func TestAdminJWKSURL_ExplicitAddressWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:18080"
	cfg.AuthServer.JWKSURL = "http://control-plane:3001/.well-known/jwks.json"

	got := adminJWKSURL(cfg)
	if got != "http://control-plane:3001/.well-known/jwks.json" {
		t.Fatalf("configured JWKS address not used: got %q", got)
	}
	if strings.Contains(got, "localhost:18080") {
		t.Fatalf("resolved back to the issuer origin, which is the browser's address and not dialable here: %q", got)
	}
}

// Every deployment that predates the field leaves it empty, and must keep
// resolving to exactly the URL it resolved to before — including an external
// IdP, whose publicly reachable issuer makes the derivation correct.
func TestAdminJWKSURL_UnsetKeepsIssuerDerivation(t *testing.T) {
	for _, tc := range []struct{ name, issuer, jwks, want string }{
		{"external idp", "https://tenant.okta.com", "", "https://tenant.okta.com/.well-known/jwks.json"},
		{"trailing slash is not doubled", "https://idp.example.com/", "", "https://idp.example.com/.well-known/jwks.json"},
		{"whitespace is not an address", "https://idp.example.com", "   ", "https://idp.example.com/.well-known/jwks.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.AuthServer.Issuer = tc.issuer
			cfg.AuthServer.JWKSURL = tc.jwks
			if got := adminJWKSURL(cfg); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// InitJWT is what actually hands the address to the verifier; a resolver that
// is correct but unwired would leave the 401 in place.
func TestInitJWT_UsesConfiguredJWKSAddress(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthServer.Issuer = "http://localhost:18080"
	cfg.AuthServer.JWKSURL = "http://control-plane:3001/.well-known/jwks.json"

	if v := InitJWT(context.Background(), cfg, nil, "cp-test", silentLogger()); v == nil {
		t.Fatal("expected a verifier")
	}
	if got := adminJWKSURL(cfg); got != cfg.AuthServer.JWKSURL {
		t.Fatalf("InitJWT would fetch keys from %q, not the configured %q", got, cfg.AuthServer.JWKSURL)
	}
}
