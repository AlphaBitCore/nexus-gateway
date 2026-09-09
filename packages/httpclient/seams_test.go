package httpclient

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The two Config seams exist because their absence was being answered with an
// allowlist entry. Each test asserts the observable outcome, not the field.

// TestNoTimeout_IsDistinguishableFromUnset pins the ambiguity the field was
// added to remove: Timeout == 0 has to mean "unset, give me the default" for
// every caller that never thought about it, and there has to be a way to say
// "none" for a stream whose total duration is not a meaningful bound.
func TestNoTimeout_IsDistinguishableFromUnset(t *testing.T) {
	if got := New(Config{}).Timeout; got != 30*time.Second {
		t.Errorf("an unset Timeout must default: got %v, want 30s", got)
	}
	if got := New(Config{NoTimeout: true}).Timeout; got != 0 {
		t.Errorf("NoTimeout must leave no client deadline: got %v", got)
	}
	// NoTimeout wins over a stray value rather than silently keeping it: a
	// caller that sets both has said two things, and the explicit one is the
	// one that cannot be an accident of a zero value.
	if got := New(Config{NoTimeout: true, Timeout: 9 * time.Second}).Timeout; got != 0 {
		t.Errorf("NoTimeout must win over a set Timeout: got %v", got)
	}
}

// stubRT answers everything with 418 so a test can tell whose transport ran.
type stubRT struct{ hits int }

func (s *stubRT) RoundTrip(*http.Request) (*http.Response, error) {
	s.hits++
	return &http.Response{StatusCode: http.StatusTeapot, Body: http.NoBody, Header: http.Header{}}, nil
}

// TestTransportHook_ReplacesTheDialerButNotTheWrapper is the property that makes
// the hook safe to hand out: a caller may take over what dials, and still cannot
// opt out of the logging and request-id layer. That layer is the part of this
// factory the rule exists to guarantee; the pool is a default.
func TestTransportHook_ReplacesTheDialerButNotTheWrapper(t *testing.T) {
	stub := &stubRT{}
	var handedBase bool
	c := New(Config{
		Caller: "seam-test",
		Transport: func(base *http.Transport) http.RoundTripper {
			handedBase = base != nil
			return stub
		},
	})

	if !handedBase {
		t.Error("the hook must receive the tuned transport, so a caller can wrap it")
	}
	if _, ok := c.Transport.(*loggingTransport); !ok {
		t.Fatalf("the wrapper must stay outermost: got %T", c.Transport)
	}
	if Base(c.Transport) != http.RoundTripper(stub) {
		t.Errorf("what dials must be what the hook returned: got %T", Base(c.Transport))
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	req = req.WithContext(t.Context())
	resp, err := c.Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if stub.hits != 1 {
		t.Errorf("the hook's transport must actually carry the request: %d hits", stub.hits)
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418 from the stub", resp.StatusCode)
	}
}

// TestBase_StopsAtATransportItDoesNotOwn keeps Base from over-peeling. A caller
// wrapping its own retry layer needs Base to stop there, not to strip it: the
// nexus-cli test asserts its RetryTransport through this.
func TestBase_StopsAtATransportItDoesNotOwn(t *testing.T) {
	stub := &stubRT{}
	if got := Base(stub); got != http.RoundTripper(stub) {
		t.Errorf("Base must return a transport with no Unwrap unchanged: got %T", got)
	}
	if got := Base(nil); got != nil {
		t.Errorf("Base(nil) = %v, want nil", got)
	}
}

// TestTLSFloor_IsStatedNotInherited: Go's client default has been TLS 1.2 for
// several releases, so this asserts the floor is written down rather than
// assumed — inheriting it leaves every caller relying on a default.
func TestTLSFloor_IsStatedNotInherited(t *testing.T) {
	tr, ok := Base(New(Config{}).Transport).(*http.Transport)
	if !ok {
		t.Fatalf("Base did not reach the tuned transport: %T", New(Config{}).Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("every client must carry an explicit TLS config")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
}
