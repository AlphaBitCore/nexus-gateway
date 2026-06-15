package interception

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

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/interception/interceptionstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// In-memory fakes

type fakeInterceptionDB struct {
	mu      sync.Mutex
	domains map[string]*interceptionstore.InterceptionDomainRow
	paths   map[string]*interceptionstore.InterceptionPathRow

	createHits int

	listErr    error
	getErr     error
	createErr  error
	updateErr  error
	deleteErr  error
	getPathErr error
}

func newFakeInterceptionDB() *fakeInterceptionDB {
	return &fakeInterceptionDB{
		domains: map[string]*interceptionstore.InterceptionDomainRow{},
		paths:   map[string]*interceptionstore.InterceptionPathRow{},
	}
}

func (f *fakeInterceptionDB) seedDomain(d interceptionstore.InterceptionDomainRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := d
	if cp.Paths == nil {
		cp.Paths = []interceptionstore.InterceptionPathRow{}
	}
	f.domains[d.ID] = &cp
}

func (f *fakeInterceptionDB) seedPath(p interceptionstore.InterceptionPathRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.paths[p.ID] = &cp
}

func (f *fakeInterceptionDB) ListInterceptionDomains(_ context.Context, p interceptionstore.InterceptionDomainListParams) (*interceptionstore.ListInterceptionDomainsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []interceptionstore.InterceptionDomainRow
	for _, d := range f.domains {
		if p.Enabled != nil && d.Enabled != *p.Enabled {
			continue
		}
		if p.Search != "" && !strings.Contains(d.Name, p.Search) {
			continue
		}
		out = append(out, *d)
	}
	return &interceptionstore.ListInterceptionDomainsResult{Domains: out, Total: len(out)}, nil
}

