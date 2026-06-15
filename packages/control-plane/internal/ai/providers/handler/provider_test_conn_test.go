// Coverage for provider_test_conn.go: RegisterProviderTestRoutes,
// ProviderTestConnection, ProviderTest, decryptCredentialByID,
// getFirstCredentialKey, decryptCredential, forwardProviderTest,
// ListProviderHealth.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// RegisterProviderTestRoutes — wires 3 routes

func TestRegisterProviderTestRoutes_WiresThreeRoutes(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterProviderTestRoutes(g, iamMW)
	want := map[string]bool{
		"POST /api/admin/providers/test-connection": false,
		"POST /api/admin/providers/:id/test":        false,
		"GET /api/admin/provider-health":            false,
	}
	for _, r := range e.Routes() {
		if _, ok := want[r.Method+" "+r.Path]; ok {
			want[r.Method+" "+r.Path] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("route %s not registered", k)
		}
	}
}

// TestRegisterProviderTestRoutes_TestConnectionGatedOnCreate proves F-0369: the
// draft test-connection route is gated on the provider-config-write tier
// (provider:create), NOT provider:read. A read-only viewer can no longer use the
// probe as a blind-SSRF / internal-endpoint fingerprinting oracle. The
// stored-provider test stays on provider:read + credential:probe.
func TestRegisterProviderTestRoutes_TestConnectionGatedOnCreate(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")

	// Record the action string each iamMW is constructed with, in registration
	// order, so we can map them back to routes.
	var actions []string
	iamMW := func(action string) echo.MiddlewareFunc {
		actions = append(actions, action)
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterProviderTestRoutes(g, iamMW)

	// RegisterProviderTestRoutes constructs the middlewares in this fixed order:
	//   1) test-connection            -> provider:create   (raised, F-0369)
	//   2) /:id/test provider gate     -> provider:read
	//   3) /:id/test credential gate   -> credential:probe
	//   4) provider-health             -> provider:read
	want := []string{
		"admin:provider.create",
		"admin:provider.read",
		"admin:credential.probe",
		"admin:provider.read",
	}
	if len(actions) != len(want) {
		t.Fatalf("iamMW invoked %d times %v; want %d %v", len(actions), actions, len(want), want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("iamMW[%d] = %q; want %q", i, actions[i], want[i])
		}
	}
}

func TestProviderTestConnection_BadJSON_Returns400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderTestConnection(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestProviderTestConnection_MissingRequired_Returns400(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no name", `{"adapterType":"openai","baseUrl":"http://x"}`},
		{"no baseUrl", `{"name":"n","adapterType":"openai"}`},
		{"no adapterType", `{"name":"n","baseUrl":"http://x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c, _ := echoCtx(req, rec, "u-1")
			_ = h.ProviderTestConnection(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d; want 400", tc.name, rec.Code)
			}
		})
	}
}

func TestProviderTestConnection_InvalidAdapterType_Returns400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"n","adapterType":"INVALID","baseUrl":"http://x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ProviderTestConnection(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "adapterType must be one of") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestProviderTestConnection_AIGatewayUnreachable_ReturnsOK_SuccessFalse(t *testing.T) {
	// forwardProviderTest returns 200 even on transport failure (per the handler contract).
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1", // no listener
	})
	body := `{"name":"test","adapterType":"openai","baseUrl":"https://api.openai.com","apiKey":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderTestConnection(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (transport failures are 200+error body)", rec.Code)
	}
	var body2 map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body2)
	if body2["success"] != false {
		t.Errorf("success should be false on unreachable gateway; got %v", body2)
	}
}

func TestProviderTestConnection_AIGatewayResponds_PassesThrough(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"latencyMs":42}`))
	}))
	defer gw.Close()

	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: gw.URL})
	body := `{"name":"test","adapterType":"openai","baseUrl":"https://api.openai.com","apiKey":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderTestConnection(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success should be true; got %v", resp)
	}
}

func TestProviderTest_ProviderNotFound_Returns404(t *testing.T) {
	mock, db := newMockStore(t)
	// GetProvider SQL: SELECT id, name, "displayName"... FROM "Provider" WHERE id = $1
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-missing").
		WillReturnRows(pgxmock.NewRows(providerCols))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-missing")
	if err := h.ProviderTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestProviderTest_ProviderGetError_Returns404(t *testing.T) {
	mock, db := newMockStore(t)
	// GetProvider SQL: SELECT id, name, "displayName"... FROM "Provider" WHERE id = $1
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("x").
		WillReturnError(errors.New("db err"))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.ProviderTest(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404 (get-provider error treated as not-found)", rec.Code)
	}
}

func TestProviderTest_HappyWithSpecificCredentialID(t *testing.T) {
	// When credentialId is specified, decryptCredentialByID is called.
	// The cred row will be returned; decryption will succeed using the test vault
	// since makeCredentialEncryptedRow uses literal "enc-key-blob" ciphertext.
	// The handler proceeds with whatever key and forwards to the gateway.
	mock, db := newMockStore(t)
	now := nowFixture()

	// GetProvider SQL: SELECT id, name, ... FROM "Provider" WHERE id = $1
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	// GetCredentialEncrypted — SQL: SELECT {CredMetadataColumns}, "encryptedKey"... FROM "Credential" WHERE id = $1
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols).
			AddRow(makeCredentialEncryptedRow(now, "v1")...))

	// AI gateway stub.
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer gw.Close()

	vault := newTestVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{AIGatewayURL: gw.URL})
	body := `{"credentialId":"cred-1"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.ProviderTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	// Handler returns 200 (gateway stub responds 200) even if decrypt yields empty key.
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderTest_HappyWithFirstCredential(t *testing.T) {
	// When no credentialId, getFirstCredentialKey is called.
	// getFirstCredentialKey calls ListCredentials which first runs COUNT(*) then the data query.
	mock, db := newMockStore(t)
	now := nowFixture()

	// GetProvider SQL: SELECT id, name, ... FROM "Provider" WHERE id = $1
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	// ListCredentials COUNT query: SELECT COUNT(*) FROM "Credential" c WHERE c."providerId" = $1 AND c.enabled = $2
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-1", true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))

	// ListCredentials data query: SELECT c.{CredMetadataColumns} FROM "Credential" c WHERE ... LIMIT $3 OFFSET $4
	mock.ExpectQuery(`FROM "Credential" c`).
		WithArgs("prov-1", true, 1, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).
			AddRow(makeCredentialRow(now)...))

	// GetCredentialEncrypted for the first cred.
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols).
			AddRow(makeCredentialEncryptedRow(now, "v1")...))

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer gw.Close()

	vault := newTestVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{AIGatewayURL: gw.URL})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.ProviderTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

// decryptCredential — multi-vault and vault paths

func TestDecryptCredential_MultiVault_Success(t *testing.T) {
	multi := newTestMultiVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, multi, ProxyConfig{})

	// Encrypt something with v2 (current) so we can decrypt it back. SEC-C1-02:
	// seal under the same row-identity AAD the decrypt path rebuilds from
	// (credentialID, providerID).
	v2Vault := vaultForKey(t, "v2")
	aad := keyderive.ProviderCredentialAAD("cred-tc", "prov-tc")
	enc, err := v2Vault.Encrypt("my-secret-key", aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	got := h.decryptCredential(context.Background(), "cred-tc", "prov-tc", enc.Ciphertext, enc.IV, enc.Tag, "v2")
	if got != "my-secret-key" {
		t.Errorf("decryptCredential = %q; want 'my-secret-key'", got)
	}
}

func TestDecryptCredential_MultiVault_WrongKey_ReturnsEmpty(t *testing.T) {
	multi := newTestMultiVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, multi, ProxyConfig{})
	got := h.decryptCredential(context.Background(), "cid", "pid", "bad-cipher", "bad-iv", "bad-tag", "v999")
	if got != "" {
		t.Errorf("bad key should return empty; got %q", got)
	}
}

func TestDecryptCredential_SingleVault_Success(t *testing.T) {
	vault := newTestVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{})

	aad := keyderive.ProviderCredentialAAD("cred-sv", "prov-sv")
	enc, err := vault.Encrypt("plain-key", aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got := h.decryptCredential(context.Background(), "cred-sv", "prov-sv", enc.Ciphertext, enc.IV, enc.Tag, "v1")
	if got != "plain-key" {
		t.Errorf("decryptCredential = %q; want 'plain-key'", got)
	}
}

func TestDecryptCredential_SingleVault_BadCiphertext_ReturnsEmpty(t *testing.T) {
	vault := newTestVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{})
	got := h.decryptCredential(context.Background(), "c", "p", "bad", "bad", "bad", "v1")
	if got != "" {
		t.Errorf("bad ciphertext should return empty; got %q", got)
	}
}

func TestDecryptCredential_NoVaults_ReturnsEmpty(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	got := h.decryptCredential(context.Background(), "cid", "pid", "c", "i", "t", "v1")
	if got != "" {
		t.Errorf("no vaults should return empty; got %q", got)
	}
}

// decryptCredentialByID — DB error and not-found branches

func TestDecryptCredentialByID_DBError_ReturnsEmpty(t *testing.T) {
	mock, db := newMockStore(t)
	// GetCredentialEncrypted SQL: SELECT {CredMetadataColumns}, "encryptedKey"... FROM "Credential" WHERE id = $1
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("cred-bad").
		WillReturnError(errors.New("planner err"))

	vault := newTestVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{})
	got := h.decryptCredentialByID(context.Background(), "cred-bad")
	if got != "" {
		t.Errorf("DB error should return empty; got %q", got)
	}
}

func TestDecryptCredentialByID_NotFound_ReturnsEmpty(t *testing.T) {
	mock, db := newMockStore(t)
	// GetCredentialEncrypted SQL: SELECT {CredMetadataColumns}, "encryptedKey"... FROM "Credential" WHERE id = $1
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(credentialEncryptedCols))

	vault := newTestVault(t)
	h := newHandler(db, nil, &auditSpy{}, nil, vault, nil, ProxyConfig{})
	got := h.decryptCredentialByID(context.Background(), "missing")
	if got != "" {
		t.Errorf("not-found should return empty; got %q", got)
	}
}

// getFirstCredentialKey — no creds and DB error branches

func TestGetFirstCredentialKey_NoCreds_ReturnsEmpty(t *testing.T) {
	mock, db := newMockStore(t)
	// ListCredentials runs COUNT(*) first, then the data query.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-x", true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM "Credential" c`).
		WithArgs("prov-x", true, 1, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	got := h.getFirstCredentialKey(context.Background(), "prov-x")
	if got != "" {
		t.Errorf("no creds should return empty; got %q", got)
	}
}

func TestGetFirstCredentialKey_ListError_ReturnsEmpty(t *testing.T) {
	mock, db := newMockStore(t)
	// ListCredentials COUNT(*) query fails.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-x", true).
		WillReturnError(errors.New("db err"))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	got := h.getFirstCredentialKey(context.Background(), "prov-x")
	if got != "" {
		t.Errorf("list error should return empty; got %q", got)
	}
}

func TestGetFirstCredentialKey_GetEncryptedError_ReturnsEmpty(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()

	// ListCredentials COUNT(*) succeeds with 1.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-1", true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
	// ListCredentials data query succeeds.
	mock.ExpectQuery(`FROM "Credential" c`).
		WithArgs("prov-1", true, 1, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	// GetCredentialEncrypted returns error.
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("cred-1").
		WillReturnError(errors.New("enc fetch err"))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	got := h.getFirstCredentialKey(context.Background(), "prov-1")
	if got != "" {
		t.Errorf("get-encrypted error should return empty; got %q", got)
	}
}

func TestListProviderHealth_Happy(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "status", "rollingErrorRate", "avgLatencyMs",
			"sampleCount", "lastRequestAt", "lastErrorAt",
		}).AddRow("prov-1", "test-provider", "healthy", 0.01, 120, 50, &now, &now))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/provider-health", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListProviderHealth(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("expected 1 health row; got %d", len(data))
	}
}

func TestListProviderHealth_DBError_Returns500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnError(errors.New("db err"))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/provider-health", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListProviderHealth(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

// forwardProviderTest — direct unit test (not through ProviderTestConnection)

func TestForwardProviderTest_GatewayUnreachable_Returns200WithError(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1",
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.forwardProviderTest(c, "test-provider", "openai", "http://api.example.com", "sk-x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["success"] != false {
		t.Errorf("success should be false on unreachable gateway; got %v", body)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("error field should be present")
	}
}

// Compile-time guard: ensure time is actually used in this file.
var _ = time.Now

// Compile-time guard: io is used.
var _ = io.EOF
