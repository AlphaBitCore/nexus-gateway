package clienttls

import (
	"crypto/tls"
	"net/http"
	"testing"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/httpclient"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

func TestWithTLSConfig_NilConfigClears(t *testing.T) {
	hc := nexushttp.New(nexushttp.Config{ForceHTTP2: nexushttp.On()})
	if err := WithTLSConfig(hc, &tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatalf("WithTLSConfig (set): %v", err)
	}
	if err := WithTLSConfig(hc, nil); err != nil {
		t.Fatalf("WithTLSConfig (clear): %v", err)
	}
	tr, err := UnderlyingTransport(hc)
	if err != nil {
		t.Fatalf("UnderlyingTransport: %v", err)
	}
	if tr.TLSClientConfig != nil {
		t.Fatalf("nil cfg should clear TLSClientConfig, got %+v", tr.TLSClientConfig)
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

// nilUnwrapRT is a RoundTripper that exposes Unwrap() returning nil — the
// rare termination case the loop in underlyingHTTPTransport must guard
// against.
type nilUnwrapRT struct{}

func (nilUnwrapRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (nilUnwrapRT) Unwrap() http.RoundTripper                       { return nil }

func TestUnderlyingTransport_UnwrapReturningNilTerminates(t *testing.T) {
	// Without the `if rt == nil { return nil }` guard inside
	// underlyingHTTPTransport's Unwrap loop, this would NPE the next
	// iteration's type assertion. Pin the contract: returns nil-transport,
	// error surfaced to caller.
	c := &http.Client{Transport: nilUnwrapRT{}}
	if _, err := UnderlyingTransport(c); err == nil {
		t.Fatal("nil-Unwrap chain should error, not panic")
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