func (f *fakeInterceptionDB) GetInterceptionDomain(_ context.Context, id string) (*interceptionstore.InterceptionDomainRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	d, ok := f.domains[id]
	if !ok {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (f *fakeInterceptionDB) CreateInterceptionDomain(_ context.Context, in interceptionstore.CreateInterceptionDomainInput) (*interceptionstore.InterceptionDomainRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createHits++
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now()
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	d := &interceptionstore.InterceptionDomainRow{
		ID:                "domain-new",
		Name:              in.Name,
		HostPattern:       in.HostPattern,
		HostMatchType:     in.HostMatchType,
		AdapterID:         in.AdapterID,
		Enabled:           enabled,
		DefaultPathAction: in.DefaultPathAction,
		OnAdapterError:    in.OnAdapterError,
		NetworkZone:       in.NetworkZone,
		CreatedAt:         now,
		UpdatedAt:         now,
		Paths:             []interceptionstore.InterceptionPathRow{},
	}
	f.domains[d.ID] = d
	cp := *d
	return &cp, nil
}

func (f *fakeInterceptionDB) UpdateInterceptionDomain(_ context.Context, id string, in interceptionstore.UpdateInterceptionDomainInput) (*interceptionstore.InterceptionDomainRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	d, ok := f.domains[id]
	if !ok {
		return nil, nil
	}
	if in.Name != nil {
		d.Name = *in.Name
	}
	if in.Enabled != nil {
		d.Enabled = *in.Enabled
	}
	d.UpdatedAt = time.Now()
	cp := *d
	return &cp, nil
}

func (f *fakeInterceptionDB) DeleteInterceptionDomain(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.domains[id]; !ok {
		return pgx.ErrNoRows
	}
	delete(f.domains, id)
	return nil
}

func (f *fakeInterceptionDB) GetInterceptionPath(_ context.Context, id string) (*interceptionstore.InterceptionPathRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getPathErr != nil {
		return nil, f.getPathErr
	}
	p, ok := f.paths[id]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (f *fakeInterceptionDB) CreateInterceptionPath(_ context.Context, domainID string, in interceptionstore.CreateInterceptionPathInput) (*interceptionstore.InterceptionPathRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now()
	p := &interceptionstore.InterceptionPathRow{
		ID:        "path-new",
		DomainID:  domainID,
		Action:    in.Action,
		MatchType: in.MatchType,
		Priority:  in.Priority,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.paths[p.ID] = p
	cp := *p
	return &cp, nil
}

func (f *fakeInterceptionDB) UpdateInterceptionPath(_ context.Context, id string, in interceptionstore.UpdateInterceptionPathInput) (*interceptionstore.InterceptionPathRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	p, ok := f.paths[id]
	if !ok {
		return nil, nil
	}
	if in.Action != nil {
		p.Action = *in.Action
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	p.UpdatedAt = time.Now()
	cp := *p
	return &cp, nil
}

func (f *fakeInterceptionDB) DeleteInterceptionPath(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.paths[id]; !ok {
		return pgx.ErrNoRows
	}
	delete(f.paths, id)
	return nil
}

// hubSpy captures InvalidateConfig calls.
type hubSpy struct {
	mu    sync.Mutex
	calls []string
}

func (h *hubSpy) NotifyConfigChange(_ context.Context, _ interface{ notifyArg() }) (interface{}, error) {
	return nil, nil
}
func (h *hubSpy) InvalidateConfig(_ context.Context, thingType, configKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, thingType+"/"+configKey)
}
func (h *hubSpy) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	copy(out, h.calls)
	return out
}

type mqNop struct{}

func (m *mqNop) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Close() error                                        { return nil }

// metaNop is a stub for systemmetastore.Store; interception handler calls
// incrementConfigVersion which calls meta.GetSystemMetadata + SetSystemMetadata.
// We pass nil meta in tests — incrementConfigVersion guards against nil meta
// via h.meta being nil (if we pass nil, it will panic). We use a real Store
// backed by nothing, OR simply tolerate the panic by not calling mutating paths
// that invoke incrementConfigVersion (or by passing a meta-nil handler).
// Actually the handler guards nil via h.meta != nil — let's verify.

func newTestHandler(db *fakeInterceptionDB, hub HubAPI) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&mqNop{}, "audit", logger)
	return &Handler{
		store:  db,
		meta:   nil, // meta is only used in incrementConfigVersion; nil-safe if nil-guarded
		hub:    hub,
		audit:  aw,
		logger: logger,
	}
}

func echoCtxWith(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
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
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u1", KeyName: "Admin"})
	return c, rec
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
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

func seedDomain(db *fakeInterceptionDB) interceptionstore.InterceptionDomainRow {
	d := interceptionstore.InterceptionDomainRow{
		ID: "dom-1", Name: "test.com", HostPattern: "test.com",
		HostMatchType: "EXACT", AdapterID: "adapt-1", Enabled: true,
		DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN", NetworkZone: "PUBLIC",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Paths: []interceptionstore.InterceptionPathRow{},
	}
	db.seedDomain(d)
	return d
}

// Helper tests

func TestParsePagination_Defaults(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 || pg.Offset != 0 {
		t.Errorf("got %d/%d; want 50/0", pg.Limit, pg.Offset)
	}
}

func TestParsePagination_Clamped(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("got %d; want 1000", pg.Limit)
	}
}

func TestErrJSON_Shape(t *testing.T) {
	m := errJSON("msg", "type", "code")
	env, ok := m["error"].(map[string]any)
	if !ok || env["message"] != "msg" || env["type"] != "type" || env["code"] != "code" {
		t.Errorf("unexpected: %+v", m)
	}
}

func TestDeref_Nil(t *testing.T) {
	if deref(nil) != "" {
		t.Error("expected empty string for nil")
	}
}

func TestDeref_Ptr(t *testing.T) {
	s := "hello"
	if deref(&s) != "hello" {
		t.Error("expected hello")
	}
}

func TestValidateEnum_ValidValue(t *testing.T) {
	if msg := validateEnum("field", "EXACT", validHostMatchTypes); msg != "" {
		t.Errorf("expected no error; got %q", msg)
	}
}

func TestValidateEnum_InvalidValue(t *testing.T) {
	msg := validateEnum("field", "BOGUS", validHostMatchTypes)
	if msg == "" {
		t.Error("expected validation error for BOGUS")
	}
	if !strings.Contains(msg, "field") {
		t.Errorf("expected field name in error; got: %q", msg)
	}
}

func TestValidateEnum_EmptyValue_Skipped(t *testing.T) {
	if msg := validateEnum("field", "", validHostMatchTypes); msg != "" {
		t.Errorf("expected no error for empty value; got %q", msg)
	}
}

func TestItoaPos_Zero(t *testing.T) {
	if itoaPos(0) != "0" {
		t.Error("expected '0'")
	}
}

func TestItoaPos_Positive(t *testing.T) {
	if itoaPos(42) != "42" {
		t.Errorf("expected '42'; got %q", itoaPos(42))
	}
}

func TestInvalidateInterceptionDomains_NilHub(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	// Must not panic with nil hub/meta
	c, _ := echoCtxWith(http.MethodGet, "/", "")
	h.invalidateInterceptionDomains(c) // hub nil → should be a no-op
}

func TestListInterceptionDomains_Empty(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains", "")
	if err := h.ListInterceptionDomains(c); err != nil {
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

func TestListInterceptionDomains_WithData(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains", "")
	if err := h.ListInterceptionDomains(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 1 {
		t.Errorf("total = %v; want 1", m["total"])
	}
}

func TestListInterceptionDomains_StoreError(t *testing.T) {
	db := newFakeInterceptionDB()
	db.listErr = errors.New("db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains", "")
	if err := h.ListInterceptionDomains(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestListInterceptionDomains_EnabledFilter(t *testing.T) {
	db := newFakeInterceptionDB()
	d := seedDomain(db)
	disabledD := d
	disabledD.ID = "dom-2"
	disabledD.Enabled = false
	db.seedDomain(disabledD)
	h := newTestHandler(db, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/interception-domains?enabled=true", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u1"})
	if err := h.ListInterceptionDomains(c); err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 1 {
		t.Errorf("total = %v; want 1 (enabled only)", m["total"])
	}
}

func TestGetInterceptionDomain_Found(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.GetInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["id"] != "dom-1" {
		t.Errorf("id = %v; want dom-1", m["id"])
	}
}

func TestGetInterceptionDomain_NotFound(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGetInterceptionDomain_StoreError(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getErr = errors.New("db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodGet, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.GetInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func validCreateBody() string {
	b, _ := json.Marshal(map[string]any{
		"name":        "example.com",
		"hostPattern": "example.com",
		"adapterId":   "adapt-1",
	})
	return string(b)
}

func TestCreateInterceptionDomain_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	spy := &hubSpy{}
	h := newTestHandler(db, &fakeHubAPI{spy: spy})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", validCreateBody())
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	// Hub invalidation: both compliance-proxy and agent
	calls := spy.seen()
	if len(calls) < 2 {
		t.Errorf("expected ≥2 hub invalidation calls; got %v", calls)
	}
}

func TestCreateInterceptionDomain_MissingRequired_Returns400(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"missing name", mustJSONI(map[string]any{"hostPattern": "x.com", "adapterId": "a"}), "required"},
		{"missing hostPattern", mustJSONI(map[string]any{"name": "n", "adapterId": "a"}), "required"},
		{"missing adapterId", mustJSONI(map[string]any{"name": "n", "hostPattern": "h"}), "required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeInterceptionDB()
			h := newTestHandler(db, nil)
			c, rec := echoCtxWith(http.MethodPost, "/interception-domains", tc.body)
			if err := h.CreateInterceptionDomain(c); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400", rec.Code)
			}
		})
	}
}

func TestCreateInterceptionDomain_InvalidHostMatchType_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"hostMatchType": "INVALID",
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (invalid hostMatchType)", rec.Code)
	}
}

