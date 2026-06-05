package login

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/crewjam/saml"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// SP endpoint paths, mounted under the auth-server issuer. The ACS path is
// also the SAML response Destination the IdP must target; the metadata path
// doubles as the SP entityID.
const (
	samlACSPath      = "/authserver/saml/acs"
	samlMetadataPath = "/authserver/saml/metadata"
)

// errSAMLIncompleteConfig is returned when a SAML IdP row is missing one of
// the fields required to build a ServiceProvider.
var errSAMLIncompleteConfig = errors.New("saml: incomplete idp config (entityId / ssoUrl / certificatePem required)")

// buildSAMLServiceProvider constructs a crewjam ServiceProvider for one IdP.
// The SP entityID and ACS URL derive from the auth-server issuer; the IdP
// metadata (entityID, HTTP-POST SSO URL, signing certificate) comes from the
// decoded IdP config. The SP holds no key: the AuthnRequest is unsigned (the
// IdP signs the response) and assertions are not encrypted, so neither signing
// nor decryption keys are needed. AllowIDPInitiated is left false so a
// response without an InResponseTo we issued is rejected.
//
// AuthnNameIDFormat is Unspecified so crewjam omits the NameIDPolicy Format
// constraint and the IdP returns its native NameID. This is a deliberate,
// IdP-agnostic choice driven by our identity model: SAML login matches a
// returning user by the NameID subject (resolveOrProvision →
// FindByIdPSubject), so that subject must be a STABLE per-user identifier.
// crewjam otherwise defaults an unset format to "transient" — a per-session
// pseudonym that changes every login — which silently breaks subject matching
// on any spec-compliant IdP (and which some IdPs reject outright with a
// top-level status:Requester; Auth0, whose default NameID is unspecified, is
// one such IdP). A specific format (persistent / emailAddress) is avoided
// because it re-introduces IdP-rejection / onboarding friction; letting the IdP
// issue its native stable NameID is the most interoperable, zero-IdP-config
// option.
func buildSAMLServiceProvider(cfg *store.SAMLConfig, issuer string) (*saml.ServiceProvider, error) {
	if cfg == nil {
		return nil, errSAMLIncompleteConfig
	}
	if cfg.EntityID == "" || cfg.SSOURL == "" || cfg.Certificate == "" {
		return nil, errSAMLIncompleteConfig
	}
	idpCert, err := parseCertificatePEM(cfg.Certificate)
	if err != nil {
		return nil, fmt.Errorf("saml: parse idp certificate: %w", err)
	}
	base := strings.TrimRight(issuer, "/")
	acsURL, err := url.Parse(base + samlACSPath)
	if err != nil {
		return nil, fmt.Errorf("saml: parse acs url: %w", err)
	}
	metaURL, err := url.Parse(base + samlMetadataPath)
	if err != nil {
		return nil, fmt.Errorf("saml: parse metadata url: %w", err)
	}
	return &saml.ServiceProvider{
		EntityID:          metaURL.String(),
		AcsURL:            *acsURL,
		MetadataURL:       *metaURL,
		IDPMetadata:       idpEntityDescriptor(cfg.EntityID, cfg.SSOURL, idpCert),
		AuthnNameIDFormat: saml.UnspecifiedNameIDFormat,
	}, nil
}

// idpEntityDescriptor builds the minimal IdP metadata crewjam needs to verify
// a signed response: the IdP entityID, its HTTP-POST SSO endpoint, and its
// signing certificate (getIDPSigningCerts reads the KeyDescriptors of each
// IDPSSODescriptor).
func idpEntityDescriptor(entityID, ssoURL string, cert *x509.Certificate) *saml.EntityDescriptor {
	certB64 := base64.StdEncoding.EncodeToString(cert.Raw)
	return &saml.EntityDescriptor{
		EntityID: entityID,
		IDPSSODescriptors: []saml.IDPSSODescriptor{{
			SSODescriptor: saml.SSODescriptor{
				RoleDescriptor: saml.RoleDescriptor{
					KeyDescriptors: []saml.KeyDescriptor{{
						Use: "signing",
						KeyInfo: saml.KeyInfo{
							X509Data: saml.X509Data{
								X509Certificates: []saml.X509Certificate{{Data: certB64}},
							},
						},
					}},
				},
			},
			SingleSignOnServices: []saml.Endpoint{{
				Binding:  saml.HTTPPostBinding,
				Location: ssoURL,
			}},
		}},
	}
}

// parseCertificatePEM parses an IdP signing certificate supplied either as PEM
// or as bare base64 DER (admins paste either form). Returns an error if it is
// neither.
func parseCertificatePEM(s string) (*x509.Certificate, error) {
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return x509.ParseCertificate(block.Bytes)
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("certificate is neither PEM nor base64 DER")
	}
	return x509.ParseCertificate(der)
}
