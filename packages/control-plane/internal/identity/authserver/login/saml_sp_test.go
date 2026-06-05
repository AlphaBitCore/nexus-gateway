package login

import (
	"encoding/base64"
	"testing"

	"github.com/crewjam/saml"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestBuildSAMLServiceProvider(t *testing.T) {
	kp := newTestIDPKeypair(t)
	const issuer = "https://cp.nexus.test"

	t.Run("nil config", func(t *testing.T) {
		if _, err := buildSAMLServiceProvider(nil, issuer); err == nil {
			t.Fatal("expected error for nil config")
		}
	})

	t.Run("incomplete config rejected", func(t *testing.T) {
		for _, cfg := range []*store.SAMLConfig{
			{SSOURL: "x", Certificate: kp.CertPEM},   // no entityID
			{EntityID: "x", Certificate: kp.CertPEM}, // no ssoURL
			{EntityID: "x", SSOURL: "y"},             // no cert
		} {
			if _, err := buildSAMLServiceProvider(cfg, issuer); err == nil {
				t.Errorf("expected error for incomplete config %+v", cfg)
			}
		}
	})

	t.Run("bad certificate rejected", func(t *testing.T) {
		cfg := &store.SAMLConfig{EntityID: "idp", SSOURL: "https://idp/sso", Certificate: "not-a-cert"}
		if _, err := buildSAMLServiceProvider(cfg, issuer); err == nil {
			t.Fatal("expected error for unparseable certificate")
		}
	})

	t.Run("valid config builds SP with issuer-derived URLs + IdP metadata", func(t *testing.T) {
		cfg := &store.SAMLConfig{
			EntityID:    "https://idp.acme.test/metadata",
			SSOURL:      "https://idp.acme.test/sso",
			Certificate: kp.CertPEM,
		}
		sp, err := buildSAMLServiceProvider(cfg, issuer+"/") // trailing slash trimmed
		if err != nil {
			t.Fatalf("buildSAMLServiceProvider: %v", err)
		}
		if got := sp.AcsURL.String(); got != issuer+samlACSPath {
			t.Errorf("AcsURL = %q, want %q", got, issuer+samlACSPath)
		}
		if got := sp.MetadataURL.String(); got != issuer+samlMetadataPath {
			t.Errorf("MetadataURL = %q, want %q", got, issuer+samlMetadataPath)
		}
		if sp.EntityID != issuer+samlMetadataPath {
			t.Errorf("EntityID = %q, want %q", sp.EntityID, issuer+samlMetadataPath)
		}
		if sp.AllowIDPInitiated {
			t.Error("AllowIDPInitiated must be false (reject IdP-initiated)")
		}
		// IdP metadata must carry the SSO endpoint + signing cert so crewjam
		// can post the AuthnRequest and verify the response.
		if len(sp.IDPMetadata.IDPSSODescriptors) != 1 {
			t.Fatalf("want 1 IDPSSODescriptor, got %d", len(sp.IDPMetadata.IDPSSODescriptors))
		}
		d := sp.IDPMetadata.IDPSSODescriptors[0]
		if len(d.SingleSignOnServices) != 1 || d.SingleSignOnServices[0].Location != cfg.SSOURL {
			t.Errorf("SSO endpoint not set: %+v", d.SingleSignOnServices)
		}
		if d.SingleSignOnServices[0].Binding != saml.HTTPPostBinding {
			t.Errorf("SSO binding = %q, want POST", d.SingleSignOnServices[0].Binding)
		}
		kd := d.KeyDescriptors
		if len(kd) != 1 || kd[0].KeyInfo.X509Data.X509Certificates[0].Data == "" {
			t.Errorf("signing cert not embedded in IdP metadata")
		}
	})

	// Regression: SAML login matches a returning user by the NameID subject, so
	// the subject must be stable across logins. crewjam defaults an unset format
	// to "transient" (a per-session pseudonym that changes every login), which
	// breaks that matching on any compliant IdP — and which some IdPs reject
	// outright with a top-level status:Requester. The SP must request Unspecified
	// (let the IdP issue its native stable NameID); the AuthnRequest must never
	// carry transient.
	t.Run("AuthnRequest does not demand transient NameID (Auth0 compat)", func(t *testing.T) {
		cfg := &store.SAMLConfig{
			EntityID:    "https://idp.acme.test/metadata",
			SSOURL:      "https://idp.acme.test/sso",
			Certificate: kp.CertPEM,
		}
		sp, err := buildSAMLServiceProvider(cfg, issuer)
		if err != nil {
			t.Fatalf("buildSAMLServiceProvider: %v", err)
		}
		if sp.AuthnNameIDFormat != saml.UnspecifiedNameIDFormat {
			t.Fatalf("AuthnNameIDFormat = %q, want Unspecified", sp.AuthnNameIDFormat)
		}
		req, err := sp.MakeAuthenticationRequest(cfg.SSOURL, saml.HTTPPostBinding, saml.HTTPPostBinding)
		if err != nil {
			t.Fatalf("MakeAuthenticationRequest: %v", err)
		}
		if req.NameIDPolicy != nil && req.NameIDPolicy.Format != nil {
			if got := *req.NameIDPolicy.Format; got == string(saml.TransientNameIDFormat) {
				t.Errorf("AuthnRequest NameIDPolicy.Format = %q; must not be transient", got)
			}
		}
	})
}

func TestParseCertificatePEM(t *testing.T) {
	kp := newTestIDPKeypair(t)

	t.Run("PEM form", func(t *testing.T) {
		if _, err := parseCertificatePEM(kp.CertPEM); err != nil {
			t.Fatalf("PEM parse: %v", err)
		}
	})

	t.Run("bare base64 DER form", func(t *testing.T) {
		der := base64.StdEncoding.EncodeToString(kp.Cert.Raw)
		if _, err := parseCertificatePEM(der); err != nil {
			t.Fatalf("base64 DER parse: %v", err)
		}
	})

	t.Run("garbage rejected", func(t *testing.T) {
		if _, err := parseCertificatePEM("!!! not base64 !!!"); err == nil {
			t.Fatal("expected error for non-PEM non-base64 input")
		}
	})

	t.Run("base64 of non-cert rejected", func(t *testing.T) {
		if _, err := parseCertificatePEM(base64.StdEncoding.EncodeToString([]byte("hello"))); err == nil {
			t.Fatal("expected error for base64 that is not a certificate")
		}
	})
}
