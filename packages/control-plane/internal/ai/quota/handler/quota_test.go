package quota

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

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/quotastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// In-memory fakes

type fakeQuotaDB struct {
	mu        sync.Mutex
	policies  map[string]*quotastore.QuotaPolicy
	overrides map[string]*quotastore.QuotaOverride

	listPoliciesErr   error
	getPolicyErr      error
	createPolicyErr   error
	updatePolicyErr   error
	deletePolicyErr   error
	listOverridesErr  error
	getOverrideErr    error
	getByTargetErr    error
	getByTargetResult *quotastore.QuotaOverride
	createOverrideErr error
	updateOverrideErr error
	deleteOverrideErr error

	// Last-call captures for assertions on params plumbing.
	lastCreatePolicyParams *quotastore.CreateQuotaPolicyParams
	lastUpdatePolicyParams *quotastore.UpdateQuotaPolicyParams
}

func newFakeQuotaDB() *fakeQuotaDB {
	return &fakeQuotaDB{
		policies:  map[string]*quotastore.QuotaPolicy{},
		overrides: map[string]*quotastore.QuotaOverride{},
	}
}

func (f *fakeQuotaDB) ListQuotaPolicies(_ context.Context, _ quotastore.QuotaPolicyListParams) ([]quotastore.QuotaPolicy, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listPoliciesErr != nil {
		return nil, 0, f.listPoliciesErr
	}
	var out []quotastore.QuotaPolicy
	for _, p := range f.policies {
		out = append(out, *p)
	}
	return out, len(out), nil
}

func (f *fakeQuotaDB) GetQuotaPolicy(_ context.Context, id string) (*quotastore.QuotaPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getPolicyErr != nil {
		return nil, f.getPolicyErr
	}
	p := f.policies[id]
	if p == nil {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (f *fakeQuotaDB) CreateQuotaPolicy(_ context.Context, p quotastore.CreateQuotaPolicyParams) (*quotastore.QuotaPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pCopy := p
	f.lastCreatePolicyParams = &pCopy
	if f.createPolicyErr != nil {
		return nil, f.createPolicyErr
	}
	pol := &quotastore.QuotaPolicy{
		ID:              "pol-" + p.Name,
		Name:            p.Name,
		Scope:           p.Scope,
		PeriodType:      p.PeriodType,
		EnforcementMode: p.EnforcementMode,
		Enabled:         p.Enabled,
	}
	f.policies[pol.ID] = pol
	return pol, nil
}

func (f *fakeQuotaDB) UpdateQuotaPolicy(_ context.Context, id string, p quotastore.UpdateQuotaPolicyParams) (*quotastore.QuotaPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pCopy := p
	f.lastUpdatePolicyParams = &pCopy
	if f.updatePolicyErr != nil {
		return nil, f.updatePolicyErr
	}
	pol := f.policies[id]
	if pol == nil {
		return nil, nil
	}
	cp := *pol
	if p.Name != nil {
		cp.Name = *p.Name
	}
	return &cp, nil
}

func (f *fakeQuotaDB) DeleteQuotaPolicy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletePolicyErr != nil {
		return f.deletePolicyErr
	}
	delete(f.policies, id)
	return nil
}

func (f *fakeQuotaDB) ListQuotaOverrides(_ context.Context, _ quotastore.QuotaOverrideListParams) ([]quotastore.QuotaOverride, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listOverridesErr != nil {
		return nil, 0, f.listOverridesErr
	}
	var out []quotastore.QuotaOverride
	for _, o := range f.overrides {
		out = append(out, *o)
	}
	return out, len(out), nil
}

func (f *fakeQuotaDB) GetQuotaOverride(_ context.Context, id string) (*quotastore.QuotaOverride, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getOverrideErr != nil {
		return nil, f.getOverrideErr
	}
	o := f.overrides[id]
	if o == nil {
		return nil, nil
	}
	cp := *o
	return &cp, nil
}

func (f *fakeQuotaDB) GetQuotaOverrideByTarget(_ context.Context, _, _ string) (*quotastore.QuotaOverride, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getByTargetErr != nil {
		return nil, f.getByTargetErr
	}
	return f.getByTargetResult, nil
}

func (f *fakeQuotaDB) CreateQuotaOverride(_ context.Context, p quotastore.CreateQuotaOverrideParams) (*quotastore.QuotaOverride, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createOverrideErr != nil {
		return nil, f.createOverrideErr
	}
	o := &quotastore.QuotaOverride{
		ID:         "ovr-" + p.TargetID,
		TargetType: p.TargetType,
		TargetID:   p.TargetID,
	}
	f.overrides[o.ID] = o
	return o, nil
}

func (f *fakeQuotaDB) UpdateQuotaOverride(_ context.Context, id string, _ quotastore.UpdateQuotaOverrideParams) (*quotastore.QuotaOverride, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateOverrideErr != nil {
		return nil, f.updateOverrideErr
	}
	o := f.overrides[id]
	if o == nil {
		return nil, nil
	}
	cp := *o
	return &cp, nil
}

func (f *fakeQuotaDB) DeleteQuotaOverride(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteOverrideErr != nil {
		return f.deleteOverrideErr
	}
	delete(f.overrides, id)
	return nil
}

// fakeMetricsDB implements metricsDB.
type fakeMetricsDB struct {
	rows []metrics.RollupRow
	err  error
}

func (f *fakeMetricsDB) QueryRollup(_ context.Context, _ metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	return f.rows, f.err
}

// fakeUsersDB implements usersDB.
type fakeUsersDB struct {
	user *userstore.NexusUserSafe
	err  error
}

func (f *fakeUsersDB) GetNexusUserSafe(_ context.Context, _ string) (*userstore.NexusUserSafe, error) {
	return f.user, f.err
}

// fakeOrgsDB implements orgsDB.
type fakeOrgsDB struct {
	org *orgstore.Organization
	err error
}

func (f *fakeOrgsDB) GetOrganization(_ context.Context, _ string) (*orgstore.Organization, error) {
	return f.org, f.err
}

// fakeVKsDB implements vksDB.
type fakeVKsDB struct {
	vk  *vkstore.VirtualKey
	err error
}

func (f *fakeVKsDB) GetVirtualKey(_ context.Context, _ string) (*vkstore.VirtualKey, error) {
	return f.vk, f.err
}

// fakeHubAPI implements HubAPI.
type fakeHubAPI struct {
	mu    sync.Mutex
	calls []string
}

func (h *fakeHubAPI) NotifyConfigChange(_ context.Context, _ hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	return nil, nil
}

func (h *fakeHubAPI) InvalidateConfig(_ context.Context, thingType, configKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, thingType+"/"+configKey)
}

