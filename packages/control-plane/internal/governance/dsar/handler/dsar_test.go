package dsar

import (
	"bytes"
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

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/dsar/dsarstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

type fakeDB struct {
	mu         sync.Mutex
	rows       map[string]*dsarstore.DSARRequest
	listErr    error
	getErr     error
	createErr  error
	updateErr  error
	accessErr  error
	erasureErr error
}

func newFakeDB() *fakeDB {
	return &fakeDB{rows: map[string]*dsarstore.DSARRequest{}}
}

func (f *fakeDB) seed(r dsarstore.DSARRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := r
	f.rows[r.ID] = &cp
}

func (f *fakeDB) ListDSARRequests(_ context.Context, status string, limit, offset int) ([]dsarstore.DSARRequest, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	var all []dsarstore.DSARRequest
	for _, r := range f.rows {
		if status == "" || r.Status == status {
			all = append(all, *r)
		}
	}
	total := len(all)
	if offset >= total {
		return []dsarstore.DSARRequest{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

func (f *fakeDB) GetDSARRequest(_ context.Context, id string) (*dsarstore.DSARRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.rows[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (f *fakeDB) CreateDSARRequest(_ context.Context, p dsarstore.CreateDSARRequestParams) (*dsarstore.DSARRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	r := &dsarstore.DSARRequest{
		ID:        "dsar-new-1",
		SubjectID: p.SubjectID,
		Contact:   p.Contact,
		Type:      p.Type,
		Status:    "PENDING",
		Notes:     p.Notes,
		CreatedBy: p.CreatedBy,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	f.rows[r.ID] = r
	cp := *r
	return &cp, nil
}

func (f *fakeDB) UpdateDSARRequest(_ context.Context, id string, p dsarstore.UpdateDSARParams) (*dsarstore.DSARRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	r, ok := f.rows[id]
	if !ok {
		return nil, nil
	}
	if p.Status != nil {
		r.Status = *p.Status
	}
	if p.Notes != nil {
		r.Notes = p.Notes
	}
	if p.CompletedAt != nil {
		r.CompletedAt = p.CompletedAt
	}
	if p.Outcome != nil {
		r.Outcome = p.Outcome
	}
	if p.UpdatedBy != nil {
		r.UpdatedBy = p.UpdatedBy
	}
	r.UpdatedAt = time.Now()
	cp := *r
	return &cp, nil
}

func (f *fakeDB) FulfillDSARAccess(_ context.Context, subjectID string) (*dsarstore.DSARAccessExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accessErr != nil {
		return nil, f.accessErr
	}
	return &dsarstore.DSARAccessExport{
		VKRows:    []map[string]any{{"id": "te-1", "subjectId": subjectID}},
		AgentRows: []map[string]any{},
		Devices:   []map[string]any{},
	}, nil
}

func (f *fakeDB) FulfillDSARErasure(_ context.Context, _ string) (*dsarstore.DSARErasureResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.erasureErr != nil {
		return nil, f.erasureErr
	}
	return &dsarstore.DSARErasureResult{VKAnonymised: 5, AgentAnonymised: 2, TotalAnonymised: 7}, nil
}

// mqNop satisfies mq.Producer; used by audit.NewWriter in tests.
type mqNop struct{}

func (m *mqNop) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Close() error                                        { return nil }

func newTestHandler(db *fakeDB) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&mqNop{}, "audit", logger)
	return &Handler{db: db, audit: aw, logger: logger}
}

func echoCtx(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var bodyR io.Reader
	if body != "" {
		bodyR = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyR)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:   "user-admin",
		KeyName: "Test Admin",
	})
	return c, rec
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

func TestParsePagination_Defaults(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 || pg.Offset != 0 {
		t.Errorf("got limit=%d offset=%d; want 50/0", pg.Limit, pg.Offset)
	}
}

func TestParsePagination_Custom(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=10&offset=20", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 10 || pg.Offset != 20 {
		t.Errorf("got limit=%d offset=%d; want 10/20", pg.Limit, pg.Offset)
	}
}

func TestParsePagination_Clamped(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("got limit=%d; want 1000 (clamped)", pg.Limit)
	}
}

func TestParsePagination_InvalidIgnored(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc&offset=-5", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 || pg.Offset != 0 {
		t.Errorf("got limit=%d offset=%d; want 50/0", pg.Limit, pg.Offset)
	}
}

// errJSON / internalServerError

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("msg", "validation_error", "code-x")
	env, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope")
	}
	if env["message"] != "msg" || env["type"] != "validation_error" || env["code"] != "code-x" {
		t.Errorf("unexpected envelope: %+v", env)
	}
}