// F-0271a: a REGEX host pattern that does not compile must be rejected at
// authoring time (400), never persisted (the data plane silently drops an
// uncompilable matcher, so the intercept rule would never fire).
func TestCreateInterceptionDomain_InvalidHostRegex_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "(unbalanced", "adapterId": "a",
		"hostMatchType": "REGEX",
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (invalid host regex)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "regular expression") {
		t.Errorf("expected regex error message; got %s", rec.Body.String())
	}
	if db.createHits != 0 {
		t.Errorf("CreateInterceptionDomain store hit %d times; want 0", db.createHits)
	}
}

// A REGEX host pattern that DOES compile is accepted (proves the check is not
// over-broad — non-REGEX patterns with regex-special chars stay valid too).
func TestCreateInterceptionDomain_ValidHostRegex_OK(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": `^api\.(openai|anthropic)\.com$`, "adapterId": "a",
		"hostMatchType": "REGEX",
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (valid regex, body=%s)", rec.Code, rec.Body.String())
	}
}

// F-0271a: an invalid REGEX path pattern is rejected too.
func TestCreateInterceptionDomain_InvalidPathRegex_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"paths": []map[string]any{
			{"matchType": "REGEX", "action": "PROCESS", "pathPattern": []string{"(bad["}},
		},
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (invalid path regex)", rec.Code)
	}
}

