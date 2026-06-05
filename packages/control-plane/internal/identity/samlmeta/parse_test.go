package samlmeta

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCertB64 generates a fresh self-signed certificate and returns its
// base64-encoded DER — the form a signing KeyDescriptor carries in metadata.
func testCertB64(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "idp.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

const (
	nsMeta      = `urn:oasis:names:tc:SAML:2.0:metadata`
	nsDsig      = `http://www.w3.org/2000/09/xmldsig#`
	nsAssertion = `urn:oasis:names:tc:SAML:2.0:assertion`
	postBinding = `urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST`
	rdrBinding  = `urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect`

	emailURI = `http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress`
	groupURI = `http://schemas.xmlsoap.org/claims/Group`
)

func signingKD(certB64 string) string {
	return fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, certB64)
}

func ssoEndpoint(binding, loc string) string {
	return fmt.Sprintf(`<SingleSignOnService Binding="%s" Location="%s"/>`, binding, loc)
}

func attrDecl(name, friendly string) string {
	return fmt.Sprintf(`<Attribute xmlns="%s" Name="%s" FriendlyName="%s" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:uri"/>`, nsAssertion, name, friendly)
}

// entityDoc assembles an EntityDescriptor IdP metadata document from parts.
func entityDoc(entityID, inner string) string {
	return fmt.Sprintf(`<EntityDescriptor xmlns="%s" entityID="%s"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">%s</IDPSSODescriptor></EntityDescriptor>`, nsMeta, entityID, inner)
}

func TestParse_Auth0Style(t *testing.T) {
	cert := testCertB64(t)
	inner := signingKD(cert) +
		attrDecl(emailURI, "E-Mail Address") +
		attrDecl(`http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name`, "Name") +
		ssoEndpoint(rdrBinding, "https://idp.auth0.test/samlp/abc") +
		ssoEndpoint(postBinding, "https://idp.auth0.test/samlp/abc")
	doc := entityDoc("urn:idp.auth0.test", inner)

	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EntityID != "urn:idp.auth0.test" {
		t.Errorf("EntityID = %q", got.EntityID)
	}
	// HTTP-POST binding wins over the HTTP-Redirect endpoint listed first.
	if got.SSOURL != "https://idp.auth0.test/samlp/abc" {
		t.Errorf("SSOURL = %q", got.SSOURL)
	}
	// Email attribute auto-detected from the declared long-URL Name.
	if got.EmailAttribute != emailURI {
		t.Errorf("EmailAttribute = %q, want %q", got.EmailAttribute, emailURI)
	}
	// Auth0 default declares no groups attribute.
	if got.GroupsAttribute != "" {
		t.Errorf("GroupsAttribute = %q, want empty", got.GroupsAttribute)
	}
	// Certificate re-encodes to canonical, parseable PEM.
	block, _ := pem.Decode([]byte(got.CertificatePEM))
	if block == nil {
		t.Fatalf("CertificatePEM is not PEM: %q", got.CertificatePEM)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("CertificatePEM does not parse: %v", err)
	}
}

