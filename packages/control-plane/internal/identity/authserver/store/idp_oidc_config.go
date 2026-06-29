package store

import (
	"github.com/goccy/go-json"
)

// OIDCConfig is the runtime view of an IdentityProvider row's OIDC
// config blob — decoded from the `config` JSONB column into Go fields
// the login handlers can consume.
//
// Field names mirror the lowerCamelCase JSON keys produced by the
// admin UI's CRUD flow, so the JSON tags don't surprise. The struct
// is intentionally small — only what the OIDC begin / callback /
// token-exchange code needs.
type OIDCConfig struct {
	Enabled      bool   `json:"-"` // copied from the parent IdentityProvider.Enabled
	DisplayName  string `json:"-"` // copied from the parent IdentityProvider.Name
	Issuer       string `json:"issuer"`
	JwksURI      string `json:"jwksUri"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	// RedirectURI is the callback URL the OIDC provider redirects to
	// after auth. Must match what was registered with the provider.
	RedirectURI  string `json:"redirectUri"`
	AuthorizeURL string `json:"authorizeUrl"`
	TokenURL     string `json:"tokenUrl"`
	Audience     string `json:"audience"`
	EmailClaim   string `json:"emailClaim"`
	GroupClaim   string `json:"groupClaim"`
	// AuthorizeParams are extra key/value pairs appended verbatim to the IdP
	// authorization request — e.g. Auth0's required `organization`, or
	// `prompt`/`connection`/`audience`. Admin-configured per IdP so provider
	// quirks stay in config, not code. Reserved OAuth params (response_type,
	// client_id, redirect_uri, scope, state) cannot be overridden.
	AuthorizeParams []OIDCAuthorizeParam `json:"authorizeParams"`
}

// OIDCAuthorizeParam is one extra authorization-request query parameter.
type OIDCAuthorizeParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DecodeOIDCConfig converts an IdentityProvider row into a runtime
// OIDCConfig. Returns nil if the row isn't an OIDC type. The Enabled
// + DisplayName fields are lifted from the parent row so the login
// handlers don't need to thread two values around.
func DecodeOIDCConfig(idp *IdentityProvider) *OIDCConfig {
	if idp == nil || idp.Type != "oidc" {
		return nil
	}
	var cfg OIDCConfig
	if len(idp.Config) > 0 {
		// IdentityProvider.Config is map[string]any in this struct;
		// re-marshal then unmarshal into the typed shape.
		raw, _ := json.Marshal(idp.Config)
		_ = json.Unmarshal(raw, &cfg)
	}
	cfg.Enabled = idp.Enabled
	cfg.DisplayName = idp.Name
	if cfg.EmailClaim == "" {
		cfg.EmailClaim = "email"
	}
	return &cfg
}
