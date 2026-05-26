package oauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

func TestJWKS_ReturnsAllActiveKeys(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	_, _ = ks.Generate()
	_, _ = ks.Generate()

	e := echo.New()
	e.GET("/.well-known/jwks.json", oauth.JWKSHandler(ks))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}

	var got struct {
		Keys []struct {
			KID string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(got.Keys))
	}
	for _, k := range got.Keys {
		if k.Kty != "RSA" || k.Alg != "RS256" || k.Use != "sig" {
			t.Fatalf("unexpected jwk: %+v", k)
		}
		if k.N == "" || k.E == "" {
			t.Fatalf("jwk missing n or e: %+v", k)
		}
	}
}
