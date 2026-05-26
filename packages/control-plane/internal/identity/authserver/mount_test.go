package authserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestMount_RegistersJWKSEndpoint is a smoke test that verifies Mount wires
// the JWKS route when a keystore is supplied. It does not exercise the
// OAuth/login routes because those require a DB; see e2e_test.go for the
// full flow.
func TestMount_RegistersJWKSEndpoint(t *testing.T) {
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	if _, err := ks.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	e := echo.New()
	authserver.Mount(e, authserver.Deps{Keystore: ks})
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("route /.well-known/jwks.json not registered")
	}
}
