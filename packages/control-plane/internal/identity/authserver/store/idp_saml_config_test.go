package store

import "testing"

func TestDecodeSAMLConfig(t *testing.T) {
	t.Run("nil idp returns nil", func(t *testing.T) {
		if DecodeSAMLConfig(nil) != nil {
			t.Fatal("expected nil for nil idp")
		}
	})

	t.Run("non-saml type returns nil", func(t *testing.T) {
		if DecodeSAMLConfig(&IdentityProvider{Type: "oidc"}) != nil {
			t.Fatal("expected nil for oidc type")
		}
	})

	t.Run("full config decoded, parent fields lifted", func(t *testing.T) {
		idp := &IdentityProvider{
			Type:    "saml",
			Name:    "Acme SSO",
			Enabled: true,
			Config: map[string]any{
				"entityId":        "https://idp.acme.test/metadata",
				"ssoUrl":          "https://idp.acme.test/sso",
				"certificatePem":  "-----BEGIN CERTIFICATE-----\nMII...\n-----END CERTIFICATE-----",
				"emailAttribute":  "mail",
				"groupsAttribute": "memberOf",
			},
		}
		cfg := DecodeSAMLConfig(idp)
		if cfg == nil {
			t.Fatal("expected non-nil config")
			return
		}
		if cfg.EntityID != "https://idp.acme.test/metadata" || cfg.SSOURL != "https://idp.acme.test/sso" {
			t.Errorf("entityID/ssoURL not decoded: %+v", cfg)
		}
		if cfg.EmailAttr != "mail" || cfg.GroupsAttr != "memberOf" {
			t.Errorf("attribute names not decoded: email=%q groups=%q", cfg.EmailAttr, cfg.GroupsAttr)
		}
		if !cfg.Enabled || cfg.DisplayName != "Acme SSO" {
			t.Errorf("parent fields not lifted: enabled=%v name=%q", cfg.Enabled, cfg.DisplayName)
		}
	})

	t.Run("attribute names default when absent", func(t *testing.T) {
		cfg := DecodeSAMLConfig(&IdentityProvider{
			Type:   "saml",
			Config: map[string]any{"entityId": "x", "ssoUrl": "y", "certificatePem": "z"},
		})
		if cfg.EmailAttr != "email" {
			t.Errorf("EmailAttr default = %q, want email", cfg.EmailAttr)
		}
		if cfg.GroupsAttr != "groups" {
			t.Errorf("GroupsAttr default = %q, want groups", cfg.GroupsAttr)
		}
	})

	t.Run("empty config still yields defaults", func(t *testing.T) {
		cfg := DecodeSAMLConfig(&IdentityProvider{Type: "saml"})
		if cfg == nil || cfg.EmailAttr != "email" || cfg.GroupsAttr != "groups" {
			t.Errorf("empty config defaults wrong: %+v", cfg)
		}
	})

	t.Run("ssoParams decoded", func(t *testing.T) {
		cfg := DecodeSAMLConfig(&IdentityProvider{
			Type: "saml",
			Config: map[string]any{
				"entityId": "x", "ssoUrl": "y", "certificatePem": "z",
				"ssoParams": []any{
					map[string]any{"key": "organization", "value": "org_abc123"},
				},
			},
		})
		if cfg == nil || len(cfg.SSOParams) != 1 {
			t.Fatalf("ssoParams not decoded: %+v", cfg)
		}
		if cfg.SSOParams[0].Key != "organization" || cfg.SSOParams[0].Value != "org_abc123" {
			t.Errorf("ssoParams wrong: %+v", cfg.SSOParams)
		}
	})
}
