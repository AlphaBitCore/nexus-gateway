package issuer

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
)

// TestCACert_ReturnsInMemoryCert asserts that CACert() returns the loaded
// CA certificate (not nil) and that it is the same object as the internal
// field — callers that build trust pools from CACert() must get the
// authoritative cert, not a copy that might lag rotation.
func TestCACert_ReturnsInMemoryCert(t *testing.T) {
	certPath, keyPath, err := testutil.WriteTestCA(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	got := iss.CACert()
	if got == nil {
		t.Fatal("CACert() returned nil")
	}
	// Must be the same pointer as the internal field — not a copy.
	if got != iss.caCert {
		t.Error("CACert() must return the internal caCert pointer, not a copy")
	}
	// Must be a CA cert.
	if !got.IsCA {
		t.Error("CACert() returned a non-CA certificate")
	}
}
