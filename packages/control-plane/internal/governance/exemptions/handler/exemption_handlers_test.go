package exemption

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// Test fixtures

// fakeHub captures Hub.InvalidateConfig calls. Every
// compliance-exemption mutation fans out a Cat B invalidate signal to
// both receivers (compliance-proxy + agent), so each call to the
// helper produces two invalidate hits.
type fakeHub struct {
	mu                 sync.Mutex
	invalidateHits     int
	invalidateLastType string
	invalidateLastKey  string
	invalidateTypes    map[string]bool
}

func (f *fakeHub) InvalidateConfig(_ context.Context, thingType, configKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidateHits++
	f.invalidateLastType = thingType
	f.invalidateLastKey = configKey
	if f.invalidateTypes == nil {
		f.invalidateTypes = map[string]bool{}
	}
	f.invalidateTypes[thingType] = true
}

// fakeData is a programmable DataLayer stub. Each method records call
// counts and lets the test inject errors/return values.
type fakeData struct {
	mu sync.Mutex

	// CreateExemptionRequest
	createExReqHits int
	createExReq     *store.ExemptionRequest
	createExReqErr  error

	// GetExemptionRequest
	getExReqHits int
	getExReq     *store.ExemptionRequest
	getExReqErr  error

	// MarkExemptionRequestRejected
	markRejectHits int
	markRejectErr  error

	// ApproveExemptionRequestWithGrant
	approveHits  int
	approveGrant *store.ComplianceExemptionGrant
	approveErr   error

	// GetComplianceExemptionGrant
	getGrantHits int
	getGrant     *store.ComplianceExemptionGrant
	getGrantErr  error

	// GetComplianceExemptionGrantByExemptionRequestID
	getGrantByReqHits int
	getGrantByReq     *store.ComplianceExemptionGrant
	getGrantByReqErr  error

	// InsertComplianceExemptionGrant
	insertGrantHits  int
	insertGrant      *store.ComplianceExemptionGrant
	insertGrantErr   error
	insertGrantInput store.ComplianceExemptionGrantInsert

	// UpdateComplianceExemptionGrantInactive
	updateInactiveHits     int
	updateInactiveErr      error
	updateInactiveLastID   string
	updateInactiveLastBool bool

	// DeleteComplianceExemptionGrantIfPreActivation
	deleteHits int
	deleteOK   bool
	deleteErr  error

	// ListUnifiedExemptionsPage
	listUnifiedHits  int
	listUnified      []store.UnifiedExemptionRow
	listUnifiedTotal int
	listUnifiedErr   error

	// GetUnifiedExemptionByID
	getUnifiedHits int
	getUnified     *store.UnifiedExemptionRow
	getUnifiedErr  error
}

func (f *fakeData) CreateExemptionRequest(_ context.Context, _ map[string]any) (*store.ExemptionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createExReqHits++
	return f.createExReq, f.createExReqErr
}

func (f *fakeData) GetExemptionRequest(_ context.Context, _ string) (*store.ExemptionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getExReqHits++
	return f.getExReq, f.getExReqErr
}

func (f *fakeData) MarkExemptionRequestRejected(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markRejectHits++
	return f.markRejectErr
}

func (f *fakeData) ApproveExemptionRequestWithGrant(_ context.Context, _, _, _ string) (*store.ComplianceExemptionGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveHits++
	return f.approveGrant, f.approveErr
}

func (f *fakeData) GetComplianceExemptionGrant(_ context.Context, _ string) (*store.ComplianceExemptionGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getGrantHits++
	return f.getGrant, f.getGrantErr
}

func (f *fakeData) GetComplianceExemptionGrantByExemptionRequestID(_ context.Context, _ string) (*store.ComplianceExemptionGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getGrantByReqHits++
	return f.getGrantByReq, f.getGrantByReqErr
}

func (f *fakeData) InsertComplianceExemptionGrant(_ context.Context, p store.ComplianceExemptionGrantInsert) (*store.ComplianceExemptionGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertGrantHits++
	f.insertGrantInput = p
	return f.insertGrant, f.insertGrantErr
}

