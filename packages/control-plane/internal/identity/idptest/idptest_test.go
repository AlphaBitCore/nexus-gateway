package idptest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
)

func ctx() context.Context { return context.Background() }

// hijackShort writes a 200 with an oversized Content-Length then closes the
// socket early, so the client's io.ReadAll returns a non-nil read error.
func hijackShort(w http.ResponseWriter, _ *http.Request) {
	conn, bufrw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort")
	_ = bufrw.Flush()
	_ = conn.Close()
}

// ---- ProbeOIDC ----

func TestProbeOIDC_Validation(t *testing.T) {
	if r := ProbeOIDC(ctx(), map[string]any{}); r.OK || r.Error == "" {
		t.Fatal("missing issuer must fail")
	}
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": "not_a_url"}); r.OK || r.Error == "" {
		t.Fatal("invalid issuer URL must fail")
	}
}

func TestProbeOIDC_Success_ViaDiscovery(t *testing.T) {
	// The discovery server runs on 127.0.0.1; install a resolver that skips the
	// SSRF host guard for this loopback test, restoring the guarded default after.
	prev := NewProbeResolver
	NewProbeResolver = func() *oidcdisco.Resolver {
		return oidcdisco.NewResolver(oidcdisco.WithInsecureSkipHostCheck())
	}
	t.Cleanup(func() { NewProbeResolver = prev })

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jwks_uri":"` + srv.URL + `/jwks","token_endpoint":"` + srv.URL + `/tok","authorization_endpoint":"` + srv.URL + `/auth"}`))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1"}]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	r := ProbeOIDC(ctx(), map[string]any{"issuer": srv.URL})
	if !r.OK || r.Detail["keysFound"].(int) != 1 || r.Detail["discoveryResolved"] != true {
		t.Fatalf("discovery success expected: %+v", r)
	}
}

func TestProbeOIDC_ExplicitEndpoints_SkipDiscovery(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kid":"k1"}]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	// All three present → discovery branch skipped.
	r := ProbeOIDC(ctx(), map[string]any{
		"issuer": srv.URL, "jwksUri": srv.URL + "/jwks", "tokenUrl": "https://x/tok", "authorizeUrl": "https://x/auth",
	})
	if !r.OK {
		t.Fatalf("explicit-endpoints success expected: %+v", r)
	}
}

func TestProbeOIDC_DiscoveryErrors(t *testing.T) {
	// Fetch failure (dead port).
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": "http://127.0.0.1:1"}); r.OK || r.Error == "" {
		t.Fatal("discovery fetch failure must error")
	}
	// Non-200.
	s500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer s500.Close()
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": s500.URL}); r.OK {
		t.Fatal("discovery 500 must error")
	}
	// Parse error (bad JSON).
	sBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{not json")) }))
	defer sBad.Close()
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": sBad.URL}); r.OK {
		t.Fatal("discovery parse error must error")
	}
	// Read error (short body).
	sShort := httptest.NewServer(http.HandlerFunc(hijackShort))
	defer sShort.Close()
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": sShort.URL}); r.OK {
		t.Fatal("discovery read error must error")
	}
	// Discovery returns no jwks_uri → unresolved.
	sEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer sEmpty.Close()
	if r := ProbeOIDC(ctx(), map[string]any{"issuer": sEmpty.URL}); r.OK || r.Error == "" {
		t.Fatal("missing jwks_uri must error")
	}
}

func TestProbeOIDC_JWKSErrors(t *testing.T) {
	skip := func(jwks string) map[string]any {
		return map[string]any{"issuer": "https://i", "jwksUri": jwks, "tokenUrl": "https://t", "authorizeUrl": "https://a"}
	}
	// Fetch failure.
	if r := ProbeOIDC(ctx(), skip("http://127.0.0.1:1/jwks")); r.OK {
		t.Fatal("jwks fetch failure must error")
	}
	// Non-200.
	s500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer s500.Close()
	if r := ProbeOIDC(ctx(), skip(s500.URL)); r.OK {
		t.Fatal("jwks 503 must error")
	}
	// Read error.
	sShort := httptest.NewServer(http.HandlerFunc(hijackShort))
	defer sShort.Close()
	if r := ProbeOIDC(ctx(), skip(sShort.URL)); r.OK {
		t.Fatal("jwks read error must error")
	}
	// Parse error.
	sBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{bad")) }))
	defer sBad.Close()
	if r := ProbeOIDC(ctx(), skip(sBad.URL)); r.OK {
		t.Fatal("jwks parse error must error")
	}
	// No keys.
	sNoKeys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"keys":[]}`)) }))
	defer sNoKeys.Close()
	if r := ProbeOIDC(ctx(), skip(sNoKeys.URL)); r.OK || r.Error == "" {
		t.Fatal("empty JWKS must error")
	}
}

// ---- ProbeSAML ----

func genCertPEM(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "idp.example"},
		Issuer:       pkix.Name{CommonName: "idp.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestProbeSAML(t *testing.T) {
	good := genCertPEM(t, time.Now().Add(24*time.Hour))
	expired := genCertPEM(t, time.Now().Add(-24*time.Hour))

	cases := []struct {
		name   string
		cfg    map[string]any
		wantOK bool
	}{
		{"missing fields", map[string]any{}, false},
		{"bad sso url", map[string]any{"entityId": "e", "ssoUrl": "not_a_url"}, false},
		{"missing cert", map[string]any{"entityId": "e", "ssoUrl": "https://idp/sso"}, false},
		{"bad pem", map[string]any{"entityId": "e", "ssoUrl": "https://idp/sso", "certificatePem": "not pem"}, false},
		{"bad cert bytes", map[string]any{"entityId": "e", "ssoUrl": "https://idp/sso", "certificatePem": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"}, false},
		{"expired cert", map[string]any{"entityId": "e", "ssoUrl": "https://idp/sso", "certificatePem": expired}, false},
		{"success", map[string]any{"entityId": "e", "ssoUrl": "https://idp/sso", "certificatePem": good}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ProbeSAML(ctx(), c.cfg)
			if r.OK != c.wantOK {
				t.Fatalf("%s: OK=%v want %v (err=%q)", c.name, r.OK, c.wantOK, r.Error)
			}
			if c.wantOK && r.Detail["certSubject"] == nil {
				t.Fatalf("%s: success must carry cert detail", c.name)
			}
		})
	}
}

// ---- Probe dispatcher ----

func TestProbe_Dispatch(t *testing.T) {
	if _, err := Probe(ctx(), "OIDC", map[string]any{}); err != nil {
		t.Fatalf("oidc dispatch: %v", err)
	}
	if _, err := Probe(ctx(), " saml ", map[string]any{}); err != nil {
		t.Fatalf("saml dispatch: %v", err)
	}
	if _, err := Probe(ctx(), "ldap", map[string]any{}); err == nil {
		t.Fatal("unsupported type must error")
	}
}
