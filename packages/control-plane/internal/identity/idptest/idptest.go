// Package idptest provides synchronous connectivity probes for external
// IdP configurations. The Probe* functions accept either a saved
// IdentityProvider row's config payload or a candidate (unsaved)
// payload from the UI's Add-IdP wizard and verify reachability without
// persisting state.
//
// Probes are bounded by a 10s context timeout per check. They are
// safe to call from request-handler goroutines.
package idptest

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
)

// Result is the structured outcome of a probe. The HTTP handler renders
// this verbatim as the body of POST /identity-providers[/:id]/test.
type Result struct {
	OK        bool                   `json:"ok"`
	Detail    map[string]interface{} `json:"detail,omitempty"`
	Error     string                 `json:"error,omitempty"`
	ElapsedMs int64                  `json:"elapsedMs"`
}

// ProbeOIDC validates that an OIDC IdP config is reachable by fetching
// either the explicit jwks_uri (when provided) or the discovery
// document at `<issuer>/.well-known/openid-configuration` and confirming
// it has the expected shape. Returns ok=true with the discovered fields
// or ok=false with a human-readable error.
func ProbeOIDC(ctx context.Context, cfg map[string]any) Result {
	start := time.Now()
	res := Result{}
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()

	issuer, _ := cfg["issuer"].(string)
	if issuer == "" {
		res.Error = "issuer is required"
		return res
	}
	if _, err := url.ParseRequestURI(issuer); err != nil {
		res.Error = fmt.Sprintf("issuer is not a valid URL: %v", err)
		return res
	}

	client := newClient(10 * time.Second)

	jwksCfg, _ := cfg["jwksUri"].(string)
	tokenCfg, _ := cfg["tokenUrl"].(string)
	authCfg, _ := cfg["authorizeUrl"].(string)
	// Resolve any missing endpoint from the issuer's discovery document. A
	// fresh resolver (no shared cache) keeps the probe honest: an admin who
	// just changed the issuer must see the live document, not a cached one.
	// The resolver carries the SSRF host guard: an admin probing an
	// internal/loopback issuer is rejected before any fetch. NewProbeResolver is
	// an exported test seam (overridden to skip the guard for loopback httptest
	// servers in both this package and external handler tests).
	eps, err := NewProbeResolver().Resolve(ctx, issuer, oidcdisco.Endpoints{
		AuthorizeURL: authCfg,
		TokenURL:     tokenCfg,
		JwksURI:      jwksCfg,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	jwksURI := eps.JwksURI
	tokenURL := eps.TokenURL
	authorizeURL := eps.AuthorizeURL

	if jwksURI == "" {
		res.Error = "jwks_uri could not be resolved from config or discovery"
		return res
	}

	// Verify the JWKS endpoint is reachable + parseable.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	resp, err := client.Do(req)
	if err != nil {
		res.Error = fmt.Sprintf("JWKS fetch failed: %v", err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		res.Error = fmt.Sprintf("JWKS returned HTTP %d", resp.StatusCode)
		return res
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		res.Error = fmt.Sprintf("JWKS read: %v", err)
		return res
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		res.Error = fmt.Sprintf("JWKS parse: %v", err)
		return res
	}
	if len(jwks.Keys) == 0 {
		res.Error = "JWKS document has no keys"
		return res
	}

	res.OK = true
	res.Detail = map[string]any{
		"issuer":            issuer,
		"jwksUri":           jwksURI,
		"authorizeUrl":      authorizeURL,
		"tokenUrl":          tokenURL,
		"keysFound":         len(jwks.Keys),
		"discoveryResolved": tokenURL != "" && authorizeURL != "",
	}
	return res
}

// ProbeSAML validates a SAML IdP config by parsing the configured
// certificate PEM and checking that the SSO URL is well-formed. Live
// SAML metadata fetch is performed only if no certificate is supplied
// (the wizard's typical case is "I pasted in the metadata XML").
func ProbeSAML(ctx context.Context, cfg map[string]any) Result {
	start := time.Now()
	res := Result{}
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()

	entityID, _ := cfg["entityId"].(string)
	ssoURL, _ := cfg["ssoUrl"].(string)
	certPEM, _ := cfg["certificatePem"].(string)

	if entityID == "" || ssoURL == "" {
		res.Error = "entityId and ssoUrl are required"
		return res
	}
	if _, err := url.ParseRequestURI(ssoURL); err != nil {
		res.Error = fmt.Sprintf("ssoUrl is not a valid URL: %v", err)
		return res
	}

	if certPEM == "" {
		res.Error = "certificatePem is required (paste the IdP signing cert)"
		return res
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		res.Error = "certificatePem is not valid PEM"
		return res
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		res.Error = fmt.Sprintf("certificate parse failed: %v", err)
		return res
	}
	if time.Now().After(cert.NotAfter) {
		res.Error = fmt.Sprintf("certificate expired on %s", cert.NotAfter.Format(time.RFC3339))
		return res
	}

	res.OK = true
	res.Detail = map[string]any{
		"entityId":     entityID,
		"ssoUrl":       ssoURL,
		"certSubject":  cert.Subject.String(),
		"certIssuer":   cert.Issuer.String(),
		"certNotAfter": cert.NotAfter.Format(time.RFC3339),
	}
	_ = ctx
	return res
}

// Probe is a protocol-dispatching helper used by the test endpoints
// when the IdP config is fed in as a structured payload.
func Probe(ctx context.Context, idpType string, cfg map[string]any) (Result, error) {
	switch strings.ToLower(strings.TrimSpace(idpType)) {
	case "oidc":
		return ProbeOIDC(ctx, cfg), nil
	case "saml":
		return ProbeSAML(ctx, cfg), nil
	default:
		return Result{}, fmt.Errorf("unsupported idp type: %q", idpType)
	}
}

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// NewProbeResolver builds the discovery resolver the OIDC probe uses. It is an
// exported package var so both internal and external tests can substitute a
// resolver that skips the SSRF host guard (their discovery server runs on
// 127.0.0.1); production keeps the guarded default. Never use a
// guard-disabled resolver outside tests.
var NewProbeResolver = func() *oidcdisco.Resolver { return oidcdisco.NewResolver() }