func (f *fakeData) UpdateComplianceExemptionGrantInactive(_ context.Context, id string, inactive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateInactiveHits++
	f.updateInactiveLastID = id
	f.updateInactiveLastBool = inactive
	return f.updateInactiveErr
}

func (f *fakeData) DeleteComplianceExemptionGrantIfPreActivation(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteHits++
	return f.deleteOK, f.deleteErr
}

func (f *fakeData) ListUnifiedExemptionsPage(_ context.Context, _ string, _ time.Time, _, _ int) ([]store.UnifiedExemptionRow, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listUnifiedHits++
	return f.listUnified, f.listUnifiedTotal, f.listUnifiedErr
}

func (f *fakeData) GetUnifiedExemptionByID(_ context.Context, _ string, _ time.Time) (*store.UnifiedExemptionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getUnifiedHits++
	return f.getUnified, f.getUnifiedErr
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noopAuditWriter returns a non-nil *audit.Writer whose producer is nil so
// LogObserved is a no-op. This is enough to drive the `h.audit != nil` true
// branches in every handler without wiring an MQ.
func noopAuditWriter() *audit.Writer {
	return audit.NewWriter(nil, "audit", quietLogger())
}

// captureProducer records the bytes enqueued to the audit queue so a test can
// decode the emitted AdminAuditMessage and assert EntityID / AfterState.
type captureProducer struct {
	mu   sync.Mutex
	last []byte
}

func (p *captureProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *captureProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = append([]byte(nil), data...)
	return nil
}
func (p *captureProducer) Close() error { return nil }

func (p *captureProducer) decode(t *testing.T) mq.AdminAuditMessage {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		t.Fatal("no audit message was enqueued")
	}
	var m mq.AdminAuditMessage
	if err := json.Unmarshal(p.last, &m); err != nil {
		t.Fatalf("decode audit message: %v", err)
	}
	return m
}

func newHandler(data DataLayer, hub HubAPI) *Handler {
	return &Handler{
		hub:       hub,
		logger:    quietLogger(),
		dataLayer: data,
	}
}

// newHandlerWithDB constructs a handler that uses pgxmock-backed *store.DB
// as the DataLayer for endpoints that hit DB directly (e.g. CreateRequest).
func newHandlerWithDB(t *testing.T, hub HubAPI) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	h := New(Deps{DataLayer: db, Hub: hub, Logger: quietLogger()})
	return mock, h
}

// ctxWithAuth returns an Echo context with a populated AdminAuth.
func ctxWithAuth(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "user-1", KeyName: "Alice"})
	return c, rec
}

func withParam(c echo.Context, name, value string) echo.Context {
	c.SetParamNames(name)
	c.SetParamValues(value)
	return c
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return v
}

// data() seam + New() default-logger

func TestData_OverrideTakesPrecedence(t *testing.T) {
	fd := &fakeData{}
	h := &Handler{dataLayer: fd}
	if h.data() != fd {
		t.Error("override should win")
	}
}

func TestData_FallsBackToDB(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	db := store.NewWithPgxPool(mock)
	h := &Handler{dataLayer: db}
	if h.data() == nil {
		t.Error("expected dataLayer to be non-nil")
	}
}

func TestNew_NilLoggerDefaults(t *testing.T) {
	h := New(Deps{})
	if h.logger == nil {
		t.Error("expected default logger when nil")
	}
}

func TestNew_WiresAllDeps(t *testing.T) {
	fd := &fakeData{}
	hub := &fakeHub{}
	log := quietLogger()
	h := New(Deps{DataLayer: fd, Hub: hub, Logger: log})
	if h.dataLayer != fd || h.hub != hub || h.logger != log {
		t.Error("Deps wiring lost a field")
	}
}

func TestActorFromContext_Populated(t *testing.T) {
	c, _ := ctxWithAuth(http.MethodGet, "/", "")
	a := actorFromContext(c)
	if a.UserID != "user-1" || a.Name != "Alice" {
		t.Errorf("actor = %+v", a)
	}
}

