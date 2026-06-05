package store

import (
	"encoding/json"
)

// SAMLConfig is the runtime view of a SAML IdentityProvider row's config
// blob — decoded from the `config` JSONB column into the fields the SAML
// login handlers consume. Field names mirror the lowerCamelCase JSON keys
// the admin UI's IdP CRUD flow produces (entityId / ssoUrl / certificatePem),
// plus the attribute-name mappings used to read the email and group values
// out of the assertion.
type SAMLConfig struct {
	Enabled     bool   `json:"-"` // copied from the parent IdentityProvider.Enabled
	DisplayName string `json:"-"` // copied from the parent IdentityProvider.Name

	// EntityID is the IdP's SAML entityID (the issuer of its assertions).
	EntityID string `json:"entityId"`
	// SSOURL is the IdP's HTTP-POST SingleSignOnService endpoint that the
	// SP-initiated AuthnRequest is posted to.
	SSOURL string `json:"ssoUrl"`
	// Certificate is the IdP's signing certificate (PEM, or bare base64 DER)
	// used to verify the XML signature on the response/assertion.
	Certificate string `json:"certificatePem"`

	// EmailAttr names the assertion attribute carrying the user's email.
	// Defaults to "email".
	EmailAttr string `json:"emailAttribute"`
	// GroupsAttr names the assertion attribute carrying the user's group
	// memberships (resolved through IdpGroupMapping). Defaults to "groups".
	GroupsAttr string `json:"groupsAttribute"`

	// SSOParams are extra key/value pairs appended verbatim to the IdP SSO URL
	// when the SP-initiated AuthnRequest is delivered — e.g. Auth0
	// Organizations' required `organization`. Mirrors OIDCConfig.AuthorizeParams
	// so a per-IdP provider quirk stays in config, not code. The SAML protocol
	// params (SAMLRequest, RelayState) are reserved and cannot be overridden.
	SSOParams []SAMLSSOParam `json:"ssoParams"`
}

// SAMLSSOParam is one extra query parameter appended to the IdP SSO URL on the
// SP-initiated AuthnRequest.
type SAMLSSOParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DecodeSAMLConfig converts an IdentityProvider row into a runtime SAMLConfig.
// Returns nil if the row isn't a SAML type. Enabled + DisplayName are lifted
// from the parent row, and the attribute names fall back to sensible defaults
// so an admin who doesn't override them still gets email + group extraction.
func DecodeSAMLConfig(idp *IdentityProvider) *SAMLConfig {
	if idp == nil || idp.Type != "saml" {
		return nil
	}
	var cfg SAMLConfig
	if len(idp.Config) > 0 {
		raw, _ := json.Marshal(idp.Config)
		_ = json.Unmarshal(raw, &cfg)
	}
	cfg.Enabled = idp.Enabled
	cfg.DisplayName = idp.Name
	if cfg.EmailAttr == "" {
		cfg.EmailAttr = "email"
	}
	if cfg.GroupsAttr == "" {
		cfg.GroupsAttr = "groups"
	}
	return &cfg
}