func TestInternalServerError_StatusAndBody(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = internalServerError(c, "boom")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "boom") || !strings.Contains(body, "server_error") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestActorFromContext_WithAuth(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/", "")
	a := actorFromContext(c)
	if a.UserID != "user-admin" {
		t.Errorf("UserID = %q; want user-admin", a.UserID)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	a := actorFromContext(c)
	if a.UserID != "" {
		t.Errorf("expected empty UserID without auth")
	}
}

func TestListDSAR_Empty(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar", "")
	if err := h.ListDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 0 {
		t.Errorf("total = %v; want 0", m["total"])
	}
}

func TestListDSAR_WithData(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", SubjectID: "s1", Type: "ACCESS", Status: "PENDING", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.seed(dsarstore.DSARRequest{ID: "d2", SubjectID: "s2", Type: "ERASURE", Status: "COMPLETED", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar", "")
	if err := h.ListDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 2 {
		t.Errorf("total = %v; want 2", m["total"])
	}
}

func TestListDSAR_StoreError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.listErr = errors.New("db down")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar", "")
	if err := h.ListDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestListDSAR_StatusFilter(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.seed(dsarstore.DSARRequest{ID: "d2", Status: "COMPLETED", Type: "ERASURE", SubjectID: "s2", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/dsar?status=PENDING", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/dsar")
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u"})

	if err := h.ListDSAR(c); err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 1 {
		t.Errorf("total = %v; want 1 (filtered by PENDING)", m["total"])
	}
}

func TestGetDSAR_Found(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", SubjectID: "s1", Type: "ACCESS", Status: "PENDING", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar/d1", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.GetDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["id"] != "d1" {
		t.Errorf("id = %v; want d1", m["id"])
	}
}

func TestGetDSAR_NotFound(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar/nope", "")
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.GetDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGetDSAR_StoreError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.getErr = errors.New("db error")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodGet, "/dsar/d1", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.GetDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateDSAR_HappyPath_ACCESS(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"subjectId": "subj-1", "type": "ACCESS"})
	c, rec := echoCtx(http.MethodPost, "/dsar", body)
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["subjectId"] != "subj-1" {
		t.Errorf("subjectId = %v; want subj-1", m["subjectId"])
	}
	if m["status"] != "PENDING" {
		t.Errorf("status = %v; want PENDING", m["status"])
	}
}

func TestCreateDSAR_HappyPath_ERASURE(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"subjectId": "subj-2", "type": "ERASURE"})
	c, rec := echoCtx(http.MethodPost, "/dsar", body)
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
}

func TestCreateDSAR_MissingSubjectID_Returns400(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"type": "ACCESS"})
	c, rec := echoCtx(http.MethodPost, "/dsar", body)
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing subjectId)", rec.Code)
	}
}

func TestCreateDSAR_InvalidType_Returns400(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"subjectId": "s1", "type": "INVALID"})
	c, rec := echoCtx(http.MethodPost, "/dsar", body)
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (invalid type)", rec.Code)
	}
	body2 := rec.Body.String()
	if !strings.Contains(body2, "ACCESS or ERASURE") {
		t.Errorf("expected type validation message; got: %s", body2)
	}
}

func TestCreateDSAR_InvalidBody_Returns400(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar", "{bad json")
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateDSAR_StoreError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.createErr = errors.New("db error")
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"subjectId": "s1", "type": "ACCESS"})
	c, rec := echoCtx(http.MethodPost, "/dsar", body)
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateDSAR_NoAuth_UsesUnknownCreatedBy(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"subjectId": "s1", "type": "ACCESS"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/dsar", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// No WithAdminAuth — createdBy should default to "unknown"
	if err := h.CreateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
}