func TestListGrants_OK(t *testing.T) {
	fd := &fakeData{
		listUnified:      []store.UnifiedExemptionRow{{ID: "ex-1", Kind: "grant"}},
		listUnifiedTotal: 1,
	}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/?tab=effective&limit=20&offset=0", "")
	if err := h.ListGrants(c); err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["total"].(float64) != 1 {
		t.Errorf("total = %v", body["total"])
	}
}

func TestListGrants_ValidationErrorFromStore(t *testing.T) {
	fd := &fakeData{listUnifiedErr: errors.New("invalid tab")}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/?tab=bogus", "")
	_ = h.ListGrants(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_InvalidJSON(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "not-json")
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_MissingFields(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", `{"durationMinutes":60,"reason":"abcd"}`)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPostGrant_BadDuration(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	body := `{"sourceIP":"10.0.0.1","targetHost":"api.openai.com","durationMinutes":0,"reason":"abcd"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	// duration too large
	body = `{"sourceIP":"10.0.0.1","targetHost":"api.openai.com","durationMinutes":99999,"reason":"abcd"}`
	c, rec = ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("(too-large) code = %d", rec.Code)
	}
}

func TestPostGrant_BadReason(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"ab"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("short reason code = %d", rec.Code)
	}
	longReason := strings.Repeat("a", 501)
	body = `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"` + longReason + `"}`
	c, rec = ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("long reason code = %d", rec.Code)
	}
}

