package iam

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/samlmeta"
)

// metadataTestCertB64 returns the base64 DER of a fresh self-signed cert —
// the form a signing KeyDescriptor carries in SAML metadata.
func metadataTestCertB64(t *testing.T) string {
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

// auth0StyleMetadata assembles an Auth0-shaped IdP metadata document with a
// signing cert, an email attribute declaration, and an HTTP-POST SSO endpoint.
func auth0StyleMetadata(certB64 string) string {
	const (
		nsMeta      = "urn:oasis:names:tc:SAML:2.0:metadata"
		nsDsig      = "http://www.w3.org/2000/09/xmldsig#"
		nsAssertion = "urn:oasis:names:tc:SAML:2.0:assertion"
		postBinding = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
		emailURI    = "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"
	)
	inner := fmt.Sprintf(`<KeyDescriptor use="signing"><KeyInfo xmlns="%s"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`, nsDsig, certB64) +
		fmt.Sprintf(`<Attribute xmlns="%s" Name="%s" FriendlyName="E-Mail Address" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:uri"/>`, nsAssertion, emailURI) +
		fmt.Sprintf(`<SingleSignOnService Binding="%s" Location="https://idp.auth0.test/samlp/abc"/>`, postBinding)
	return fmt.Sprintf(`<EntityDescriptor xmlns="%s" entityID="urn:idp.auth0.test"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">%s</IDPSSODescriptor></EntityDescriptor>`, nsMeta, inner)
}

// TestParseSAMLMetadata_Success asserts a valid Auth0-style document yields the
// SP fields the Add-IdP form pre-fills, with the email attribute auto-detected
// and a parseable PEM certificate.
func TestParseSAMLMetadata_Success(t *testing.T) {
	h := &Handler{audit: noopAudit()}
	cert := metadataTestCertB64(t)
	body, _ := json.Marshal(map[string]string{"metadataXml": auth0StyleMetadata(cert)})

	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/parse-saml-metadata", body, "admin", "admin_user")
	if err := h.ParseSAMLMetadata(c); err != nil {
		t.Fatalf("ParseSAMLMetadata: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got samlmeta.ParsedIdP
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.EntityID != "urn:idp.auth0.test" {
		t.Errorf("entityId = %q, want urn:idp.auth0.test", got.EntityID)
	}
	if got.SSOURL != "https://idp.auth0.test/samlp/abc" {
		t.Errorf("ssoUrl = %q", got.SSOURL)
	}
	if got.EmailAttribute != "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress" {
		t.Errorf("emailAttribute = %q, want auto-detected email URI", got.EmailAttribute)
	}
	if block, _ := pem.Decode([]byte(got.CertificatePEM)); block == nil {
		t.Errorf("certificatePem is not PEM: %q", got.CertificatePEM)
	}
}

// TestParseSAMLMetadata_NoIDPDescriptor asserts a document with no
// IDPSSODescriptor (e.g. SP metadata pasted by mistake) is rejected 400 with
// the field flagged so the form can surface it inline.
func TestParseSAMLMetadata_NoIDPDescriptor(t *testing.T) {
	h := &Handler{audit: noopAudit()}
	spDoc := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="urn:sp.test"></EntityDescriptor>`
	body, _ := json.Marshal(map[string]string{"metadataXml": spDoc})

	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/parse-saml-metadata", body, "admin", "admin_user")
	if err := h.ParseSAMLMetadata(c); err != nil {
		t.Fatalf("ParseSAMLMetadata: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	errObj := decodeErrObj(t, rec.Body.Bytes())
	if errObj["message"] != samlmeta.ErrNoIDPDescriptor.Error() {
		t.Errorf("message = %v, want %q", errObj["message"], samlmeta.ErrNoIDPDescriptor.Error())
	}
	if errObj["code"] != "metadataXml" {
		t.Errorf("code (field) = %v, want metadataXml", errObj["code"])
	}
}

// TestParseSAMLMetadata_EmptyDocument asserts a blank body is rejected with the
// samlmeta ErrEmpty reason rather than a generic message.
func TestParseSAMLMetadata_EmptyDocument(t *testing.T) {
	h := &Handler{audit: noopAudit()}
	body, _ := json.Marshal(map[string]string{"metadataXml": "   "})

	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/parse-saml-metadata", body, "admin", "admin_user")
	if err := h.ParseSAMLMetadata(c); err != nil {
		t.Fatalf("ParseSAMLMetadata: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErrObj(t, rec.Body.Bytes())["message"]; msg != samlmeta.ErrEmpty.Error() {
		t.Errorf("message = %v, want %q", msg, samlmeta.ErrEmpty.Error())
	}
}

// decodeErrObj unwraps the nested errJSON envelope {"error":{message,type,code}}.
func decodeErrObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %s", string(raw))
	}
	return errObj
}

// TestParseSAMLMetadata_MalformedBody asserts a non-JSON body is rejected 400
// before reaching the parser.
func TestParseSAMLMetadata_MalformedBody(t *testing.T) {
	h := &Handler{audit: noopAudit()}
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/parse-saml-metadata", []byte("{not json"), "admin", "admin_user")
	if err := h.ParseSAMLMetadata(c); err != nil {
		t.Fatalf("ParseSAMLMetadata: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