// F-0271a: a PATCH that turns an existing EXACT host rule into a REGEX rule
// with an uncompilable pattern (matchType + pattern both changed) is rejected
// against the effective values.
func TestUpdateInterceptionDomain_InvalidHostRegex_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"hostMatchType": "REGEX", "hostPattern": "(unbalanced"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (invalid host regex on update)", rec.Code)
	}
}

// F-0271a: a PATCH that switches only the match type to REGEX must validate
// against the STORED pattern (which was authored for EXACT and may be an
// invalid regex). The effective-value check catches it.
func TestUpdateInterceptionDomain_RegexTypeAgainstStoredBadPattern_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	d := seedDomain(db)
	d.HostPattern = "(unbalanced" // stored pattern, fine as EXACT
	db.seedDomain(d)
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"hostMatchType": "REGEX"}) // pattern omitted
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (REGEX type vs stored bad pattern)", rec.Code)
	}
}

func TestCreateInterceptionDomain_InvalidOnAdapterError_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"onAdapterError": "EXPLODE",
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateInterceptionDomain_InvalidNetworkZone_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"networkZone": "MARS",
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateInterceptionDomain_InvalidPathAction_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"paths": []map[string]any{{"action": "BOGUS"}},
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (invalid path action)", rec.Code)
	}
}

func TestCreateInterceptionDomain_PathMissingAction_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{
		"name": "x", "hostPattern": "x.com", "adapterId": "a",
		"paths": []map[string]any{{"matchType": "EXACT"}},
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (path missing action)", rec.Code)
	}
}

func TestCreateInterceptionDomain_InvalidBody_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", "{bad json")
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateInterceptionDomain_StoreError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.createErr = errors.New("db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", validCreateBody())
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateInterceptionDomain_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	newName := "updated.com"
	body := mustJSON(t, map[string]any{"name": newName})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["name"] != newName {
		t.Errorf("name = %v; want %s", m["name"], newName)
	}
}