func TestParse_OktaStyle_NoDeclaredAttributes(t *testing.T) {
	cert := testCertB64(t)
	inner := signingKD(cert) + ssoEndpoint(postBinding, "https://acme.okta.test/sso/saml")
	got, err := Parse([]byte(entityDoc("http://www.okta.test/exk1", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EmailAttribute != "" || got.GroupsAttribute != "" {
		t.Errorf("expected no attribute detection, got email=%q groups=%q", got.EmailAttribute, got.GroupsAttribute)
	}
	if got.SSOURL != "https://acme.okta.test/sso/saml" {
		t.Errorf("SSOURL = %q", got.SSOURL)
	}
}

func TestParse_AzureStyle_GroupsAttribute(t *testing.T) {
	cert := testCertB64(t)
	inner := signingKD(cert) +
		attrDecl(emailURI, "") +
		attrDecl(groupURI, "Group") +
		ssoEndpoint(postBinding, "https://login.microsoft.test/saml2")
	got, err := Parse([]byte(entityDoc("https://sts.windows.test/abc/", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EmailAttribute != emailURI {
		t.Errorf("EmailAttribute = %q", got.EmailAttribute)
	}
	if got.GroupsAttribute != groupURI {
		t.Errorf("GroupsAttribute = %q, want %q", got.GroupsAttribute, groupURI)
	}
}

func TestParse_RedirectOnlyBindingFallback(t *testing.T) {
	cert := testCertB64(t)
	inner := signingKD(cert) + ssoEndpoint(rdrBinding, "https://idp.test/redirect/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.SSOURL != "https://idp.test/redirect/sso" {
		t.Errorf("SSOURL = %q, want redirect fallback", got.SSOURL)
	}
}

func TestParse_EntitiesDescriptorWrapper(t *testing.T) {
	cert := testCertB64(t)
	inner := signingKD(cert) + ssoEndpoint(postBinding, "https://fed.test/sso")
	entity := entityDoc("urn:fed.test", inner)
	doc := fmt.Sprintf(`<EntitiesDescriptor xmlns="%s">%s</EntitiesDescriptor>`, nsMeta, entity)
	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EntityID != "urn:fed.test" {
		t.Errorf("EntityID = %q", got.EntityID)
	}
}

func TestParse_FriendlyNameMatch(t *testing.T) {
	// Name carries no "email" token but FriendlyName does — match on
	// FriendlyName, return the Name (the value carried on the assertion).
	cert := testCertB64(t)
	inner := signingKD(cert) +
		attrDecl("urn:custom:11", "Email Address") +
		ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EmailAttribute != "urn:custom:11" {
		t.Errorf("EmailAttribute = %q, want urn:custom:11", got.EmailAttribute)
	}
}

func TestParse_SigningCertPreferredOverFallback(t *testing.T) {
	// A use-less KeyDescriptor is a fallback; an explicit use="signing" wins.
	signing := testCertB64(t)
	fallback := testCertB64(t)
	uselessKD := fmt.Sprintf(`<KeyDescriptor><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, fallback)
	inner := uselessKD + signingKD(signing) + ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	block, _ := pem.Decode([]byte(got.CertificatePEM))
	der := base64.StdEncoding.EncodeToString(block.Bytes)
	if der != signing {
		t.Errorf("expected signing cert to win over use-less fallback")
	}
}

func TestParse_UselessKeyDescriptorFallback(t *testing.T) {
	// No explicit signing descriptor: the use-less one is accepted.
	cert := testCertB64(t)
	uselessKD := fmt.Sprintf(`<KeyDescriptor><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, cert)
	inner := uselessKD + ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.CertificatePEM == "" {
		t.Errorf("expected fallback cert to be used")
	}
}

func TestParse_SkipsKeyDescriptorWithoutCert(t *testing.T) {
	// A KeyDescriptor carrying no X509Certificate is skipped in favour of the
	// one that does.
	cert := testCertB64(t)
	emptyKD := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data></X509Data></KeyInfo></KeyDescriptor>`, nsDsig)
	inner := emptyKD + signingKD(cert) + ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.CertificatePEM == "" {
		t.Errorf("expected the populated KeyDescriptor to be used")
	}
}

func TestParse_WhitespaceOnlyCertData(t *testing.T) {
	// A signing KeyDescriptor whose certificate body is only whitespace yields
	// no usable cert.
	kd := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>   </X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig)
	inner := kd + ssoEndpoint(postBinding, "https://idp.test/sso")
	_, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if !errors.Is(err, ErrNoSigningCert) {
		t.Errorf("err = %v, want ErrNoSigningCert", err)
	}
}

func TestParse_AttributeNameEmptyFallsBackToFriendlyName(t *testing.T) {
	// When the matching attribute declares only a FriendlyName, that value is
	// returned (no Name to prefer).
	cert := testCertB64(t)
	attr := fmt.Sprintf(`<Attribute xmlns="%s" FriendlyName="email"/>`, nsAssertion)
	inner := signingKD(cert) + attr + ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.EmailAttribute != "email" {
		t.Errorf("EmailAttribute = %q, want friendly-name fallback 'email'", got.EmailAttribute)
	}
}

func TestParse_Errors(t *testing.T) {
	cert := testCertB64(t)
	validInner := signingKD(cert) + ssoEndpoint(postBinding, "https://idp.test/sso")

	tests := []struct {
		name    string
		doc     string
		wantErr error
	}{
		{
			name:    "empty",
			doc:     "   \n  ",
			wantErr: ErrEmpty,
		},
		{
			name:    "no idp descriptor",
			doc:     fmt.Sprintf(`<EntityDescriptor xmlns="%s" entityID="urn:sp.test"></EntityDescriptor>`, nsMeta),
			wantErr: ErrNoIDPDescriptor,
		},
		{
			name:    "no entityID",
			doc:     entityDoc("", validInner),
			wantErr: ErrNoEntityID,
		},
		{
			name:    "no sso url",
			doc:     entityDoc("urn:idp.test", signingKD(cert)),
			wantErr: ErrNoSSOURL,
		},
		{
			name:    "no signing cert",
			doc:     entityDoc("urn:idp.test", ssoEndpoint(postBinding, "https://idp.test/sso")),
			wantErr: ErrNoSigningCert,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Parse error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParse_MalformedXML(t *testing.T) {
	_, err := Parse([]byte(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"`))
	if err == nil {
		t.Fatal("expected error for malformed XML")
	}
	if errors.Is(err, ErrEmpty) {
		t.Errorf("malformed XML should not be ErrEmpty")
	}
}

func TestParse_BadBase64Cert(t *testing.T) {
	badKD := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>@@not-base64@@</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig)
	inner := badKD + ssoEndpoint(postBinding, "https://idp.test/sso")
	_, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err == nil || !strings.Contains(err.Error(), "decode signing certificate") {
		t.Errorf("err = %v, want decode signing certificate", err)
	}
}

func TestParse_ValidBase64ButNotACert(t *testing.T) {
	notCert := base64.StdEncoding.EncodeToString([]byte("hello world, not a certificate"))
	kd := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, notCert)
	inner := kd + ssoEndpoint(postBinding, "https://idp.test/sso")
	_, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err == nil || !strings.Contains(err.Error(), "parse signing certificate") {
		t.Errorf("err = %v, want parse signing certificate", err)
	}
}

func TestParse_CertWithWhitespaceData(t *testing.T) {
	// Metadata frequently pretty-prints the base64 with embedded newlines —
	// the parser must strip whitespace before decoding.
	cert := testCertB64(t)
	wrapped := cert[:20] + "\n        " + cert[20:]
	kd := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, wrapped)
	inner := kd + ssoEndpoint(postBinding, "https://idp.test/sso")
	got, err := Parse([]byte(entityDoc("urn:idp.test", inner)))
	if err != nil {
		t.Fatalf("Parse with whitespaced cert: %v", err)
	}
	if got.CertificatePEM == "" {
		t.Errorf("expected cert despite embedded whitespace")
	}
}
