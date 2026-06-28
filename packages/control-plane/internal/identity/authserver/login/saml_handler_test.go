package login

import (
	"context"
	"encoding/base64"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// recordingAuditProducer captures Enqueue'd audit payloads so the login
// handlers' audit emission can be asserted (audit.Writer.Log is synchronous).
type recordingAuditProducer struct{ enqueued [][]byte }

func (p *recordingAuditProducer) Publish(context.Context, string, []byte) error { return nil }
func (p *recordingAuditProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	p.enqueued = append(p.enqueued, append([]byte(nil), data...))
	return nil
}
func (p *recordingAuditProducer) Close() error { return nil }

const samlIssuer = "https://cp.nexus.test"
const idpRowQuery = `SELECT id, type, name, enabled, config`

// samlConfigJSON returns the JSON config blob stored on a SAML IdentityProvider row.
func samlConfigJSON(entityID, ssoURL, certPEM string) []byte {
	b, _ := json.Marshal(map[string]any{
		"entityId":       entityID,
		"ssoUrl":         ssoURL,
		"certificatePem": certPEM,
	})
	return b
}

// samlIdPRows builds a single-row pgxmock result for a SAML IdentityProvider.
func samlIdPRows(id, name string, enabled, jit bool, cfg []byte) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
	}).AddRow(id, "saml", name, enabled, cfg, "developer", false, jit)
}

func mustURL(t *testing.T, s string) url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url %q: %v", s, err)
	}
	return *u
}

