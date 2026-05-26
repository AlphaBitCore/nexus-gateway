package oauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
)

// discoveryBody mirrors the RFC 8414 / OpenID Connect Discovery wire format
// the handler must emit. Kept in the test file so a field rename in the
// production struct shows up here as a compile/JSON-decode failure.
type discoveryBody struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	IntrospectionEndpoint             string   `json:"introspection_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
}

// serveDiscovery wires up a fresh Echo + handler for the given issuer and
// executes a GET /.well-known/openid-configuration through ServeHTTP so the
// full middleware chain runs exactly as in production.
func serveDiscovery(t *testing.T, issuer string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.GET("/.well-known/openid-configuration", oauth.DiscoveryHandler(issuer))
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestDiscovery_ReturnsConfigurationForIssuer(t *testing.T) {
	const issuer = "https://auth.example.com"
	rec := serveDiscovery(t, issuer)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body discoveryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	if body.Issuer != issuer {
		t.Errorf("issuer=%q, want %q", body.Issuer, issuer)
	}
	if want := issuer + "/oauth/authorize"; body.AuthorizationEndpoint != want {
		t.Errorf("authorization_endpoint=%q, want %q", body.AuthorizationEndpoint, want)
	}
	if want := issuer + "/oauth/token"; body.TokenEndpoint != want {
		t.Errorf("token_endpoint=%q, want %q", body.TokenEndpoint, want)
	}
	if want := issuer + "/oauth/introspect"; body.IntrospectionEndpoint != want {
		t.Errorf("introspection_endpoint=%q, want %q", body.IntrospectionEndpoint, want)
	}
	// revocation_endpoint must advertise the RFC 7009 endpoint that the
	// authserver mounts at /oauth/revoke (see authserver/mount.go).
	if want := issuer + "/oauth/revoke"; body.RevocationEndpoint != want {
		t.Errorf("revocation_endpoint=%q, want %q", body.RevocationEndpoint, want)
	}
	if want := issuer + "/.well-known/jwks.json"; body.JWKSURI != want {
		t.Errorf("jwks_uri=%q, want %q", body.JWKSURI, want)
	}

	// Exact content + length checks on each array — not just len>0 — so a
	// refactor that adds an unintended value (e.g. an "id_token" slipping
	// into response_types_supported) fails here rather than shipping
	// silently.
	if want := []string{"code"}; !reflect.DeepEqual(body.ResponseTypesSupported, want) {
		t.Errorf("response_types_supported=%v, want %v", body.ResponseTypesSupported, want)
	}
	if want := []string{"authorization_code", "refresh_token"}; !reflect.DeepEqual(body.GrantTypesSupported, want) {
		t.Errorf("grant_types_supported=%v, want %v", body.GrantTypesSupported, want)
	}
	if want := []string{"none", "client_secret_basic"}; !reflect.DeepEqual(body.TokenEndpointAuthMethodsSupported, want) {
		t.Errorf("token_endpoint_auth_methods_supported=%v, want %v", body.TokenEndpointAuthMethodsSupported, want)
	}
	if want := []string{"S256"}; !reflect.DeepEqual(body.CodeChallengeMethodsSupported, want) {
		t.Errorf("code_challenge_methods_supported=%v, want %v", body.CodeChallengeMethodsSupported, want)
	}
	if want := []string{"RS256"}; !reflect.DeepEqual(body.IDTokenSigningAlgValuesSupported, want) {
		t.Errorf("id_token_signing_alg_values_supported=%v, want %v", body.IDTokenSigningAlgValuesSupported, want)
	}
}

func TestDiscovery_TrimsTrailingSlashFromIssuer(t *testing.T) {
	// A trailing slash on the issuer would double-slash every derived URL
	// ("https://auth.example.com//oauth/token"), breaking the iss-claim
	// comparison most clients perform. Handler must normalize once at
	// construction time.
	rec := serveDiscovery(t, "https://auth.example.com/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body discoveryBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Issuer != "https://auth.example.com" {
		t.Errorf("issuer=%q, want %q (trailing slash not trimmed)", body.Issuer, "https://auth.example.com")
	}
	if strings.Contains(body.TokenEndpoint, "//oauth") {
		t.Errorf("token_endpoint=%q contains // after host", body.TokenEndpoint)
	}
}

func TestDiscovery_CacheControlPublic(t *testing.T) {
	rec := serveDiscovery(t, "https://auth.example.com")
	if got, want := rec.Header().Get("Cache-Control"), "public, max-age=300"; got != want {
		t.Errorf("Cache-Control=%q, want %q", got, want)
	}
}

func TestDiscovery_ContentTypeJSON(t *testing.T) {
	rec := serveDiscovery(t, "https://auth.example.com")
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type=%q, want application/json...", got)
	}
}

func TestDiscovery_EmptyIssuerReturns500(t *testing.T) {
	// Empty issuer is a wiring bug — the handler must fail loudly at request
	// time rather than emit a document whose iss is "" or whose endpoints
	// begin with "/oauth/...". Regression guard for future refactors of
	// mount.go.
	rec := serveDiscovery(t, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
}
