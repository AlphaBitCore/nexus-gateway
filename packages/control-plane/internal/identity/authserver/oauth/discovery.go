package oauth

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// openIDConfig is the wire shape returned by /.well-known/openid-configuration.
// Fields and ordering follow OpenID Connect Discovery 1.0 + RFC 8414. Kept
// unexported because the handler is the only producer and tests decode into
// their own mirror struct.
type openIDConfig struct {
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

// DiscoveryHandler serves the OpenID Connect discovery document for the given
// issuer. The issuer is normalized (trailing slash trimmed) once at
// construction time and every derived endpoint URL is built from the
// normalized value, so callers may pass either form without breaking the
// iss-claim comparison that RPs perform.
//
// A zero issuer is treated as a wiring bug: the returned handler answers 500
// rather than emitting a document whose iss is "" or whose endpoints begin
// with "/oauth/...". Production mount code is expected to fail earlier, but
// this guardrail keeps a misconfigured deployment from silently handing out
// broken metadata.
func DiscoveryHandler(issuer string) echo.HandlerFunc {
	if issuer == "" {
		return func(c echo.Context) error {
			return echo.NewHTTPError(http.StatusInternalServerError, "issuer not configured")
		}
	}

	// Build the response value once and reuse per request. Marshalling runs
	// on each call via c.JSON, but the struct itself is immutable and safe
	// to share across goroutines.
	iss := strings.TrimRight(issuer, "/")
	doc := openIDConfig{
		Issuer:                            iss,
		AuthorizationEndpoint:             iss + "/oauth/authorize",
		TokenEndpoint:                     iss + "/oauth/token",
		IntrospectionEndpoint:             iss + "/oauth/introspect",
		RevocationEndpoint:                iss + "/oauth/revoke",
		JWKSURI:                           iss + "/.well-known/jwks.json",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_basic"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		IDTokenSigningAlgValuesSupported:  []string{"RS256"},
	}

	return func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "public, max-age=300")
		return c.JSON(http.StatusOK, doc)
	}
}