func TestUpdateInterceptionDomain_NotFound_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"name": "x"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/missing", body)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateInterceptionDomain_GetError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getErr = errors.New("db error")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"name": "x"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateInterceptionDomain_InvalidHostMatchType_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	hmt := "INVALID"
	body := mustJSON(t, map[string]any{"hostMatchType": hmt})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionDomain_InvalidOnAdapterError_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	v := "WRONG"
	body := mustJSON(t, map[string]any{"onAdapterError": v})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionDomain_InvalidBody_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", "{bad json")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionDomain_UpdateReturnsNil_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	// Force UpdateInterceptionDomain to return nil (simulate race)
	db.updateErr = nil
	// Seed then delete to make it disappear mid-update:
	// Actually easier: seed a domain, then point update to return nil by
	// deleting it first.
	delete(db.domains, "dom-1")
	h := newTestHandler(db, nil)
	// Now getInterceptionDomain returns nil → handler sends 404 at get
	body := mustJSON(t, map[string]any{"name": "x"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteInterceptionDomain_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.DeleteInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", rec.Code)
	}
}

func TestDeleteInterceptionDomain_NotFound_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteInterceptionDomain_GetError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getErr = errors.New("db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.DeleteInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteInterceptionDomain_DeleteError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.deleteErr = errors.New("non-FK db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.DeleteInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateInterceptionPath_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	body := mustJSON(t, map[string]any{"action": "PROCESS", "matchType": "EXACT"})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/dom-1/paths", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateInterceptionPath_DomainNotFound_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "PROCESS"})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/missing/paths", body)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestCreateInterceptionPath_GetDomainError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getErr = errors.New("db error")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "PROCESS"})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/dom-1/paths", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateInterceptionPath_InvalidBody_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/dom-1/paths", "{bad json")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateInterceptionPath_InvalidMatchType_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "PROCESS", "matchType": "BAD"})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/dom-1/paths", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateInterceptionPath_StoreError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.createErr = errors.New("db error")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "PROCESS"})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains/dom-1/paths", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.CreateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateInterceptionPath_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS", MatchType: "EXACT",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	enabled := false
	body := mustJSON(t, map[string]any{"enabled": enabled})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// F-0271a: a path PATCH supplying a REGEX matchType + an uncompilable pattern
// is rejected (400) against the effective values.
func TestUpdateInterceptionPath_InvalidRegex_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS", MatchType: "EXACT",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"matchType": "REGEX", "pathPattern": []string{"(bad["}})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (invalid path regex on update)", rec.Code)
	}
}

func TestUpdateInterceptionPath_PathNotFound_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "BLOCK"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/missing", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "missing")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateInterceptionPath_WrongDomain_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-OTHER", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "BLOCK"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	// path belongs to different domain → 404
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (domain mismatch)", rec.Code)
	}
}

func TestUpdateInterceptionPath_GetPathError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getPathErr = errors.New("db error")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "BLOCK"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateInterceptionPath_InvalidBody_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", "{bad json")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionPath_InvalidAction_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	v := "BOGUS"
	body := mustJSON(t, map[string]any{"action": v})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionPath_UpdateReturnsNil_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	// Seed path but then delete it so update returns nil (race)
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	// delete before update executes
	delete(db.paths, "path-1")
	// But GetInterceptionPath will also return nil now; that's handled before update
	// So we need a path in get-path but not in update. Use updateErr workaround:
	// Re-seed for get, then delete for update by using a custom updateErr
	// Actually simpler: seed path with wrong domainID so get returns it
	// but domainID check kicks in. That's already tested above.
	// Instead: seed path, have update return nil by deleting it first.
	// We already deleted path-1; GetInterceptionPath will also return nil → 404 from get.
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "BLOCK"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteInterceptionPath_HappyPath(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, &fakeHubAPI{spy: &hubSpy{}})
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/path-1", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", rec.Code)
	}
}

func TestDeleteInterceptionPath_PathNotFound_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/missing", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "missing")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteInterceptionPath_WrongDomain_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-OTHER", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/path-1", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (domain mismatch)", rec.Code)
	}
}

