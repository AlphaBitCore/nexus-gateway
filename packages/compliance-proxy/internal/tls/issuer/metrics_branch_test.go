package issuer

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// TestSignCert_MetricsObserved covers the `if metrics.CertSignMs != nil`
// branch in SignCert. The guard exists so the issuer can be used in tests
// and tool binaries without calling metrics.Register first. This test calls
// metrics.Register with a fresh Prometheus registerer so the branch fires
// and Observe() is called without panicking.
//
// Named failure mode: if the guard were removed, every test that uses
// issuer.SignCert without metrics initialized would panic on nil deref.
func TestSignCert_MetricsObserved(t *testing.T) {
	// Use an isolated Prometheus registry so this test doesn't conflict with
	// other tests that may have already registered the same metric names.
	pr := prometheus.NewRegistry()
	reg := registry.NewRegistry(pr)

	// Save and restore the global CertSignMs so other tests in the package
	// that run without metrics are not affected.
	prev := metrics.CertSignMs
	metrics.Register(reg)
	defer func() { metrics.CertSignMs = prev }()

	if metrics.CertSignMs == nil {
		t.Fatal("metrics.CertSignMs should be non-nil after Register")
	}

	certPath, keyPath, err := testutil.WriteTestCA(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// SignCert must call metrics.CertSignMs.With().Observe() without panicking.
	tlsCert, err := iss.SignCert("metrics-branch.example.com")
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	if tlsCert == nil {
		t.Fatal("expected non-nil cert")
	}
}
