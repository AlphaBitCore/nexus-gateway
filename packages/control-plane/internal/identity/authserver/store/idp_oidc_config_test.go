package store_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// TestDecodeOIDCConfig_NilReturnsNil asserts nil input → nil output
// (defensive guard for callers that pass a result that may be missing).
func TestDecodeOIDCConfig_NilReturnsNil(t *testing.T) {
	if got := store.DecodeOIDCConfig(nil); got != nil {
		t.Fatalf("nil idp should yield nil cfg; got %+v", got)
	}
}

// TestDecodeOIDCConfig_NonOIDCReturnsNil asserts the function refuses
// to coerce a non-OIDC row into an OIDCConfig — local IdPs have no
// JWKS/issuer fields and would render meaningless if decoded.
func TestDecodeOIDCConfig_NonOIDCReturnsNil(t *testing.T) {
	idp := &store.IdentityProvider{Type: "local", Name: "Local"}
	if got := store.DecodeOIDCConfig(idp); got != nil {
		t.Fatalf("local idp should yield nil cfg; got %+v", got)
	}
}

// TestDecodeOIDCConfig_FieldsRoundTrip asserts every JSON key from the
// admin write-path lands on the typed struct.
func TestDecodeOIDCConfig_FieldsRoundTrip(t *testing.T) {
	idp := &store.IdentityProvider{
		Type:    "oidc",
		Name:    "Okta",
		Enabled: true,
		Config: map[string]any{
			"issuer":       "https://idp.example",
			"jwksUri":      "https://idp.example/.well-known/jwks",
			"clientId":     "cid",
			"clientSecret": "csec",
			"redirectUri":  "https://cp.nexus.ai/oidc/callback",
			"authorizeUrl": "https://idp.example/authorize",
			"tokenUrl":     "https://idp.example/token",
			"audience":     "aud",
			"emailClaim":   "preferred_email",
		},
	}
	got := store.DecodeOIDCConfig(idp)
	if got == nil {
		t.Fatal("oidc idp should decode to non-nil cfg")
		return
	}
	if got.Issuer != "https://idp.example" || got.JwksURI != "https://idp.example/.well-known/jwks" {
		t.Fatalf("issuer/jwksUri not round-tripped: %+v", got)
	}
	if got.ClientID != "cid" || got.ClientSecret != "csec" {
		t.Fatalf("client credentials not round-tripped: %+v", got)
	}
	if got.RedirectURI != "https://cp.nexus.ai/oidc/callback" {
		t.Fatalf("redirectUri not round-tripped: %q", got.RedirectURI)
	}
	if got.AuthorizeURL != "https://idp.example/authorize" || got.TokenURL != "https://idp.example/token" {
		t.Fatalf("authorize/tokenURL not round-tripped: %+v", got)
	}
	if got.Audience != "aud" {
		t.Fatalf("audience not round-tripped: %q", got.Audience)
	}
	if got.EmailClaim != "preferred_email" {
		t.Fatalf("emailClaim override not applied; got %q", got.EmailClaim)
	}
	if !got.Enabled {
		t.Fatal("Enabled must be lifted from parent idp.Enabled")
	}
	if got.DisplayName != "Okta" {
		t.Fatalf("DisplayName must be lifted from idp.Name; got %q", got.DisplayName)
	}
}

// TestDecodeOIDCConfig_EmailClaimDefaultsToEmail asserts the documented
// default — when the IdP config omits emailClaim, the decoder fills in
// "email" so callback handlers don't have to repeat the fallback.
func TestDecodeOIDCConfig_EmailClaimDefaultsToEmail(t *testing.T) {
	idp := &store.IdentityProvider{
		Type:    "oidc",
		Name:    "Generic",
		Enabled: true,
		Config:  map[string]any{"issuer": "https://x"},
	}
	got := store.DecodeOIDCConfig(idp)
	if got == nil {
		t.Fatal("expected non-nil cfg")
		return
	}
	if got.EmailClaim != "email" {
		t.Fatalf("emailClaim default = %q, want %q", got.EmailClaim, "email")
	}
}

// TestDecodeOIDCConfig_EmptyConfigJustLiftsParent asserts that when
// idp.Config is empty/nil, the decoder still emits a non-nil cfg with
// Enabled + DisplayName lifted from the parent — the OIDC handlers
// can then report "issuer unset" cleanly rather than nil-deref.
func TestDecodeOIDCConfig_EmptyConfigJustLiftsParent(t *testing.T) {
	idp := &store.IdentityProvider{Type: "oidc", Name: "Empty", Enabled: false, Config: nil}
	got := store.DecodeOIDCConfig(idp)
	if got == nil {
		t.Fatal("expected non-nil cfg")
		return
	}
	if got.Issuer != "" {
		t.Fatalf("issuer should be empty; got %q", got.Issuer)
	}
	if got.Enabled {
		t.Fatal("Enabled should reflect parent (false)")
	}
	if got.DisplayName != "Empty" {
		t.Fatalf("DisplayName should be lifted; got %q", got.DisplayName)
	}
	if got.EmailClaim != "email" {
		t.Fatalf("default emailClaim should still apply on empty config; got %q", got.EmailClaim)
	}
}