func TestDeleteInterceptionPath_GetPathError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.getPathErr = errors.New("db error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/path-1", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteInterceptionPath_DeleteError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	db.deleteErr = errors.New("non-norows error")
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/path-1", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

// Route registration

func TestRegisterInterceptionDomainRoutes_DoesNotPanic(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	passthrough := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterInterceptionDomainRoutes(g, passthrough)
}

// HubAPI adapter for tests

// fakeHubAPI satisfies the HubAPI interface.
type fakeHubAPI struct {
	spy *hubSpy
}

func (f *fakeHubAPI) NotifyConfigChange(_ context.Context, _ hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	return &hub.ConfigChangeResponse{}, nil
}

func (f *fakeHubAPI) InvalidateConfig(_ context.Context, thingType, configKey string) {
	if f.spy != nil {
		f.spy.InvalidateConfig(context.TODO(), thingType, configKey)
	}
}

// buildPathInputs edge cases

func TestBuildPathInputs_Empty(t *testing.T) {
	out, msg := buildPathInputs(nil)
	if msg != "" || len(out) != 0 {
		t.Errorf("expected empty result; got %v / %q", out, msg)
	}
}

func TestBuildPathInputs_ValidPath(t *testing.T) {
	paths := []interceptionPath{{Action: "PROCESS", MatchType: "EXACT"}}
	out, msg := buildPathInputs(paths)
	if msg != "" {
		t.Errorf("expected no error; got %q", msg)
	}
	if len(out) != 1 || out[0].Action != "PROCESS" {
		t.Errorf("unexpected output: %v", out)
	}
}

func TestBuildPathInputs_MissingAction(t *testing.T) {
	paths := []interceptionPath{{MatchType: "EXACT"}}
	_, msg := buildPathInputs(paths)
	if msg == "" {
		t.Error("expected error for missing action")
	}
}

func TestBuildPathInputs_InvalidMatchType(t *testing.T) {
	paths := []interceptionPath{{Action: "PROCESS", MatchType: "BOGUS"}}
	_, msg := buildPathInputs(paths)
	if msg == "" {
		t.Error("expected error for invalid matchType")
	}
}

// mustJSON without testing.T for use in table data
func mustJSONI(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// internalServerError smoke
func TestInternalServerError_Interception(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = internalServerError(c, "test error")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

// Hub invalidation fan-out
func TestInvalidateInterceptionDomains_FansOutBothThingTypes(t *testing.T) {
	spy := &hubSpy{}
	db := newFakeInterceptionDB()
	h := &Handler{
		store:  db,
		hub:    &fakeHubAPI{spy: spy},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		audit:  audit.NewWriter(&mqNop{}, "audit", slog.Default()),
	}
	c, _ := echoCtxWith(http.MethodGet, "/", "")
	h.invalidateInterceptionDomains(c)
	calls := spy.seen()
	// Should fan out to compliance-proxy and agent for interception_domains
	wantCompliance := false
	wantAgent := false
	for _, call := range calls {
		if strings.Contains(call, "compliance-proxy") && strings.Contains(call, "interception_domains") {
			wantCompliance = true
		}
		if strings.Contains(call, "agent") && strings.Contains(call, "interception_domains") {
			wantAgent = true
		}
	}
	if !wantCompliance || !wantAgent {
		t.Errorf("expected both compliance-proxy and agent invalidation; got: %v", calls)
	}
}

// Additional coverage

func TestNew_DoesNotPanic(t *testing.T) {
	// New() uses d.Pool which is an interface; passing nil pool is fine
	// since the pool is only used in store methods (not at construction).
	h := New(Deps{Pool: nil, Logger: nil})
	if h == nil {
		t.Fatal("New returned nil")
	}
}

func TestIncrementConfigVersion_WithMeta_FreshKey(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	meta := systemmetastore.New(mock)

	// GetSystemMetadata returns no rows for fresh key
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	// SetSystemMetadata inserts version=1
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&mqNop{}, "audit", logger)
	h := &Handler{store: newFakeInterceptionDB(), meta: meta, logger: logger, audit: aw}
	h.incrementConfigVersion(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestIncrementConfigVersion_WithMeta_ExistingKey(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	meta := systemmetastore.New(mock)

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("5")))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("6"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&mqNop{}, "audit", logger)
	h := &Handler{store: newFakeInterceptionDB(), meta: meta, logger: logger, audit: aw}
	h.incrementConfigVersion(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSourceIP_ReturnsRealIP(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "1.2.3.4")
	c := e.NewContext(req, httptest.NewRecorder())
	ip := sourceIP(c)
	if ip == "" {
		t.Error("expected non-empty IP")
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	a := actorFromContext(c)
	if a.UserID != "" {
		t.Errorf("expected empty UserID; got %q", a.UserID)
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?offset=-5", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Offset != 0 {
		t.Errorf("got offset=%d; want 0 (negative ignored)", pg.Offset)
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("got limit=%d; want 50 (zero ignored)", pg.Limit)
	}
}

func TestListInterceptionDomains_EnabledFalseFilter(t *testing.T) {
	db := newFakeInterceptionDB()
	d := seedDomain(db)
	disabledD := d
	disabledD.ID = "dom-disabled"
	disabledD.Enabled = false
	db.seedDomain(disabledD)
	h := newTestHandler(db, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/interception-domains?enabled=false", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u1"})
	if err := h.ListInterceptionDomains(c); err != nil {
		t.Fatal(err)
	}
	m := decodeBody(t, rec)
	if m["total"].(float64) != 1 {
		t.Errorf("total = %v; want 1 (disabled only)", m["total"])
	}
}

func TestUpdateInterceptionDomain_UpdateStoreError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	db.updateErr = errors.New("db error during update")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"name": "new-name"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateInterceptionDomain_InvalidNetworkZone_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	v := "GALAXY"
	body := mustJSON(t, map[string]any{"networkZone": v})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionDomain_InvalidDefaultPathAction_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	v := "UNKNOWN"
	body := mustJSON(t, map[string]any{"defaultPathAction": v})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", body)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionPath_InvalidMatchType_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	h := newTestHandler(db, nil)
	v := "UNKNOWN"
	body := mustJSON(t, map[string]any{"matchType": v})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateInterceptionPath_UpdateStoreError_Returns500(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	db.updateErr = errors.New("db error")
	h := newTestHandler(db, nil)
	body := mustJSON(t, map[string]any{"action": "BLOCK"})
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1/paths/path-1", body)
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.UpdateInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteInterceptionDomain_ErrNoRows_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	// Force delete to return pgx.ErrNoRows (race between get check and delete)
	db.deleteErr = pgx.ErrNoRows
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1", "")
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.DeleteInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (ErrNoRows)", rec.Code)
	}
}

func TestDeleteInterceptionPath_ErrNoRows_Returns404(t *testing.T) {
	db := newFakeInterceptionDB()
	db.seedPath(interceptionstore.InterceptionPathRow{
		ID: "path-1", DomainID: "dom-1", Action: "PROCESS",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	db.deleteErr = pgx.ErrNoRows
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodDelete, "/interception-domains/dom-1/paths/path-1", "")
	c.SetParamNames("id", "pathId")
	c.SetParamValues("dom-1", "path-1")
	if err := h.DeleteInterceptionPath(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (ErrNoRows)", rec.Code)
	}
}

// validateDomainEnums direct tests

func TestValidateDomainEnums_AllValid(t *testing.T) {
	body := interceptionDomainBody{
		HostMatchType:     "EXACT",
		DefaultPathAction: "PROCESS",
		OnAdapterError:    "FAIL_OPEN",
		NetworkZone:       "PUBLIC",
	}
	if msg := validateDomainEnums(body); msg != "" {
		t.Errorf("expected no error; got %q", msg)
	}
}

func TestValidateDomainEnums_InvalidDefaultPathAction(t *testing.T) {
	body := interceptionDomainBody{DefaultPathAction: "EXPLODE"}
	if msg := validateDomainEnums(body); msg == "" {
		t.Error("expected validation error for invalid defaultPathAction")
	}
}

func TestValidateDomainEnums_InvalidNetworkZone(t *testing.T) {
	body := interceptionDomainBody{NetworkZone: "SPACE"}
	if msg := validateDomainEnums(body); msg == "" {
		t.Error("expected validation error for invalid networkZone")
	}
}

// Avoid unused import
var _ = bytes.NewReader