// mintSignedSAMLResponse produces a base64-encoded, signed SAMLResponse the way
// a real IdP would, targeting sp, with the given InResponseTo, NameID, and
// custom attributes. Uses crewjam's IdP side so the response passes the real
// ParseResponse (signature + conditions + audience + destination + InResponseTo).
func mintSignedSAMLResponse(t *testing.T, kp *testIDPKeypair, sp *saml.ServiceProvider, idpEntityID, requestID, nameID string, custom []saml.Attribute) string {
	t.Helper()
	idp := &saml.IdentityProvider{
		Key:         kp.Key,
		Certificate: kp.Cert,
		MetadataURL: mustURL(t, idpEntityID),
		SSOURL:      mustURL(t, "https://idp.acme.test/sso"),
	}
	spMeta := sp.Metadata()
	authnReq := &saml.IdpAuthnRequest{
		IDP:                     idp,
		HTTPRequest:             httptest.NewRequest(http.MethodPost, "/sso", nil),
		ServiceProviderMetadata: spMeta,
		SPSSODescriptor:         &spMeta.SPSSODescriptors[0],
		ACSEndpoint:             &saml.IndexedEndpoint{Binding: saml.HTTPPostBinding, Location: sp.AcsURL.String()},
		Request: saml.AuthnRequest{
			ID:                          requestID,
			IssueInstant:                time.Now(),
			AssertionConsumerServiceURL: sp.AcsURL.String(),
		},
		Now: time.Now(),
	}
	session := &saml.Session{
		ID:               "sess-1",
		CreateTime:       time.Now(),
		ExpireTime:       time.Now().Add(time.Hour),
		Index:            "idx-1",
		NameID:           nameID,
		CustomAttributes: custom,
	}
	if err := (saml.DefaultAssertionMaker{}).MakeAssertion(authnReq, session); err != nil {
		t.Fatalf("MakeAssertion: %v", err)
	}
	if err := authnReq.MakeResponse(); err != nil {
		t.Fatalf("MakeResponse: %v", err)
	}
	doc := etree.NewDocument()
	doc.SetRoot(authnReq.ResponseEl)
	buf, err := doc.WriteToBytes()
	if err != nil {
		t.Fatalf("serialize response: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func attr(name string, values ...string) saml.Attribute {
	a := saml.Attribute{Name: name}
	for _, v := range values {
		a.Values = append(a.Values, saml.AttributeValue{Value: v})
	}
	return a
}

func newSAMLACSCtx(relayState, samlResponse string) (echo.Context, *httptest.ResponseRecorder) {
	form := url.Values{}
	if relayState != "" {
		form.Set("RelayState", relayState)
	}
	if samlResponse != "" {
		form.Set("SAMLResponse", samlResponse)
	}
	req := httptest.NewRequest(http.MethodPost, "/authserver/saml/acs", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// --- ACS handler ---

func TestSAMLACSHandler_FailurePaths(t *testing.T) {
	d := SAMLDeps{
		Pending:  store.NewPendingAuthzStore(),
		Requests: store.NewSAMLRequestStore(),
		Issuer:   samlIssuer,
	}
	t.Cleanup(d.Pending.Close)
	t.Cleanup(d.Requests.Close)

	t.Run("missing RelayState", func(t *testing.T) {
		c, rec := newSAMLACSCtx("", "x")
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400", rec.Code)
		}
	})

	t.Run("no pending entry (replay / unsolicited)", func(t *testing.T) {
		c, rec := newSAMLACSCtx("ghost", "x")
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400", rec.Code)
		}
	})

	t.Run("pending present but no outstanding request id (IdP-initiated rejected)", func(t *testing.T) {
		d.Pending.Put("ctx-noreq", store.PendingAuthzEntry{IdPID: "idp-1", ExpiresAt: time.Now().Add(time.Minute)})
		c, rec := newSAMLACSCtx("ctx-noreq", "x")
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 (no outstanding AuthnRequest = reject)", rec.Code)
		}
	})
}

func TestSAMLACSHandler_HappyAndProvision(t *testing.T) {
	kp := newTestIDPKeypair(t)
	const idpEntityID = "https://idp.acme.test/metadata"
	cfg := samlConfigJSON(idpEntityID, "https://idp.acme.test/sso", kp.CertPEM)
	sp, err := buildSAMLServiceProvider(store.DecodeSAMLConfig(&store.IdentityProvider{Type: "saml", Config: map[string]any{
		"entityId": idpEntityID, "ssoUrl": "https://idp.acme.test/sso", "certificatePem": kp.CertPEM,
	}}), samlIssuer)
	if err != nil {
		t.Fatalf("build sp: %v", err)
	}

	t.Run("happy: known federated identity -> 302 with auth code", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		// GetByID for the IdP
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").
			WillReturnRows(samlIdPRows("idp-1", "Acme", true, true, cfg))
		// FindByIdPSubject -> found (so the match branch resolves the user;
		// the subsequent fire-and-forget UpdateRawClaims error is ignored by
		// the handler and is not asserted here). The WithArgs pins the subject
		// the assertion must carry, so a 302 proves NameID extraction worked.
		mock.MatchExpectationsInOrder(false)
		mock.ExpectQuery(`FROM "UserFederatedIdentity"`).WithArgs("idp-1", "alice@acme.test").
			WillReturnRows(pgxmock.NewRows([]string{
				"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
			}).AddRow("fi-1", "user-1", "idp-1", "alice@acme.test", nil, []byte(`{}`), time.Now(), nil))

		pending := store.NewPendingAuthzStore()
		t.Cleanup(pending.Close)
		reqs := store.NewSAMLRequestStore()
		t.Cleanup(reqs.Close)
		authctx := "ctx-happy"
		pending.Put(authctx, store.PendingAuthzEntry{
			ClientID: "cli", RedirectURI: "http://127.0.0.1/cb", State: "st", CodeChallenge: "cc",
			IdPID: "idp-1", ExpiresAt: time.Now().Add(5 * time.Minute),
		})
		const reqID = "id-test-req-1"
		reqs.Put(authctx, reqID)

		prod := &recordingAuditProducer{}
		d := SAMLDeps{
			IdPs: store.NewIdPStoreWithPool(mock), Federated: store.NewFederatedStoreWithPool(mock),
			Pending: pending, AuthCodes: store.NewAuthCodeStore(5 * time.Minute), Requests: reqs, Issuer: samlIssuer,
			Audit: audit.NewWriter(prod, "admin-audit", nil),
		}
		t.Cleanup(d.AuthCodes.Close)

		resp := mintSignedSAMLResponse(t, kp, sp, idpEntityID, reqID, "alice@acme.test",
			[]saml.Attribute{attr("email", "alice@acme.test"), attr("groups", "admins", "eng")})
		c, rec := newSAMLACSCtx(authctx, resp)
		if err := SAMLACSHandler(d)(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "code=") || !strings.HasPrefix(loc, "http://127.0.0.1/cb") {
			t.Errorf("redirect missing code/redirect_uri: %q", loc)
		}
		// A successful SAML login must emit the admin.login.succeeded audit row.
		if len(prod.enqueued) != 1 {
			t.Fatalf("want 1 audit row, got %d", len(prod.enqueued))
		}
		var ev map[string]any
		if err := json.Unmarshal(prod.enqueued[0], &ev); err != nil {
			t.Fatalf("audit unmarshal: %v", err)
		}
		if ev["action"] != "admin.login.succeeded" {
			t.Errorf("audit action = %v, want admin.login.succeeded", ev["action"])
		}
		if ev["actorLabel"] != "alice@acme.test" {
			t.Errorf("audit actorLabel = %v, want alice@acme.test", ev["actorLabel"])
		}
	})

	t.Run("invalid response (garbage) -> 401", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").
			WillReturnRows(samlIdPRows("idp-1", "Acme", true, true, cfg))
		pending := store.NewPendingAuthzStore()
		t.Cleanup(pending.Close)
		reqs := store.NewSAMLRequestStore()
		t.Cleanup(reqs.Close)
		pending.Put("ctx-bad", store.PendingAuthzEntry{IdPID: "idp-1", ExpiresAt: time.Now().Add(time.Minute)})
		reqs.Put("ctx-bad", "id-x")
		d := SAMLDeps{
			IdPs: store.NewIdPStoreWithPool(mock), Federated: store.NewFederatedStoreWithPool(mock),
			Pending: pending, AuthCodes: store.NewAuthCodeStore(time.Minute), Requests: reqs, Issuer: samlIssuer,
		}
		t.Cleanup(d.AuthCodes.Close)
		c, rec := newSAMLACSCtx("ctx-bad", base64.StdEncoding.EncodeToString([]byte("<nonsense/>")))
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d, want 401 for invalid response", rec.Code)
		}
	})

	t.Run("unknown subject + JIT disabled -> user_not_provisioned", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfgNoJIT := cfg
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").
			WillReturnRows(samlIdPRows("idp-1", "Acme", true, false, cfgNoJIT)) // jit=false
		mock.ExpectQuery(`FROM "UserFederatedIdentity"`).WithArgs("idp-1", "bob@acme.test").
			WillReturnError(pgx.ErrNoRows)

		pending := store.NewPendingAuthzStore()
		t.Cleanup(pending.Close)
		reqs := store.NewSAMLRequestStore()
		t.Cleanup(reqs.Close)
		authctx := "ctx-nojit"
		pending.Put(authctx, store.PendingAuthzEntry{IdPID: "idp-1", ExpiresAt: time.Now().Add(time.Minute)})
		const reqID = "id-test-req-2"
		reqs.Put(authctx, reqID)
		d := SAMLDeps{
			IdPs: store.NewIdPStoreWithPool(mock), Federated: store.NewFederatedStoreWithPool(mock),
			Pending: pending, AuthCodes: store.NewAuthCodeStore(time.Minute), Requests: reqs, Issuer: samlIssuer,
		}
		t.Cleanup(d.AuthCodes.Close)
		resp := mintSignedSAMLResponse(t, kp, sp, idpEntityID, reqID, "bob@acme.test", nil)
		c, rec := newSAMLACSCtx(authctx, resp)
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d, want 401 user_not_provisioned", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "user_not_provisioned") {
			t.Errorf("body = %q, want user_not_provisioned", rec.Body.String())
		}
	})
}

