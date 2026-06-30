package providers

// Security-sensitive credential/provider/model writes must
// fail loud (HTTP 502) when the Category B invalidation push to Hub fails, so
// the data plane does not keep serving a stale (e.g. just-deleted) credential.
// Each test asserts the CP DB write committed (truth preserved), the response
// is 502 with the propagation_error envelope, and NO success audit row was
// enqueued.

import (
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func assertProviderPropagationEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v; raw=%s", err, rec.Body.String())
	}
	if body.Error.Type != "propagation_error" || body.Error.Code != "HUB_PROPAGATION_FAILED" {
		t.Errorf("envelope = %+v; want propagation_error/HUB_PROPAGATION_FAILED", body.Error)
	}
}

func TestCreateCredential_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`INSERT INTO "Credential"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	v := newTestVault(t)
	hub := &hubSpy{invalidateErr: errors.New("hub unreachable")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, v, nil, ProxyConfig{})

	body := `{"name":"alpha","providerId":"prov-1","apiKey":"sk-test","rotationState":"none","selectionWeight":50}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateCredential(c); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/credentials" {
		t.Errorf("hub invalidate = %v; want 1× ai-gateway/credentials", hub.seen())
	}
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0 (must not log success on push failure)", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB insert did not commit before push: %v", err)
	}
}

func TestDeleteModel_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectExec(`DELETE FROM "Model" WHERE id`).
		WithArgs("model-1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("model-1")
	if err := h.DeleteModel(c); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/models" {
		t.Errorf("hub invalidate = %v; want 1× ai-gateway/models", hub.seen())
	}
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB delete did not commit before push: %v", err)
	}
}

func TestUpdateCredential_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	mock.ExpectQuery(`UPDATE "Credential" SET`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"name":"renamed"}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	if err := h.UpdateCredential(c); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestDeleteCredential_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	mock.ExpectExec(`DELETE FROM "Credential" WHERE id`).
		WithArgs("cred-1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	if err := h.DeleteCredential(c); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB delete did not commit before push: %v", err)
	}
}

func TestUpdateCredentialReliabilityOverrides_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"authFailThreshold":10}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	if err := h.UpdateCredentialReliabilityOverrides(c); err != nil {
		t.Fatalf("UpdateCredentialReliabilityOverrides: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestUpdateModel_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectQuery(`UPDATE "Model"`).
		WithArgs(anyArgs(22)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"type":"chat","name":"Updated","aliases":["a"],"features":["vision"]}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("model-1")
	if err := h.UpdateModel(c); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestCreateProvider_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(anyArgs(12)...).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectCommit()
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"name":"alpha","baseUrl":"https://x","adapterType":"openai"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateProvider(c); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	// The push fails on the very first key, before incrementConfigVersion runs;
	// the provider row is already committed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("provider insert did not commit before push: %v", err)
	}
}

func TestUpdateProvider_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`UPDATE "Provider"`).
		WithArgs(anyArgs(15)...).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"displayName":"D"}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.UpdateProvider(c); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestDeleteProvider_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "Model"`).WithArgs("prov-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec(`DELETE FROM "Provider"`).WithArgs("prov-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("provider delete did not commit before push: %v", err)
	}
}

func TestAddProviderModel_HubFailure502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(anyArgs(20)...).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	hub := &hubSpy{invalidateErr: errors.New("hub down")}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"name":"gpt-4o","type":"chat"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.AddProviderModel(c); err != nil {
		t.Fatalf("AddProviderModel: %v", err)
	}
	assertProviderPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

// anyArgs returns n pgxmock.AnyArg() matchers for variadic WithArgs calls.
func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}
