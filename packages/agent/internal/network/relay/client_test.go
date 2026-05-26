package relay

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// newTestClient builds a Client whose Transport trusts the given test
// server's TLS cert and registers metrics into a fresh registry.
// Returns the client and the opsmetrics registry the client emits into so
// callers can assert on dial / handshake counters by name + dimension.
func newTestClient(t *testing.T) (*Client, *opsmetrics.Registry) {
	t.Helper()
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	c, err := New(Config{
		UserAgent:   "nexus-agent/test",
		OpsRegistry: reg,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	return c, reg
}

// sampleValue scans samples from reg.Collect() and returns the value of the
// first Sample matching (name, dim). dim follows opsmetrics.joinDimension's
// stable sorted "k=v;k=v" form. Returns 0 when no match exists.
func sampleValue(reg *opsmetrics.Registry, name, dim string) float64 {
	for _, s := range reg.Collect() {
		if s.Name == name && s.DimensionKey == dim {
			return s.Value
		}
	}
	return 0
}

func TestNew_RequiresOpsRegistry(t *testing.T) {
	// OpsRegistry is non-optional: relay's metrics drive Hub-side
	// dial / handshake counters. Missing-registry must fail-fast
	// at construction.
	_, err := New(Config{UserAgent: "x"})
	if err == nil {
		t.Fatal("nil OpsRegistry should error")
	}
}

func TestClient_HTTPClientExposesUnderlying(t *testing.T) {
	// Tests + WithClientCert helper rely on Client.HTTPClient() returning
	// the same *http.Client used for Do. Without this, WithClientCert
	// callers would mutate a different client than Do sends through.
	c, _ := newTestClient(t)
	hc := c.HTTPClient()
	if hc == nil {
		t.Fatal("HTTPClient returned nil")
	}
	if hc != c.httpClient {
		t.Errorf("HTTPClient returned %p, internal %p — must be the same instance", hc, c.httpClient)
	}
}

func TestClient_Do_HappyPath(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "nexus-agent/") {
			t.Errorf("User-Agent = %q, want prefix nexus-agent/", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()

	c, _ := newTestClient(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestClient_Do_DialAccounting(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c, reg := newTestClient(t)
	host := strings.TrimPrefix(ts.URL, "https://")
	if i := strings.Index(host, ":"); i > 0 {
		host = host[:i]
	}

	for i := range 10 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// opsmetrics.joinDimension renders labels alphabetically as "k=v;k=v";
	// the relay registers ["host","mode"] so the sorted form is host=...;mode=...
	dimNew := "host=" + host + ";mode=new"
	dimReused := "host=" + host + ";mode=reused"
	if got := sampleValue(reg, "relay.dial_total", dimNew); got != 1 {
		t.Errorf("dials{mode=new} = %v, want 1", got)
	}
	if got := sampleValue(reg, "relay.dial_total", dimReused); got != 9 {
		t.Errorf("dials{mode=reused} = %v, want 9", got)
	}
	// Exactly one TLS handshake should have happened — HTTP/2 multiplex
	// keeps subsequent requests on the same conn.
	if got := sampleValue(reg, "relay.handshake_total", ""); got != 1 {
		t.Errorf("handshake_total = %v, want 1 (single TLS handshake reused via H2)", got)
	}
}