func TestPostGrant_HubNil(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"abcd"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_BadEffectiveFrom(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"abcd","effectiveFrom":"not-rfc3339"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_ExpiresInPast(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	// effectiveFrom in distant past + small duration → expiresAt in the past
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":1,"reason":"abcd","effectiveFrom":"2000-01-01T00:00:00Z"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_InsertFails(t *testing.T) {
	fd := &fakeData{insertGrantErr: errors.New("boom")}
	h := newHandler(fd, &fakeHub{})
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"abcd"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.PostGrant(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPostGrant_Happy(t *testing.T) {
	fd := &fakeData{
		insertGrant: &store.ComplianceExemptionGrant{ID: "g-1"},
	}
	hub := &fakeHub{}
	h := newHandler(fd, hub)
	body := `{"sourceIP":"10.0.0.1","targetHost":"x.com","durationMinutes":60,"reason":"abcd"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.PostGrant(c); err != nil {
		t.Fatalf("PostGrant: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if hub.invalidateHits != 2 {
		t.Errorf("invalidate hits = %d, want 2 (compliance-proxy + agent)", hub.invalidateHits)
	}
	if !hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] || hub.invalidateLastKey != "exemptions" {
		t.Errorf("invalidate fan-out = %v key=%q, want {compliance-proxy, agent} key=exemptions",
			hub.invalidateTypes, hub.invalidateLastKey)
	}
}

// F-0265: the grant-create audit row must carry the grant EntityID and the
// grant scope (host, sourceIP, window, reason) so "who exempted which host,
// for how long" is answerable from the audit row alone.
func TestPostGrant_AuditCarriesEntityIDAndScope(t *testing.T) {
	fd := &fakeData{
		insertGrant: &store.ComplianceExemptionGrant{
			ID:              "grant-77",
			SourceIP:        "10.0.0.1",
			TargetHost:      "api.openai.com",
			Reason:          "incident-bridge",
			DurationMinutes: 60,
		},
	}
	cap := &captureProducer{}
	h := newHandler(fd, &fakeHub{})
	h.audit = audit.NewWriter(cap, "audit", quietLogger())
	body := `{"sourceIP":"10.0.0.1","targetHost":"api.openai.com","durationMinutes":60,"reason":"incident-bridge"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.PostGrant(c); err != nil {
		t.Fatalf("PostGrant: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	msg := cap.decode(t)
	if msg.EntityID != "grant-77" {
		t.Errorf("audit EntityID = %q; want grant-77 (was empty before F-0265)", msg.EntityID)
	}
	after, ok := msg.AfterState.(map[string]any)
	if !ok {
		t.Fatalf("AfterState type = %T; want map", msg.AfterState)
	}
	if after["targetHost"] != "api.openai.com" || after["sourceIP"] != "10.0.0.1" || after["reason"] != "incident-bridge" {
		t.Errorf("AfterState missing grant scope: %+v", after)
	}
	for _, k := range []string{"effectiveFrom", "expiresAt"} {
		if _, present := after[k]; !present {
			t.Errorf("AfterState missing window field %q: %+v", k, after)
		}
	}
}

func TestPatchGrant_EmptyID(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPatch, "/", `{"inactive":true}`)
	withParam(c, "id", "")
	_ = h.PatchGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPatchGrant_InvalidJSON(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPatch, "/", "not-json")
	withParam(c, "id", "g-1")
	_ = h.PatchGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPatchGrant_HubNil(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodPatch, "/", `{"inactive":true}`)
	withParam(c, "id", "g-1")
	_ = h.PatchGrant(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPatchGrant_NotFound(t *testing.T) {
	fd := &fakeData{updateInactiveErr: pgx.ErrNoRows}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPatch, "/", `{"inactive":true}`)
	withParam(c, "id", "g-1")
	_ = h.PatchGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPatchGrant_UpdateInternalErr(t *testing.T) {
	fd := &fakeData{updateInactiveErr: errors.New("db down")}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPatch, "/", `{"inactive":false}`)
	withParam(c, "id", "g-1")
	_ = h.PatchGrant(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPatchGrant_Happy(t *testing.T) {
	fd := &fakeData{}
	hub := &fakeHub{}
	h := newHandler(fd, hub)
	c, rec := ctxWithAuth(http.MethodPatch, "/", `{"inactive":true}`)
	withParam(c, "id", "g-1")
	if err := h.PatchGrant(c); err != nil {
		t.Fatalf("PatchGrant: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if fd.updateInactiveLastID != "g-1" || !fd.updateInactiveLastBool {
		t.Errorf("update call args: id=%q bool=%v", fd.updateInactiveLastID, fd.updateInactiveLastBool)
	}
	if hub.invalidateHits != 2 || hub.invalidateLastKey != "exemptions" ||
		!hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] {
		t.Errorf("invalidate fan-out broken: hits=%d key=%q types=%v",
			hub.invalidateHits, hub.invalidateLastKey, hub.invalidateTypes)
	}
}

func TestDeleteGrant_EmptyID(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_HubNil(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_DeleteErr(t *testing.T) {
	fd := &fakeData{deleteErr: errors.New("boom")}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_NotPreActivation_NotFound(t *testing.T) {
	fd := &fakeData{deleteOK: false, getGrant: nil}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_NotPreActivation_Activated(t *testing.T) {
	fd := &fakeData{deleteOK: false, getGrant: &store.ComplianceExemptionGrant{ID: "g-1"}}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_NotPreActivation_GetErr(t *testing.T) {
	fd := &fakeData{deleteOK: false, getGrantErr: errors.New("oops")}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	_ = h.DeleteGrant(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteGrant_Happy(t *testing.T) {
	fd := &fakeData{deleteOK: true}
	hub := &fakeHub{}
	h := newHandler(fd, hub)
	c, rec := ctxWithAuth(http.MethodDelete, "/", "")
	withParam(c, "id", "g-1")
	if err := h.DeleteGrant(c); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	if hub.invalidateHits != 2 || hub.invalidateLastKey != "exemptions" ||
		!hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] {
		t.Errorf("invalidate fan-out broken: hits=%d key=%q types=%v",
			hub.invalidateHits, hub.invalidateLastKey, hub.invalidateTypes)
	}
}

func TestGetUnified_EmptyID(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/", "")
	withParam(c, "id", "")
	_ = h.GetUnified(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetUnified_Err(t *testing.T) {
	fd := &fakeData{getUnifiedErr: errors.New("oops")}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.GetUnified(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetUnified_NotFound(t *testing.T) {
	fd := &fakeData{getUnified: nil}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.GetUnified(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetUnified_Happy(t *testing.T) {
	fd := &fakeData{getUnified: &store.UnifiedExemptionRow{ID: "ex-1", Kind: "grant"}}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodGet, "/", "")
	withParam(c, "id", "ex-1")
	if err := h.GetUnified(c); err != nil {
		t.Fatalf("GetUnified: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApproveRequest_EmptyID(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_HubNil(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_GetReqErr(t *testing.T) {
	fd := &fakeData{getExReqErr: errors.New("oops")}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_NotFound(t *testing.T) {
	h := newHandler(&fakeData{}, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_AlreadyApprovedWithGrantReapply(t *testing.T) {
	fd := &fakeData{
		getExReq:      &store.ExemptionRequest{ID: "ex-1", Status: "APPROVED"},
		getGrantByReq: &store.ComplianceExemptionGrant{ID: "g-1"},
	}
	hub := &fakeHub{}
	h := newHandler(fd, hub)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	if err := h.ApproveRequest(c); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["reapplied"] != true {
		t.Errorf("expected reapplied=true; got %v", body)
	}
	if hub.invalidateHits != 2 || hub.invalidateLastKey != "exemptions" ||
		!hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] {
		t.Errorf("invalidate fan-out broken: hits=%d key=%q types=%v",
			hub.invalidateHits, hub.invalidateLastKey, hub.invalidateTypes)
	}
}

func TestApproveRequest_AlreadyApproved_GetGrantErr(t *testing.T) {
	fd := &fakeData{
		getExReq:         &store.ExemptionRequest{ID: "ex-1", Status: "APPROVED"},
		getGrantByReqErr: errors.New("boom"),
	}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_AlreadyApproved_NoGrantFallsThrough_Conflict(t *testing.T) {
	// status=APPROVED but no grant → falls through, then status != PENDING → 409
	fd := &fakeData{
		getExReq:      &store.ExemptionRequest{ID: "ex-1", Status: "APPROVED"},
		getGrantByReq: nil,
	}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_NotPending_Conflict(t *testing.T) {
	fd := &fakeData{getExReq: &store.ExemptionRequest{ID: "ex-1", Status: "REJECTED"}}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_ApproveErr_NoRows_Conflict(t *testing.T) {
	fd := &fakeData{
		getExReq:   &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		approveErr: pgx.ErrNoRows,
	}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_ApproveErr_Generic(t *testing.T) {
	fd := &fakeData{
		getExReq:   &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		approveErr: errors.New("oops"),
	}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_ApproveReturnsNilGrant_NotFound(t *testing.T) {
	fd := &fakeData{
		getExReq:     &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		approveGrant: nil,
	}
	h := newHandler(fd, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.ApproveRequest(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestApproveRequest_Happy(t *testing.T) {
	fd := &fakeData{
		getExReq:     &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		approveGrant: &store.ComplianceExemptionGrant{ID: "g-1"},
	}
	hub := &fakeHub{}
	h := newHandler(fd, hub)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	if err := h.ApproveRequest(c); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if hub.invalidateHits != 2 || hub.invalidateLastKey != "exemptions" ||
		!hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] {
		t.Errorf("invalidate fan-out broken: hits=%d key=%q types=%v",
			hub.invalidateHits, hub.invalidateLastKey, hub.invalidateTypes)
	}
}

func TestRejectRequest_EmptyID(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_GetReqErr(t *testing.T) {
	fd := &fakeData{getExReqErr: errors.New("oops")}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_NotFound(t *testing.T) {
	h := newHandler(&fakeData{}, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_NotPending(t *testing.T) {
	fd := &fakeData{getExReq: &store.ExemptionRequest{ID: "ex-1", Status: "APPROVED"}}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_MarkErr_NoRows_Conflict(t *testing.T) {
	fd := &fakeData{
		getExReq:      &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		markRejectErr: pgx.ErrNoRows,
	}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_MarkErr_Generic(t *testing.T) {
	fd := &fakeData{
		getExReq:      &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"},
		markRejectErr: errors.New("oops"),
	}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	_ = h.RejectRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRejectRequest_Happy(t *testing.T) {
	fd := &fakeData{getExReq: &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"}}
	h := newHandler(fd, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	if err := h.RejectRequest(c); err != nil {
		t.Fatalf("RejectRequest: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if fd.markRejectHits != 1 {
		t.Errorf("markRejectHits = %d", fd.markRejectHits)
	}
}

// CreateRequest — employee-facing submit. Hits h.db.CreateExemptionRequest
// directly so must use pgxmock.

func TestCreateRequest_InvalidJSON(t *testing.T) {
	_, h := newHandlerWithDB(t, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", "not-json")
	_ = h.CreateRequest(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCreateRequest_MissingFields(t *testing.T) {
	_, h := newHandlerWithDB(t, nil)
	c, rec := ctxWithAuth(http.MethodPost, "/", `{"sourceIp":"10.0.0.1"}`)
	_ = h.CreateRequest(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCreateRequest_DBErr(t *testing.T) {
	mock, h := newHandlerWithDB(t, nil)
	mock.ExpectQuery(`INSERT INTO exemption_request`).
		WithArgs("tx-1", "10.0.0.1", "x.com", "need access", 240, "employee").
		WillReturnError(errors.New("db down"))
	body := `{"transactionId":"tx-1","sourceIp":"10.0.0.1","targetHost":"x.com","reason":"need access"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	_ = h.CreateRequest(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCreateRequest_HappyDefaults(t *testing.T) {
	mock, h := newHandlerWithDB(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO exemption_request`).
		WithArgs("tx-1", "10.0.0.1", "x.com", "need access", 240, "employee").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "transaction_id", "source_ip", "target_host", "reason", "status",
			"duration_minutes", "reviewed_by", "review_note", "reviewed_at", "createdAt", "requested_by",
		}).AddRow(
			"er-1", "tx-1", "10.0.0.1", "x.com", "need access", "PENDING",
			240, (*string)(nil), (*string)(nil), (*time.Time)(nil), now, "employee",
		))
	body := `{"transactionId":"tx-1","sourceIp":"10.0.0.1","targetHost":"x.com","reason":"need access"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.CreateRequest(c); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateRequest_HappyOverrides(t *testing.T) {
	mock, h := newHandlerWithDB(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO exemption_request`).
		WithArgs("tx-2", "10.0.0.2", "y.com", "vendor", 60, "alice").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "transaction_id", "source_ip", "target_host", "reason", "status",
			"duration_minutes", "reviewed_by", "review_note", "reviewed_at", "createdAt", "requested_by",
		}).AddRow(
			"er-2", "tx-2", "10.0.0.2", "y.com", "vendor", "PENDING",
			60, (*string)(nil), (*string)(nil), (*time.Time)(nil), now, "alice",
		))
	body := `{"transactionId":"tx-2","sourceIp":"10.0.0.2","targetHost":"y.com","reason":"vendor","durationMinutes":60,"requestedBy":"alice"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.CreateRequest(c); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

// invalidateExemptions — Cat B helper coverage: fan-out invalidate
// for both compliance-proxy and agent thing-types + audit-verb
// mapping for "delete" vs every other action ("create" / "update" /
// "approve" / "reconcile" all flow through the default VerbUpdate
// branch).

func TestInvalidateExemptions_NoHub_NoAudit_NoOp(t *testing.T) {
	// hub=nil + audit=nil + c=nil: the helper must not panic and must
	// emit no observable side effect.
	h := newHandler(&fakeData{}, nil)
	h.invalidateExemptions(context.Background(), "create", "", nil, Actor{}, nil)
}

func TestInvalidateExemptions_FiresHubInvalidateForBothLegs(t *testing.T) {
	hub := &fakeHub{}
	h := newHandler(&fakeData{}, hub)
	h.invalidateExemptions(context.Background(), "create", "g-1", map[string]any{"targetHost": "api.x"}, Actor{UserID: "u-1", Name: "Bob"}, nil)
	if hub.invalidateHits != 2 {
		t.Fatalf("invalidate hits = %d, want 2 (compliance-proxy + agent)", hub.invalidateHits)
	}
	if !hub.invalidateTypes["compliance-proxy"] || !hub.invalidateTypes["agent"] {
		t.Errorf("invalidate types = %v, want {compliance-proxy, agent}", hub.invalidateTypes)
	}
	if hub.invalidateLastKey != "exemptions" {
		t.Errorf("invalidate key = %s, want exemptions", hub.invalidateLastKey)
	}
}

func TestInvalidateExemptions_AuditSkippedWhenNoEchoCtx(t *testing.T) {
	// c == nil branch: audit block is short-circuited even if h.audit is wired.
	hub := &fakeHub{}
	h := newHandler(&fakeData{}, hub)
	h.audit = noopAuditWriter()
	h.invalidateExemptions(context.Background(), "create", "", nil, Actor{}, nil)
	// Hub invalidate still fires for both legs — proof the audit-skip
	// branch ran without blocking the invalidation.
	if hub.invalidateHits != 2 {
		t.Errorf("invalidate hits = %d, want 2", hub.invalidateHits)
	}
}

func TestInvalidateExemptions_AuditPath_Delete(t *testing.T) {
	// "delete" action must take the VerbDelete branch. We assert via the
	// no-panic audit path (audit writer producer is nil so LogObserved is
	// a no-op); the dispatch into that path is the coverage target.
	hub := &fakeHub{}
	h := newHandler(&fakeData{}, hub)
	h.audit = noopAuditWriter()
	c, _ := ctxWithAuth(http.MethodPost, "/", "")
	h.invalidateExemptions(context.Background(), "delete", "g-1", nil, Actor{UserID: "u-1", Name: "Bob"}, c)
	if hub.invalidateHits != 2 {
		t.Errorf("invalidate hits = %d, want 2", hub.invalidateHits)
	}
}

func TestInvalidateExemptions_AuditPath_Default(t *testing.T) {
	// "reconcile" / "create" / "update" / "approve" all collapse to the
	// VerbUpdate branch; exercise one to lock the mapping in.
	hub := &fakeHub{}
	h := newHandler(&fakeData{}, hub)
	h.audit = noopAuditWriter()
	c, _ := ctxWithAuth(http.MethodPost, "/", "")
	h.invalidateExemptions(context.Background(), "reconcile", "g-1", map[string]any{"exemptionRequestId": "r-1"}, Actor{UserID: "u-1", Name: "Bob"}, c)
	if hub.invalidateHits != 2 {
		t.Errorf("invalidate hits = %d, want 2", hub.invalidateHits)
	}
}

// audit.Writer wiring — non-nil branches in 6 handlers + syncShadow

func TestRejectRequest_WithAudit(t *testing.T) {
	fd := &fakeData{getExReq: &store.ExemptionRequest{ID: "ex-1", Status: "PENDING"}}
	h := newHandler(fd, nil)
	h.audit = noopAuditWriter()
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	withParam(c, "id", "ex-1")
	if err := h.RejectRequest(c); err != nil {
		t.Fatalf("RejectRequest: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCreateRequest_WithAudit(t *testing.T) {
	mock, h := newHandlerWithDB(t, nil)
	h.audit = noopAuditWriter()
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO exemption_request`).
		WithArgs("tx-3", "10.0.0.3", "z.com", "audit-on", 240, "employee").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "transaction_id", "source_ip", "target_host", "reason", "status",
			"duration_minutes", "reviewed_by", "review_note", "reviewed_at", "createdAt", "requested_by",
		}).AddRow(
			"er-3", "tx-3", "10.0.0.3", "z.com", "audit-on", "PENDING",
			240, (*string)(nil), (*string)(nil), (*time.Time)(nil), now, "employee",
		))
	body := `{"transactionId":"tx-3","sourceIp":"10.0.0.3","targetHost":"z.com","reason":"audit-on"}`
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.CreateRequest(c); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code = %d", rec.Code)
	}
}
