package relay

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWithClientCert_NilClient(t *testing.T) {
	if err := WithClientCert(nil, tls.Certificate{}); err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

func TestWithClientCert_RejectsForeignTransport(t *testing.T) {
	c := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	err := WithClientCert(c, tls.Certificate{})
	if err == nil {
		t.Fatal("expected error for non-*http.Transport, got nil")
	}
}

func TestWithTLSConfig_NilClient(t *testing.T) {
	if err := WithTLSConfig(nil, &tls.Config{}); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestWithTLSConfig_RejectsForeignTransport(t *testing.T) {
	c := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	if err := WithTLSConfig(c, &tls.Config{}); err == nil {
		t.Fatal("expected error for non-*http.Transport")
	}
}

func TestWithTLSConfig_ClonesAndAssigns(t *testing.T) {
	hc := nexushttp.New(nexushttp.Config{ForceHTTP2: nexushttp.On()})
	cfg := &tls.Config{InsecureSkipVerify: true}
	if err := WithTLSConfig(hc, cfg); err != nil {
		t.Fatalf("WithTLSConfig: %v", err)
	}
	tr, err := UnderlyingTransport(hc)
	if err != nil {
		t.Fatalf("UnderlyingTransport: %v", err)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLSClientConfig not applied: %+v", tr.TLSClientConfig)
	}
	// Mutating the original must not bleed into the transport (Clone semantics).
	cfg.InsecureSkipVerify = false
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("transport TLSClientConfig was not cloned")
	}
}

func TestUnderlyingTransport_NilClient(t *testing.T) {
	if _, err := UnderlyingTransport(nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestUnderlyingTransport_RejectsForeignTransport(t *testing.T) {
	c := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	if _, err := UnderlyingTransport(c); err == nil {
		t.Fatal("expected error for non-*http.Transport")
	}
}

// nilUnwrapRT is a RoundTripper that exposes Unwrap() returning nil —
// the rare termination case the loop in underlyingHTTPTransport must
// guard against.
type nilUnwrapRT struct{}

func (nilUnwrapRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (nilUnwrapRT) Unwrap() http.RoundTripper                       { return nil }

func TestUnderlyingTransport_UnwrapReturningNilTerminates(t *testing.T) {
	// Without the `if rt == nil { return nil }` guard inside
	// underlyingHTTPTransport's Unwrap loop, this would NPE the
	// next iteration's type assertion. Pin the contract:
	// returns nil-transport, error surfaced to caller.
	c := &http.Client{Transport: nilUnwrapRT{}}
	_, err := UnderlyingTransport(c)
	if err == nil {
		t.Fatal("nil-Unwrap chain should error, not panic")
	}
}

func TestWithClientCert_UnwrapReturningNilErrors(t *testing.T) {
	c := &http.Client{Transport: nilUnwrapRT{}}
	if err := WithClientCert(c, tls.Certificate{}); err == nil {
		t.Fatal("expected error on nil-Unwrap chain")
	}
}

func TestWithTLSConfig_UnwrapReturningNilErrors(t *testing.T) {
	c := &http.Client{Transport: nilUnwrapRT{}}
	if err := WithTLSConfig(c, &tls.Config{}); err == nil {
		t.Fatal("expected error on nil-Unwrap chain")
	}
}

func TestUnderlyingTransport_ReturnsTransport(t *testing.T) {
	hc := nexushttp.New(nexushttp.Config{})
	tr, err := UnderlyingTransport(hc)
	if err != nil {
		t.Fatalf("UnderlyingTransport: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil *http.Transport")
	}
}

func TestWithClientCert_PresentsCertOnNextDial(t *testing.T) {
	clientCertPEM, clientKeyPEM := generateSelfSignedCert(t, "test-client")
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(clientCertPEM) {
		t.Fatal("failed to add client cert to CA pool")
	}

	var sawCert bool
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  caPool,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no client cert presented")
			}
			sawCert = true
			return nil
		},
	}
	ts.StartTLS()
	defer ts.Close()

	hc := nexushttp.New(nexushttp.Config{
		ForceHTTP2: nexushttp.On(),
	})
	tr, err := UnderlyingTransport(hc)
	if err != nil {
		t.Fatalf("UnderlyingTransport: %v", err)
	}
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	if err := WithClientCert(hc, clientCert); err != nil {
		t.Fatalf("WithClientCert: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if !sawCert {
		t.Fatal("server did not see client cert after WithClientCert")
	}
}
