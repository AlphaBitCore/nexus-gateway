// Package samlmeta extracts the SP-relevant fields from a SAML IdP metadata
// XML document so the admin Add-IdP form can be pre-filled by uploading the
// IdP's metadata instead of hand-copying entityID, SSO URL, and the signing
// certificate. This keeps the error-prone steps (certificate paste especially)
// off the admin, and — because many IdPs (Auth0, Azure AD) declare their
// emitted attributes in metadata — lets Nexus auto-detect the email / groups
// attribute names too, so the customer never has to rename claims on the IdP.
//
// Parsing is pure (no network): the caller supplies the XML bytes. The
// companion admin endpoint accepts an uploaded/pasted document only — there is
// deliberately no server-side metadata-URL fetch, to keep the surface free of
// SSRF exposure.
package samlmeta

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
)

// ParsedIdP is the SAML IdP configuration extracted from a metadata document,
// shaped to match the JSON keys the admin Add-IdP form's SAML config produces
// (entityId / ssoUrl / certificatePem) plus the optional attribute-name
// overrides. EmailAttribute / GroupsAttribute are empty when the metadata
// declares no attribute whose name/friendly-name looks like email / groups.
type ParsedIdP struct {
	EntityID        string `json:"entityId"`
	SSOURL          string `json:"ssoUrl"`
	CertificatePEM  string `json:"certificatePem"`
	EmailAttribute  string `json:"emailAttribute,omitempty"`
	GroupsAttribute string `json:"groupsAttribute,omitempty"`
}

var (
	// ErrEmpty is returned when the supplied document is blank.
	ErrEmpty = errors.New("saml metadata is empty")
	// ErrNoIDPDescriptor is returned when the document parses but carries no
	// IDPSSODescriptor (e.g. it is SP metadata, or an unrelated entity).
	ErrNoIDPDescriptor = errors.New("saml metadata has no IDPSSODescriptor")
	// ErrNoEntityID is returned when the IdP entity has no entityID.
	ErrNoEntityID = errors.New("saml metadata has no entityID")
	// ErrNoSSOURL is returned when the IdP declares no usable
	// SingleSignOnService endpoint.
	ErrNoSSOURL = errors.New("saml metadata has no SingleSignOnService location")
	// ErrNoSigningCert is returned when the IdP declares no signing
	// certificate — without it the assertion signature can't be verified.
	ErrNoSigningCert = errors.New("saml metadata has no signing certificate")
)

// Parse extracts the SP-relevant fields from an IdP SAML metadata document.
// It accepts both a bare <EntityDescriptor> and an <EntitiesDescriptor>
// federation wrapper (the first entity carrying an IDPSSODescriptor wins).
func Parse(xmlData []byte) (*ParsedIdP, error) {
	if len(strings.TrimSpace(string(xmlData))) == 0 {
		return nil, ErrEmpty
	}
	// samlsp.ParseMetadata runs the document through an XML round-trip
	// validator first (guards against XML-injection / entity-expansion) and
	// unwraps an EntitiesDescriptor to the first IdP entity.
	ed, err := samlsp.ParseMetadata(xmlData)
	if err != nil {
		return nil, fmt.Errorf("parse saml metadata: %w", err)
	}
	if len(ed.IDPSSODescriptors) == 0 {
		return nil, ErrNoIDPDescriptor
	}
	idp := ed.IDPSSODescriptors[0]

	entityID := strings.TrimSpace(ed.EntityID)
	if entityID == "" {
		return nil, ErrNoEntityID
	}

	ssoURL := pickSSOURL(idp.SingleSignOnServices)
	if ssoURL == "" {
		return nil, ErrNoSSOURL
	}

	certPEM, err := signingCertPEM(idp.KeyDescriptors)
	if err != nil {
		return nil, err
	}

	out := &ParsedIdP{
		EntityID:        entityID,
		SSOURL:          ssoURL,
		CertificatePEM:  certPEM,
		EmailAttribute:  pickAttribute(idp.Attributes, "email"),
		GroupsAttribute: pickAttribute(idp.Attributes, "group"),
	}
	return out, nil
}

// pickSSOURL returns the IdP's SingleSignOnService location, preferring the
// HTTP-POST binding (the binding Nexus posts its AuthnRequest to and the one
// it advertises in SP metadata). It falls back to the first endpoint listed so
// an IdP that only declares HTTP-Redirect still resolves. crewjam rejects an
// endpoint with an empty Location while unmarshalling, so locations here are
// already non-empty.
func pickSSOURL(eps []saml.Endpoint) string {
	var fallback string
	for _, e := range eps {
		if e.Binding == saml.HTTPPostBinding {
			return strings.TrimSpace(e.Location)
		}
		if fallback == "" {
			fallback = strings.TrimSpace(e.Location)
		}
	}
	return fallback
}

// signingCertPEM returns the IdP's signing certificate re-encoded as canonical
// PEM. A KeyDescriptor with use="signing" wins; a use-less descriptor (some
// IdPs omit the attribute, meaning the key serves both roles) is accepted as a
// fallback. The base64 DER carried in metadata is decoded and re-encoded so the
// stored value is always well-formed PEM, and the certificate is parsed to
// reject a malformed blob up front.
func signingCertPEM(kds []saml.KeyDescriptor) (string, error) {
	var fallbackDER string
	signingDER := ""
	for _, kd := range kds {
		certs := kd.KeyInfo.X509Data.X509Certificates
		if len(certs) == 0 {
			continue
		}
		data := strings.Join(strings.Fields(certs[0].Data), "")
		if data == "" {
			continue
		}
		switch strings.ToLower(kd.Use) {
		case "signing":
			signingDER = data
		case "":
			if fallbackDER == "" {
				fallbackDER = data
			}
		}
	}
	chosen := signingDER
	if chosen == "" {
		chosen = fallbackDER
	}
	if chosen == "" {
		return "", ErrNoSigningCert
	}
	der, err := base64.StdEncoding.DecodeString(chosen)
	if err != nil {
		return "", fmt.Errorf("decode signing certificate: %w", err)
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		return "", fmt.Errorf("parse signing certificate: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return strings.TrimRight(string(pemBytes), "\n"), nil
}

// pickAttribute returns the Name of the first declared attribute whose Name or
// FriendlyName contains the given keyword (case-insensitive) — e.g. keyword
// "email" matches Auth0's FriendlyName "E-Mail Address" / Name
// ".../emailaddress". The Name is returned (not FriendlyName) because that is
// the value carried on the assertion attribute and matched at login. Returns ""
// when no attribute is declared or none matches; the form then falls back to
// the Nexus default attribute name.
func pickAttribute(attrs []saml.Attribute, keyword string) string {
	for _, a := range attrs {
		if strings.Contains(strings.ToLower(a.Name), keyword) ||
			strings.Contains(strings.ToLower(a.FriendlyName), keyword) {
			if name := strings.TrimSpace(a.Name); name != "" {
				return name
			}
			return strings.TrimSpace(a.FriendlyName)
		}
	}
	return ""
}