func (h *fakeHubAPI) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.calls))
	copy(cp, h.calls)
	return cp
}

// nopProducer satisfies mq.Producer.
type nopProducer struct{}

func (n *nopProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (n *nopProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (n *nopProducer) Close() error                                        { return nil }

// Test helpers

func newTestHandler(db quotaDB, met metricsDB, hub HubAPI) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&nopProducer{}, "audit", logger)
	if met == nil {
		met = &fakeMetricsDB{}
	}
	return &Handler{
		quota:   db,
		metrics: met,
		users:   &fakeUsersDB{},
		orgs:    &fakeOrgsDB{},
		vks:     &fakeVKsDB{},
		hub:     hub,
		audit:   aw,
		logger:  logger,
	}
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
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u1", KeyName: "Admin"})
	return c, rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return string(b)
}

// samplePolicy returns a valid QuotaPolicy seeded in the fake DB.
func samplePolicy() quotastore.QuotaPolicy {
	return quotastore.QuotaPolicy{
		ID:              "pol-1",
		Name:            "default-policy",
		Scope:           "user",
		PeriodType:      "monthly",
		EnforcementMode: "reject",
		Enabled:         true,
	}
}

// sampleOverride returns a valid QuotaOverride seeded in the fake DB.
func sampleOverride() quotastore.QuotaOverride {
	cost := 100.0
	return quotastore.QuotaOverride{
		ID:           "ovr-1",
		TargetType:   "user",
		TargetID:     "user-abc",
		CostLimitUsd: &cost,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

// Helper functions

func TestErrJSON(t *testing.T) {
	got := errJSON("msg", "bad", "E001")
	inner := got["error"].(map[string]any)
	if inner["message"] != "msg" {
		t.Errorf("message = %v", inner["message"])
	}
}

func TestInternalServerError(t *testing.T) {
	c, rec := echoCtx(http.MethodGet, "/", "")
	if err := internalServerError(c, "fail"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestActorFromContext_WithAuth(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/", "")
	a := actorFromContext(c)
	if a.UserID != "u1" {
		t.Errorf("UserID = %q; want u1", a.UserID)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	a := actorFromContext(c)
	if a.UserID != "" {
		t.Errorf("expected empty UserID, got %q", a.UserID)
	}
}

func TestSourceIP(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = sourceIP(c) // just check it doesn't panic
}

func TestParsePagination(t *testing.T) {
	tests := []struct {
		url        string
		wantLimit  int
		wantOffset int
	}{
		{"/x", 50, 0},
		{"/x?limit=10&offset=5", 10, 5},
		{"/x?limit=2000", 1000, 0}, // capped
		{"/x?limit=0", 50, 0},      // ignored (not positive)
		{"/x?offset=-1", 50, 0},    // ignored (negative)
		{"/x?limit=bad", 50, 0},    // ignored (non-int)
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		e := echo.New()
		c := e.NewContext(req, httptest.NewRecorder())
		pg := parsePagination(c)
		if pg.Limit != tc.wantLimit || pg.Offset != tc.wantOffset {
			t.Errorf("url=%q: limit=%d offset=%d; want %d %d", tc.url, pg.Limit, pg.Offset, tc.wantLimit, tc.wantOffset)
		}
	}
}

func TestValidateScopeCombination(t *testing.T) {
	sp := func(s string) *string { return &s }

	tests := []struct {
		name    string
		scope   string
		orgID   *string
		vkType  *string
		wantErr bool
	}{
		{"org ok", "organization", sp("org-1"), nil, false},
		{"org missing orgId", "organization", nil, nil, true},
		{"org has vkType", "organization", sp("org-1"), sp("personal"), true},
		{"user ok", "user", nil, nil, false},
		{"user has vkType", "user", nil, sp("personal"), true},
		{"project ok", "project", nil, nil, false},
		{"project has orgId", "project", sp("org-1"), nil, true},
		{"project has vkType", "project", nil, sp("personal"), true},
		{"vk ok personal", "vk", nil, sp("personal"), false},
		{"vk ok application", "vk", nil, sp("application"), false},
		{"vk missing vkType", "vk", nil, nil, true},
		{"vk invalid vkType", "vk", nil, sp("invalid"), true},
		{"vk has orgId", "vk", sp("org-1"), sp("personal"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScopeCombination(tc.scope, tc.orgID, tc.vkType)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// New constructor

func TestNew_NilLogger(t *testing.T) {
	h := New(Deps{Pool: nil, Hub: nil, Audit: nil, Logger: nil})
	if h == nil {
		t.Fatal("New returned nil")
	}
}

func TestListQuotaPolicies_Empty(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-policies", "")
	if err := h.ListQuotaPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListQuotaPolicies_WithFilters(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-policies?scope=user&enabled=true&limit=10&offset=0", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.ListQuotaPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestListQuotaPolicies_EnabledFalse(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-policies?enabled=false", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.ListQuotaPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListQuotaPolicies_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.listPoliciesErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-policies", "")
	if err := h.ListQuotaPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetQuotaPolicy_Found(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-policies/pol-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.GetQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestGetQuotaPolicy_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-policies/nope", "")
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.GetQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGetQuotaPolicy_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getPolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-policies/pol-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.GetQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func validCreatePolicyBody(overrides ...func(map[string]any)) string {
	body := map[string]any{
		"name":            "test-policy",
		"scope":           "user",
		"periodType":      "monthly",
		"enforcementMode": "reject",
	}
	for _, fn := range overrides {
		fn(body)
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestCreateQuotaPolicy_Valid(t *testing.T) {
	spy := &fakeHubAPI{}
	h := newTestHandler(newFakeQuotaDB(), nil, spy)
	c, rec := echoCtx(http.MethodPost, "/quota-policies", validCreatePolicyBody())
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on create")
	}
}

func TestCreateQuotaPolicy_MissingName(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"scope": "user", "periodType": "monthly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_MissingScope(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"name": "p", "periodType": "monthly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_InvalidScope(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"name": "p", "scope": "bad", "periodType": "monthly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_MissingPeriodType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"name": "p", "scope": "user"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_InvalidPeriodType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"name": "p", "scope": "user", "periodType": "hourly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_InvalidEnforcementMode(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"name": "p", "scope": "user", "periodType": "monthly", "enforcementMode": "explode"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_InvalidScopeCombination(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	// scope=vk without vkType
	body := mustJSON(t, map[string]any{"name": "p", "scope": "vk", "periodType": "monthly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaPolicy_DefaultEnforcementMode(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	// omit enforcementMode — handler defaults to "reject"
	body := mustJSON(t, map[string]any{"name": "p", "scope": "user", "periodType": "monthly"})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateQuotaPolicy_ExplicitEnabled(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	enabled := false
	body := mustJSON(t, map[string]any{"name": "p", "scope": "user", "periodType": "monthly", "enabled": enabled})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
}

func TestCreateQuotaPolicy_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.createPolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPost, "/quota-policies", validCreatePolicyBody())
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateQuotaPolicy_NoHub(t *testing.T) {
	// nil hub must not panic
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodPost, "/quota-policies", validCreatePolicyBody())
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
}

// TestIsMissingOrJSONNull exercises the helper directly, including the
// whitespace-tolerant JSON-null branch that isn't reachable through the
// standard Echo JSON binder (which strips surrounding whitespace).
func TestIsMissingOrJSONNull(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"nil", nil, true},
		{"empty", json.RawMessage(""), true},
		{"literal-null", json.RawMessage("null"), true},
		{"padded-null", json.RawMessage("  null  "), true},
		{"whitespace-only", json.RawMessage("   \t \n  "), true},
		{"crlf-null", json.RawMessage("\r\nnull\r\n"), true},
		{"empty-array", json.RawMessage("[]"), false},
		{"value-array", json.RawMessage("[80, 90]"), false},
		{"value-string", json.RawMessage("\"null\""), false}, // string "null" is a real value
		{"value-zero", json.RawMessage("0"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingOrJSONNull(tc.raw); got != tc.want {
				t.Errorf("isMissingOrJSONNull(%q) = %v; want %v", string(tc.raw), got, tc.want)
			}
		})
	}
}

// TestCreateQuotaPolicy_DefaultsAlertThresholds verifies the create handler
// fills the schema default [80, 90] when the caller omits alertThresholds —
// QuotaPolicy.alertThresholds is Json NOT NULL and a nil RawMessage would
// trip a 23502 not_null_violation at the database layer.
func TestCreateQuotaPolicy_DefaultsAlertThresholds(t *testing.T) {
	db := newFakeQuotaDB()
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPost, "/quota-policies", validCreatePolicyBody())
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.lastCreatePolicyParams == nil {
		t.Fatal("expected store CreateQuotaPolicy to have been called")
	}
	got := string(db.lastCreatePolicyParams.AlertThresholds)
	if got != "[80, 90]" {
		t.Errorf("AlertThresholds default = %q; want %q", got, "[80, 90]")
	}
}

// TestCreateQuotaPolicy_DefaultsAlertThresholds_ExplicitNull verifies that an
// explicit JSON null is treated identically to an omitted field and replaced
// with the schema default.
func TestCreateQuotaPolicy_DefaultsAlertThresholds_ExplicitNull(t *testing.T) {
	db := newFakeQuotaDB()
	h := newTestHandler(db, nil, nil)
	body := validCreatePolicyBody(func(m map[string]any) { m["alertThresholds"] = nil })
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.lastCreatePolicyParams == nil {
		t.Fatal("expected store CreateQuotaPolicy to have been called")
	}
	got := string(db.lastCreatePolicyParams.AlertThresholds)
	if got != "[80, 90]" {
		t.Errorf("AlertThresholds default (null) = %q; want %q", got, "[80, 90]")
	}
}

// TestCreateQuotaPolicy_PreservesCallerAlertThresholds verifies the handler
// does NOT overwrite a caller-supplied alertThresholds value with the default.
func TestCreateQuotaPolicy_PreservesCallerAlertThresholds(t *testing.T) {
	db := newFakeQuotaDB()
	h := newTestHandler(db, nil, nil)
	caller := []any{50.0, 75.0, 95.0}
	body := validCreatePolicyBody(func(m map[string]any) { m["alertThresholds"] = caller })
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	got := string(db.lastCreatePolicyParams.AlertThresholds)
	// Bind+marshal of []float64 may render as e.g. [50,75,95]; just check
	// it is the caller's payload and not the default.
	if got == "[80, 90]" {
		t.Errorf("AlertThresholds overwritten with default; got %q", got)
	}
	if !strings.Contains(got, "50") || !strings.Contains(got, "95") {
		t.Errorf("AlertThresholds = %q; expected caller-supplied [50,75,95]", got)
	}
}

// TestUpdateQuotaPolicy_AlertThresholdsExplicitNullPreservesExisting verifies
// the update handler converts an explicit JSON null into a nil RawMessage so
// the store's COALESCE($11, "alertThresholds") preserves the existing column
// value rather than overwriting with NULL.
func TestUpdateQuotaPolicy_AlertThresholdsExplicitNullPreservesExisting(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"alertThresholds": nil})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.lastUpdatePolicyParams == nil {
		t.Fatal("expected store UpdateQuotaPolicy to have been called")
	}
	if db.lastUpdatePolicyParams.AlertThresholds != nil {
		t.Errorf("AlertThresholds = %q; want nil (so COALESCE keeps existing value)", string(db.lastUpdatePolicyParams.AlertThresholds))
	}
}

func TestUpdateQuotaPolicy_Valid(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	spy := &fakeHubAPI{}
	h := newTestHandler(db, nil, spy)
	newName := "updated"
	body := mustJSON(t, map[string]any{"name": newName})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on update")
	}
}

func TestUpdateQuotaPolicy_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodPut, "/quota-policies/nope", `{"name":"x"}`)
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateQuotaPolicy_GetError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getPolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", `{"name":"x"}`)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateQuotaPolicy_InvalidScope(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"scope": "bad"})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaPolicy_InvalidPeriodType(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"periodType": "hourly"})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaPolicy_InvalidEnforcementMode(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"enforcementMode": "bad"})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaPolicy_ScopeCombinationError(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	// pol.Scope = "user", now try setting scope=organization without orgId
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"scope": "organization"})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaPolicy_UpdateStoreError(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	db.updatePolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"name": "x"})
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteQuotaPolicy_Valid(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	spy := &fakeHubAPI{}
	h := newTestHandler(db, nil, spy)
	c, rec := echoCtx(http.MethodDelete, "/quota-policies/pol-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.DeleteQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on delete")
	}
}

func TestDeleteQuotaPolicy_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-policies/nope", "")
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.DeleteQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteQuotaPolicy_GetError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getPolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-policies/pol-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.DeleteQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteQuotaPolicy_DeleteError(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	db.deletePolicyErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-policies/pol-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.DeleteQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestListQuotaOverrides_Empty(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-overrides", "")
	if err := h.ListQuotaOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListQuotaOverrides_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.listOverridesErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-overrides", "")
	if err := h.ListQuotaOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetQuotaOverride_Found(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-overrides/ovr-1", "")
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.GetQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestGetQuotaOverride_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-overrides/nope", "")
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.GetQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGetQuotaOverride_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/quota-overrides/ovr-1", "")
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.GetQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func validCreateOverrideBody() string {
	b, _ := json.Marshal(map[string]any{
		"targetType": "user",
		"targetId":   "user-abc",
	})
	return string(b)
}

func TestCreateQuotaOverride_Valid(t *testing.T) {
	spy := &fakeHubAPI{}
	h := newTestHandler(newFakeQuotaDB(), nil, spy)
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", validCreateOverrideBody())
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on create")
	}
}

func TestCreateQuotaOverride_MissingTargetType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"targetId": "u1"})
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", body)
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaOverride_InvalidTargetType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"targetType": "bad", "targetId": "u1"})
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", body)
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaOverride_MissingTargetID(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"targetType": "user"})
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", body)
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaOverride_InvalidEnforcementMode(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	em := "bad"
	body := mustJSON(t, map[string]any{"targetType": "user", "targetId": "u1", "enforcementMode": em})
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", body)
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaOverride_InvalidPeriodType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	body := mustJSON(t, map[string]any{"targetType": "user", "targetId": "u1", "periodType": "hourly"})
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", body)
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateQuotaOverride_ConflictError(t *testing.T) {
	db := newFakeQuotaDB()
	db.createOverrideErr = quotastore.ErrQuotaOverrideConflict
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", validCreateOverrideBody())
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestCreateQuotaOverride_StoreError(t *testing.T) {
	db := newFakeQuotaDB()
	db.createOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", validCreateOverrideBody())
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateQuotaOverride_NoHub(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil) // nil hub
	c, rec := echoCtx(http.MethodPost, "/quota-overrides", validCreateOverrideBody())
	if err := h.CreateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201", rec.Code)
	}
}

func TestUpdateQuotaOverride_Valid(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	spy := &fakeHubAPI{}
	h := newTestHandler(db, nil, spy)
	body := mustJSON(t, map[string]any{"reason": "test reason"})
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/ovr-1", body)
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on update")
	}
}

func TestUpdateQuotaOverride_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/nope", `{"reason":"x"}`)
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateQuotaOverride_GetError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/ovr-1", `{}`)
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateQuotaOverride_InvalidEnforcementMode(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"enforcementMode": "bad"})
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/ovr-1", body)
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaOverride_InvalidPeriodType(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	h := newTestHandler(db, nil, nil)
	body := mustJSON(t, map[string]any{"periodType": "hourly"})
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/ovr-1", body)
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateQuotaOverride_UpdateError(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	db.updateOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodPut, "/quota-overrides/ovr-1", `{}`)
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.UpdateQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteQuotaOverride_Valid(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	spy := &fakeHubAPI{}
	h := newTestHandler(db, nil, spy)
	c, rec := echoCtx(http.MethodDelete, "/quota-overrides/ovr-1", "")
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.DeleteQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", rec.Code)
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on delete")
	}
}

func TestDeleteQuotaOverride_NotFound(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-overrides/nope", "")
	c.SetParamNames("id")
	c.SetParamValues("nope")
	if err := h.DeleteQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteQuotaOverride_GetError(t *testing.T) {
	db := newFakeQuotaDB()
	db.getOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-overrides/ovr-1", "")
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.DeleteQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteQuotaOverride_DeleteError(t *testing.T) {
	db := newFakeQuotaDB()
	ovr := sampleOverride()
	db.overrides[ovr.ID] = &ovr
	db.deleteOverrideErr = errors.New("db error")
	h := newTestHandler(db, nil, nil)
	c, rec := echoCtx(http.MethodDelete, "/quota-overrides/ovr-1", "")
	c.SetParamNames("id")
	c.SetParamValues("ovr-1")
	if err := h.DeleteQuotaOverride(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

// Analytics helpers

func TestParsePeriodKey_Monthly(t *testing.T) {
	start, end, err := parsePeriodKey("2026-04")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start.Month() != time.April || start.Year() != 2026 {
		t.Errorf("start = %v", start)
	}
	if end.Month() != time.May {
		t.Errorf("end = %v", end)
	}
}

func TestParsePeriodKey_Daily(t *testing.T) {
	start, end, err := parsePeriodKey("2026-04-15")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start.Day() != 15 {
		t.Errorf("start.Day = %d", start.Day())
	}
	if end.Day() != 16 {
		t.Errorf("end.Day = %d", end.Day())
	}
}

func TestParsePeriodKey_InvalidFormat(t *testing.T) {
	_, _, err := parsePeriodKey("2026")
	if err == nil {
		t.Fatal("expected error for 4-char key")
	}
}

func TestParsePeriodKey_InvalidMonthly(t *testing.T) {
	_, _, err := parsePeriodKey("2026-99")
	if err == nil {
		t.Fatal("expected error for invalid month")
	}
}

func TestParsePeriodKey_InvalidDaily(t *testing.T) {
	_, _, err := parsePeriodKey("2026-04-99")
	if err == nil {
		t.Fatal("expected error for invalid day")
	}
}

func TestCurrentMonthPeriodKey(t *testing.T) {
	key := currentMonthPeriodKey()
	if len(key) != 7 {
		t.Errorf("key length = %d; want 7 (got %q)", len(key), key)
	}
}

func TestScopeToDimension(t *testing.T) {
	tests := []struct {
		scope string
		dim   string
		ok    bool
	}{
		{"user", "user", true},
		{"vk", "virtual_key", true},
		{"virtual_key", "virtual_key", true},
		{"project", "organization", true},
		{"organization", "organization", true},
		{"unknown", "", false},
	}
	for _, tc := range tests {
		dim, ok := scopeToDimension(tc.scope)
		if ok != tc.ok || (ok && dim != tc.dim) {
			t.Errorf("scopeToDimension(%q) = %q, %v; want %q, %v", tc.scope, dim, ok, tc.dim, tc.ok)
		}
	}
}

func TestAlertLevelFromPercent(t *testing.T) {
	tests := []struct {
		pct   float64
		level string
	}{
		{0, "normal"},
		{69.9, "normal"},
		{70, "warning"},
		{89.9, "warning"},
		{90, "critical"},
		{100, "critical"},
	}
	for _, tc := range tests {
		got := alertLevelFromPercent(tc.pct)
		if got != tc.level {
			t.Errorf("alertLevelFromPercent(%v) = %q; want %q", tc.pct, got, tc.level)
		}
	}
}

func TestQuotaAnalyticsOverview_DefaultScope(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), &fakeMetricsDB{rows: []metrics.RollupRow{
		{DimensionKey: "user=user-1", Value: 10.0},
	}}, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/overview", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsOverview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestQuotaAnalyticsOverview_InvalidScope(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/overview?scope=bad", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsOverview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsOverview_InvalidPeriodKey(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/overview?periodKey=badkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsOverview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsOverview_MetricsError(t *testing.T) {
	met := &fakeMetricsDB{err: errors.New("db error")}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/overview", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsOverview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestQuotaAnalyticsOverview_WithOverrideLimit(t *testing.T) {
	db := newFakeQuotaDB()
	cost := 50.0
	db.getByTargetResult = &quotastore.QuotaOverride{
		ID:           "ovr-1",
		CostLimitUsd: &cost,
	}
	met := &fakeMetricsDB{rows: []metrics.RollupRow{
		{DimensionKey: "user=user-1", Value: 30.0},
	}}
	h := newTestHandler(db, met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/overview?scope=user&periodKey=2026-04", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsOverview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	data := body["data"].([]any)
	if len(data) == 0 {
		t.Error("expected at least one item")
	}
}

func TestQuotaAnalyticsTrend_MissingParams(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/trend", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTrend(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsTrend_InvalidTargetType(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/trend?targetType=bad&targetId=u1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTrend(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsTrend_ValidUser(t *testing.T) {
	met := &fakeMetricsDB{rows: []metrics.RollupRow{
		{DimensionKey: "user=user-1", Value: 5.0},
	}}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/trend?targetType=user&targetId=user-1&periods=3", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTrend(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestQuotaAnalyticsTrend_MetricsError_PartialReturn(t *testing.T) {
	// Even with metrics errors the handler returns 200 with empty-cost points.
	met := &fakeMetricsDB{err: errors.New("db error")}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/trend?targetType=user&targetId=u1&periods=2", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTrend(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestQuotaAnalyticsTop_DefaultParams(t *testing.T) {
	met := &fakeMetricsDB{rows: []metrics.RollupRow{
		{DimensionKey: "user=u1", Value: 20.0},
		{DimensionKey: "user=u2", Value: 5.0},
	}}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/top", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTop(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestQuotaAnalyticsTop_InvalidScope(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/top?scope=bad", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTop(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsTop_InvalidPeriodKey(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/top?periodKey=bad", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTop(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestQuotaAnalyticsTop_MetricsError(t *testing.T) {
	met := &fakeMetricsDB{err: errors.New("db error")}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/top", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTop(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestQuotaAnalyticsTop_LimitCap(t *testing.T) {
	// Generate more than limit items.
	rows := make([]metrics.RollupRow, 5)
	for i := range rows {
		rows[i] = metrics.RollupRow{
			DimensionKey: "user=u" + string(rune('1'+i)),
			Value:        float64(i + 1),
		}
	}
	met := &fakeMetricsDB{rows: rows}
	h := newTestHandler(newFakeQuotaDB(), met, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/quota-analytics/top?limit=2", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.QuotaAnalyticsTop(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	data := body["data"].([]any)
	if len(data) > 2 {
		t.Errorf("len(data) = %d; want ≤ 2", len(data))
	}
}

func TestResolveEntityName_User(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.users = &fakeUsersDB{user: &userstore.NexusUserSafe{DisplayName: "Alice"}}
	got := h.resolveEntityName(context.Background(), "user", "u1")
	if got != "Alice" {
		t.Errorf("got %q; want Alice", got)
	}
}

func TestResolveEntityName_UserEmail(t *testing.T) {
	email := "alice@example.com"
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.users = &fakeUsersDB{user: &userstore.NexusUserSafe{Email: &email}}
	got := h.resolveEntityName(context.Background(), "user", "u1")
	if got != email {
		t.Errorf("got %q; want %q", got, email)
	}
}

func TestResolveEntityName_UserError_FallsBack(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.users = &fakeUsersDB{err: errors.New("not found")}
	got := h.resolveEntityName(context.Background(), "user", "u1")
	if got != "u1" {
		t.Errorf("got %q; want u1", got)
	}
}

func TestResolveEntityName_Org(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.orgs = &fakeOrgsDB{org: &orgstore.Organization{Name: "Acme Corp"}}
	got := h.resolveEntityName(context.Background(), "organization", "org-1")
	if got != "Acme Corp" {
		t.Errorf("got %q; want Acme Corp", got)
	}
}

func TestResolveEntityName_OrgError_FallsBack(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.orgs = &fakeOrgsDB{err: errors.New("not found")}
	got := h.resolveEntityName(context.Background(), "organization", "org-1")
	if got != "org-1" {
		t.Errorf("got %q; want org-1", got)
	}
}

func TestResolveEntityName_Project(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.orgs = &fakeOrgsDB{org: &orgstore.Organization{Name: "Project X"}}
	got := h.resolveEntityName(context.Background(), "project", "proj-1")
	if got != "Project X" {
		t.Errorf("got %q; want Project X", got)
	}
}

func TestResolveEntityName_VK(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.vks = &fakeVKsDB{vk: &vkstore.VirtualKey{Name: "test-key"}}
	got := h.resolveEntityName(context.Background(), "vk", "vk-1")
	if got != "test-key" {
		t.Errorf("got %q; want test-key", got)
	}
}

func TestResolveEntityName_VKError_FallsBack(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.vks = &fakeVKsDB{err: errors.New("not found")}
	got := h.resolveEntityName(context.Background(), "vk", "vk-1")
	if got != "vk-1" {
		t.Errorf("got %q; want vk-1", got)
	}
}

func TestResolveEntityName_VirtualKey(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	h.vks = &fakeVKsDB{vk: &vkstore.VirtualKey{Name: "my-vk"}}
	got := h.resolveEntityName(context.Background(), "virtual_key", "vk-1")
	if got != "my-vk" {
		t.Errorf("got %q; want my-vk", got)
	}
}

func TestResolveEntityName_Unknown(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	got := h.resolveEntityName(context.Background(), "unknown-scope", "id-x")
	if got != "id-x" {
		t.Errorf("got %q; want id-x", got)
	}
}

// Route registration smoke test

func TestRegisterRoutes_Smoke(t *testing.T) {
	h := newTestHandler(newFakeQuotaDB(), nil, nil)
	e := echo.New()
	g := e.Group("/api")
	noop := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterQuotaPolicyRoutes(g, noop)
	h.RegisterQuotaOverrideRoutes(g, noop)
	h.RegisterQuotaAnalyticsRoutes(g, noop)
	if len(e.Routes()) == 0 {
		t.Error("expected at least one route registered")
	}
}

// TestCreateQuotaPolicy_AllNotNullDefaultsFilled is the prophylaxis test for the
// schema-vs-handler default-drift bug class. QuotaPolicy has these NOT NULL
// columns whose Prisma @default the handler must mirror on Create so a sparse
// body never trips 23502 not_null_violation at the database:
//
//   - enforcementMode String   @default("reject")
//   - alertThresholds Json     @default("[80, 90]")
//   - priority        Int      @default(0)
//   - enabled         Boolean  @default(true)
//
// (name / scope / periodType are NOT NULL without @default, so the handler
// instead rejects the request at the validation gate — covered by the
// Missing* tests above.)
//
// The test sends the most minimal legal body and asserts the store call
// received non-nil values for every NOT NULL column above. Adding a new
// NOT NULL @default column to QuotaPolicy without mirroring it in the handler
// would cause this test to fail (or, at minimum, would require an explicit
// addition here — which is the point).
func TestCreateQuotaPolicy_AllNotNullDefaultsFilled(t *testing.T) {
	db := newFakeQuotaDB()
	h := newTestHandler(db, nil, nil)

	// Most minimal body: only name + scope + periodType (the three required
	// fields without @default). Everything else MUST be defaulted by the
	// handler.
	body := mustJSON(t, map[string]any{
		"name":       "minimal",
		"scope":      "user",
		"periodType": "monthly",
	})
	c, rec := echoCtx(http.MethodPost, "/quota-policies", body)
	if err := h.CreateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.lastCreatePolicyParams == nil {
		t.Fatal("expected store CreateQuotaPolicy to have been called")
	}

	p := db.lastCreatePolicyParams

	// enforcementMode must be schema default "reject".
	if p.EnforcementMode != "reject" {
		t.Errorf("EnforcementMode = %q; want %q (schema default)", p.EnforcementMode, "reject")
	}

	// alertThresholds must be schema default [80, 90] — never an empty/nil
	// RawMessage that the store would marshal to SQL NULL.
	if len(p.AlertThresholds) == 0 {
		t.Errorf("AlertThresholds is empty RawMessage; want %q (schema default) — would trip 23502 at DB", "[80, 90]")
	} else if string(p.AlertThresholds) != "[80, 90]" {
		t.Errorf("AlertThresholds = %q; want %q (schema default)", string(p.AlertThresholds), "[80, 90]")
	}

	// priority defaults to 0 — Go zero value matches schema default.
	if p.Priority != 0 {
		t.Errorf("Priority = %d; want 0 (schema default)", p.Priority)
	}

	// enabled must be defaulted to true. The handler explicitly sets it.
	if !p.Enabled {
		t.Errorf("Enabled = false; want true (schema default)")
	}
}

// TestUpdateQuotaPolicy_OmittedFieldsArePreservedViaCOALESCE asserts that an
// empty Update body sends nil values for every nullable field so the store's
// COALESCE keeps the existing row values. Specifically alertThresholds (the
// canonical NOT NULL Json field) must reach the store as nil — not as the
// literal bytes `null` and not as the schema default.
func TestUpdateQuotaPolicy_OmittedFieldsArePreservedViaCOALESCE(t *testing.T) {
	db := newFakeQuotaDB()
	pol := samplePolicy()
	db.policies[pol.ID] = &pol
	h := newTestHandler(db, nil, nil)

	// Empty body — caller is updating nothing.
	c, rec := echoCtx(http.MethodPut, "/quota-policies/pol-1", "{}")
	c.SetParamNames("id")
	c.SetParamValues("pol-1")
	if err := h.UpdateQuotaPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if db.lastUpdatePolicyParams == nil {
		t.Fatal("expected store UpdateQuotaPolicy to have been called")
	}

	p := db.lastUpdatePolicyParams

	// Every pointer field must be nil so COALESCE keeps existing value.
	if p.Name != nil {
		t.Errorf("Name = %v; want nil so COALESCE keeps existing", *p.Name)
	}
	if p.Scope != nil {
		t.Errorf("Scope = %v; want nil so COALESCE keeps existing", *p.Scope)
	}
	if p.PeriodType != nil {
		t.Errorf("PeriodType = %v; want nil so COALESCE keeps existing", *p.PeriodType)
	}
	if p.EnforcementMode != nil {
		t.Errorf("EnforcementMode = %v; want nil so COALESCE keeps existing", *p.EnforcementMode)
	}
	if p.Priority != nil {
		t.Errorf("Priority = %v; want nil so COALESCE keeps existing", *p.Priority)
	}
	if p.Enabled != nil {
		t.Errorf("Enabled = %v; want nil so COALESCE keeps existing", *p.Enabled)
	}
	// AlertThresholds is the canonical NOT NULL field — must be nil RawMessage
	// (not the literal bytes `null` and not the default).
	if p.AlertThresholds != nil {
		t.Errorf("AlertThresholds = %q; want nil so COALESCE keeps existing", string(p.AlertThresholds))
	}
}