func TestUpdateDSAR_ValidTransition_PENDING_to_IN_PROGRESS(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "IN_PROGRESS"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["status"] != "IN_PROGRESS" {
		t.Errorf("status = %v; want IN_PROGRESS", m["status"])
	}
}

func TestUpdateDSAR_ValidTransition_PENDING_to_REJECTED(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "REJECTED"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestUpdateDSAR_InvalidTransition_PENDING_to_COMPLETED(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "COMPLETED"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (invalid transition PENDING→COMPLETED)", rec.Code)
	}
	body2 := rec.Body.String()
	if !strings.Contains(body2, "transition") {
		t.Errorf("expected transition error message; got: %s", body2)
	}
}

func TestUpdateDSAR_InvalidTransition_COMPLETED_to_anything(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "COMPLETED", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "PENDING"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (COMPLETED is terminal)", rec.Code)
	}
}

func TestUpdateDSAR_NotFound_Returns404(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "IN_PROGRESS"})
	c, rec := echoCtx(http.MethodPut, "/dsar/missing", body)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateDSAR_GetError_Returns404(t *testing.T) {
	db := newFakeDB()
	db.getErr = errors.New("db down")
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "IN_PROGRESS"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	// GetDSARRequest error → treat as not found
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateDSAR_UpdateError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.updateErr = errors.New("db error")
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "IN_PROGRESS"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateDSAR_CompletedSetsCompletedAt(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "IN_PROGRESS", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	body := mustJSON(t, map[string]any{"status": "COMPLETED"})
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", body)
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestUpdateDSAR_InvalidBody_Returns400(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", Status: "PENDING", Type: "ACCESS", SubjectID: "s1", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPut, "/dsar/d1", "{bad json")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.UpdateDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// FulfillDSAR — ACCESS type

func TestFulfillDSAR_ACCESS_HappyPath(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", SubjectID: "subj-1", Type: "ACCESS", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d1/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["export"] == nil {
		t.Errorf("expected export in response; got: %v", m)
	}
	// Verify the request status was updated to COMPLETED
	req := m["request"].(map[string]any)
	if req["status"] != "COMPLETED" {
		t.Errorf("status = %v; want COMPLETED", req["status"])
	}
}

func TestFulfillDSAR_ACCESS_FulfillError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", SubjectID: "subj-1", Type: "ACCESS", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.accessErr = errors.New("access fulfill error")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d1/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestFulfillDSAR_ACCESS_UpdateAfterFulfillError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d1", SubjectID: "subj-1", Type: "ACCESS", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.updateErr = errors.New("update error after fulfill")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d1/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

// FulfillDSAR — ERASURE type

func TestFulfillDSAR_ERASURE_HappyPath(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d2", SubjectID: "subj-2", Type: "ERASURE", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d2/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d2")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	outcome := m["outcome"].(map[string]any)
	if outcome["vkAnonymised"].(float64) != 5 {
		t.Errorf("vkAnonymised = %v; want 5", outcome["vkAnonymised"])
	}
}

func TestFulfillDSAR_ERASURE_FulfillError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d2", SubjectID: "subj-2", Type: "ERASURE", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.erasureErr = errors.New("erasure error")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d2/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d2")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestFulfillDSAR_ERASURE_UpdateError_Returns500(t *testing.T) {
	db := newFakeDB()
	db.seed(dsarstore.DSARRequest{ID: "d2", SubjectID: "subj-2", Type: "ERASURE", Status: "IN_PROGRESS", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.updateErr = errors.New("update after erasure error")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d2/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d2")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestFulfillDSAR_NotFound_Returns404(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/missing/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestFulfillDSAR_GetError_Returns404(t *testing.T) {
	db := newFakeDB()
	db.getErr = errors.New("db down")
	h := newTestHandler(db)
	c, rec := echoCtx(http.MethodPost, "/dsar/d1/fulfill", "")
	c.SetParamNames("id")
	c.SetParamValues("d1")
	if err := h.FulfillDSAR(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

// Route registration — IAM actions smoke-check

func TestRegisterDSARRoutes_DoesNotPanic(t *testing.T) {
	db := newFakeDB()
	h := newTestHandler(db)
	e := echo.New()
	g := e.Group("/api/admin")
	passthroughIAM := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	// Must not panic
	h.RegisterDSARRoutes(g, passthroughIAM)
}