// --- Metadata + extraction helpers ---

func TestSAMLMetadataHandler(t *testing.T) {
	d := SAMLDeps{Issuer: samlIssuer}
	req := httptest.NewRequest(http.MethodGet, "/authserver/saml/metadata", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if err := SAMLMetadataHandler(d)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "EntityDescriptor") || !strings.Contains(body, samlIssuer+samlACSPath) {
		t.Errorf("metadata missing EntityDescriptor/ACS: %q", body)
	}
}

func TestSAMLAttributeExtraction(t *testing.T) {
	a := &saml.Assertion{
		Subject: &saml.Subject{NameID: &saml.NameID{Value: "  user@x  "}},
		AttributeStatements: []saml.AttributeStatement{{Attributes: []saml.Attribute{
			{Name: "email", Values: []saml.AttributeValue{{Value: "user@x"}}},
			{FriendlyName: "groups", Values: []saml.AttributeValue{{Value: "a"}, {Value: " "}, {Value: "b"}}},
		}}},
	}
	if got := samlNameID(a); got != "user@x" {
		t.Errorf("samlNameID = %q, want trimmed user@x", got)
	}
	if got := samlFirstAttr(a, "email"); got != "user@x" {
		t.Errorf("samlFirstAttr(email) = %q", got)
	}
	if got := samlAttrValues(a, "groups"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("samlAttrValues(groups) = %v, want [a b] (blank dropped)", got)
	}
	if samlNameID(nil) != "" || samlNameID(&saml.Assertion{}) != "" {
		t.Error("samlNameID must be empty for nil/no-subject")
	}
	if samlFirstAttr(a, "absent") != "" {
		t.Error("samlFirstAttr(absent) must be empty")
	}
}
