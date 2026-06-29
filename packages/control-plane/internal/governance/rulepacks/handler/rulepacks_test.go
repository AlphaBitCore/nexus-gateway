package rulepacks

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// In-memory fake store

type fakeStore struct {
	mu       sync.Mutex
	packs    map[string]*rulepack.Pack
	installs map[string]*rulepack.Install

	importHits int

	listErr       error
	getErr        error
	importErr     error
	updateErr     error
	deleteErr     error
	installErr    error
	updateInstErr error
	deleteInstErr error
	upsertOvrErr  error
	loadForErr    error
	upgradeRes    *rulepack.UpgradeResult
	upgradeErr    error
}

func (f *fakeStore) UpgradeInstallToLatest(_ context.Context, installID string) (*rulepack.UpgradeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upgradeErr != nil {
		return nil, f.upgradeErr
	}
	if f.upgradeRes != nil {
		return f.upgradeRes, nil
	}
	if _, ok := f.installs[installID]; !ok {
		return nil, rulepack.ErrInstallNotFound
	}
	return &rulepack.UpgradeResult{InstallID: installID, Upgraded: false}, nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		packs:    map[string]*rulepack.Pack{},
		installs: map[string]*rulepack.Install{},
	}
}

func (f *fakeStore) seedPack(p rulepack.Pack) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.packs[p.ID] = &cp
}

func (f *fakeStore) seedInstall(i rulepack.Install) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := i
	f.installs[i.ID] = &cp
}

func (f *fakeStore) ListPacks(_ context.Context) ([]rulepack.Pack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []rulepack.Pack
	for _, p := range f.packs {
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeStore) GetPack(_ context.Context, id string) (*rulepack.Pack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	p, ok := f.packs[id]
	if !ok {
		return nil, rulepack.ErrPackNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakeStore) ImportPack(_ context.Context, p *rulepack.Pack) (*rulepack.Pack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importHits++
	if f.importErr != nil {
		return nil, f.importErr
	}
	if p.ID == "" {
		p.ID = "pack-new"
	}
	cp := *p
	f.packs[cp.ID] = &cp
	ret := *f.packs[cp.ID]
	return &ret, nil
}

func (f *fakeStore) UpdatePack(_ context.Context, packID string, u rulepack.PackUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	p, ok := f.packs[packID]
	if !ok {
		return rulepack.ErrPackNotFound
	}
	if u.Maintainer != nil {
		p.Maintainer = *u.Maintainer
	}
	if u.Description != nil {
		p.Description = *u.Description
	}
	return nil
}

func (f *fakeStore) DeletePack(_ context.Context, packID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.packs[packID]; !ok {
		return rulepack.ErrPackNotFound
	}
	delete(f.packs, packID)
	return nil
}

func (f *fakeStore) Install(_ context.Context, in rulepack.Install) (*rulepack.Install, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installErr != nil {
		return nil, f.installErr
	}
	if in.ID == "" {
		in.ID = "install-new"
	}
	cp := in
	f.installs[cp.ID] = &cp
	ret := *f.installs[cp.ID]
	return &ret, nil
}

func (f *fakeStore) UpdateInstall(_ context.Context, installID string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateInstErr != nil {
		return f.updateInstErr
	}
	inst, ok := f.installs[installID]
	if !ok {
		return rulepack.ErrInstallNotFound
	}
	inst.Enabled = enabled
	return nil
}

func (f *fakeStore) DeleteInstall(_ context.Context, installID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteInstErr != nil {
		return f.deleteInstErr
	}
	if _, ok := f.installs[installID]; !ok {
		return rulepack.ErrInstallNotFound
	}
	delete(f.installs, installID)
	return nil
}

func (f *fakeStore) ListInstallsForHook(_ context.Context, hookID string) ([]rulepack.Install, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []rulepack.Install
	for _, i := range f.installs {
		if i.BoundHookID == hookID {
			out = append(out, *i)
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertOverrides(_ context.Context, _ string, _ []rulepack.Override) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upsertOvrErr
}

func (f *fakeStore) LoadForInstall(_ context.Context, installID string) (*rulepack.EffectiveRuleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadForErr != nil {
		return nil, f.loadForErr
	}
	inst, ok := f.installs[installID]
	if !ok {
		return nil, errors.New("install not found")
	}
	return &rulepack.EffectiveRuleSet{Install: rulepack.Install{ID: inst.ID}}, nil
}

// hubSpy captures InvalidateConfig calls.
type hubSpy struct {
	mu    sync.Mutex
	calls []string
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

// mqNop satisfies mq.Producer for audit.NewWriter.
type mqNop struct{}

func (m *mqNop) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mqNop) Close() error                                        { return nil }

// Test helpers

func newTestHandler(store RulePackStore, hub HubInvalidator) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&mqNop{}, "audit", logger)
	return New(store, aw, hub)
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

// samplePack is a fully-valid pack: it satisfies the same ValidatePack contract
// the JSON Create handler now enforces (namespaced name, v-prefixed semver,
// severity ∈ {hard,soft,warn}, non-empty category, compilable pattern).
func samplePack() rulepack.Pack {
	return rulepack.Pack{
		ID:         "pack-1",
		Name:       "pii/rules",
		Version:    "v1.0.0",
		Maintainer: "security",
		Rules: []rulepack.Rule{
			{RuleID: "r1", Category: "pii", Pattern: `\bSSN\b`, Severity: "soft"},
		},
	}
}

func TestList_Empty(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs", "")
	if err := h.List(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var packs []any
	json.NewDecoder(rec.Body).Decode(&packs)
	if len(packs) != 0 {
		t.Errorf("expected 0 packs; got %d", len(packs))
	}
}

func TestList_WithPacks(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs", "")
	if err := h.List(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var packs []any
	json.NewDecoder(rec.Body).Decode(&packs)
	if len(packs) != 1 {
		t.Errorf("expected 1 pack; got %d", len(packs))
	}
}

func TestList_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("db error")
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs", "")
	if err := h.List(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGet_Found(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs/pack-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Get(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestGet_NotFound(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.Get(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGet_EmptyID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/rule-packs/", "")
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.Get(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (empty id)", rec.Code)
	}
}

var validYAML = `
name: security/test-pack
version: "v1.0.0"
maintainer: sec
rules:
  - id: r1
    category: pii
    pattern: '\bSSN\b'
    severity: hard
`

func TestPreview_ValidYAML_Returns200(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/preview", strings.NewReader(validYAML))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u"})
	if err := h.Preview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["errors"] == nil {
		t.Error("expected errors key in response")
	}
}

func TestPreview_InvalidYAML_Returns200WithErrors(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/preview", strings.NewReader("not: valid: yaml: pack"))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	// Preview always returns 200
	if err := h.Preview(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (parse errors in body)", rec.Code)
	}
}

func TestImport_ValidYAML_Returns200(t *testing.T) {
	spy := &hubSpy{}
	h := newTestHandler(newFakeStore(), spy)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/import", strings.NewReader(validYAML))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u"})
	if err := h.Import(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// Hub invalidation fired
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation calls on import")
	}
}

func TestImport_InvalidYAML_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/import", strings.NewReader("not: valid: yaml: pack"))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.Import(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestImport_DuplicateVersion_Returns409(t *testing.T) {
	store := newFakeStore()
	store.importErr = rulepack.ErrDuplicatePackVersion
	h := newTestHandler(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/import", strings.NewReader(validYAML))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.Import(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestImport_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.importErr = errors.New("db error")
	h := newTestHandler(store, nil)
	req := httptest.NewRequest(http.MethodPost, "/rule-packs/import", strings.NewReader(validYAML))
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.Import(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreate_ValidPack_Returns201(t *testing.T) {
	spy := &hubSpy{}
	h := newTestHandler(newFakeStore(), spy)
	body := mustJSON(t, samplePack())
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on create")
	}
}

func TestCreate_MissingName_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, rulepack.Pack{Version: "1.0", Maintainer: "sec", Rules: []rulepack.Rule{{RuleID: "r1", Pattern: "x", Severity: "high"}}})
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing name)", rec.Code)
	}
}

func TestCreate_MissingVersion_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, rulepack.Pack{Name: "n", Maintainer: "sec", Rules: []rulepack.Rule{{RuleID: "r1", Pattern: "x", Severity: "high"}}})
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing version)", rec.Code)
	}
}

func TestCreate_MissingMaintainer_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, rulepack.Pack{Name: "n", Version: "1.0", Rules: []rulepack.Rule{{RuleID: "r1", Pattern: "x", Severity: "high"}}})
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing maintainer)", rec.Code)
	}
}

func TestCreate_NoRules_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, rulepack.Pack{Name: "n", Version: "1.0", Maintainer: "sec"})
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (no rules)", rec.Code)
	}
}

func TestCreate_RuleMissingFields_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, rulepack.Pack{
		Name: "n", Version: "1.0", Maintainer: "sec",
		Rules: []rulepack.Rule{{Pattern: "x", Severity: "high"}}, // missing ruleId
	})
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (rule missing fields)", rec.Code)
	}
}

// A JSON-authored pack with an uncompilable regex must be rejected
// with 400 — never silently stored (the evaluator skips uncompilable patterns,
// so a stored "PII block" rule with a typo'd regex never fires). ImportPack
// must NOT be reached.
func TestCreate_InvalidRegex_Returns400_NotStored(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store, nil)
	bad := samplePack()
	bad.Rules[0].Pattern = `(unbalanced` // does not compile
	body := mustJSON(t, bad)
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (invalid regex)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid regex") {
		t.Errorf("expected invalid-regex detail; got %s", rec.Body.String())
	}
	if store.importHits != 0 {
		t.Errorf("ImportPack was called %d times; want 0 (bad pack must not reach the store)", store.importHits)
	}
}

// A bad severity / missing category authored via JSON Create must also
// be rejected with 400 — the YAML Import validator's checks now apply here too.
func TestCreate_BadSeverity_Returns400(t *testing.T) {
	store := newFakeStore()
	h := newTestHandler(store, nil)
	bad := samplePack()
	bad.Rules[0].Severity = "critical" // not in {hard,soft,warn}
	body := mustJSON(t, bad)
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (bad severity)", rec.Code)
	}
	if store.importHits != 0 {
		t.Errorf("ImportPack called %d times; want 0", store.importHits)
	}
}

func TestCreate_DuplicateVersion_Returns409(t *testing.T) {
	store := newFakeStore()
	store.importErr = rulepack.ErrDuplicatePackVersion
	h := newTestHandler(store, nil)
	body := mustJSON(t, samplePack())
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestCreate_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.importErr = errors.New("db error")
	h := newTestHandler(store, nil)
	body := mustJSON(t, samplePack())
	c, rec := echoCtx(http.MethodPost, "/rule-packs", body)
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreate_InvalidBody_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPost, "/rule-packs", "{bad json")
	if err := h.Create(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// Update (PATCH)

func TestUpdate_HappyPath(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	maintainer := "new-team"
	body := mustJSON(t, map[string]any{"maintainer": maintainer})
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/pack-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on update")
	}
}

func TestUpdate_EmptyID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"maintainer": "x"})
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/", body)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (empty id)", rec.Code)
	}
}

func TestUpdate_NoFields_Returns400(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{})
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/pack-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (no fields)", rec.Code)
	}
}

func TestUpdate_PackNotFound_Returns404(t *testing.T) {
	store := newFakeStore()
	store.updateErr = rulepack.ErrPackNotFound
	h := newTestHandler(store, nil)
	store.seedPack(samplePack()) // seed so get works, but update returns ErrPackNotFound
	maintainer := "x"
	body := mustJSON(t, map[string]any{"maintainer": &maintainer})
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/pack-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdate_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	store.updateErr = errors.New("db error")
	h := newTestHandler(store, nil)
	maintainer := "x"
	body := mustJSON(t, map[string]any{"maintainer": &maintainer})
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/pack-1", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdate_InvalidBody_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPatch, "/rule-packs/pack-1", "{bad json")
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Update(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestDelete_HappyPath(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	c, rec := echoCtx(http.MethodDelete, "/rule-packs/pack-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Delete(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on delete")
	}
}

func TestDelete_EmptyID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-packs/", "")
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.Delete(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestDelete_NotFound_Returns404(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-packs/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.Delete(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDelete_ConflictError_Returns409(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	store.deleteErr = errors.New("violates foreign key constraint")
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-packs/pack-1", "")
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.Delete(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestDryRun_HappyPath(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{"content": "User SSN was found"})
	c, rec := echoCtx(http.MethodPost, "/rule-packs/pack-1/dry-run", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.DryRun(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeBody(t, rec)
	if m["matches"] == nil {
		t.Error("expected matches in response")
	}
}

func TestDryRun_EmptyID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"content": "test"})
	c, rec := echoCtx(http.MethodPost, "/rule-packs//dry-run", body)
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.DryRun(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestDryRun_PackNotFound_Returns404(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"content": "test"})
	c, rec := echoCtx(http.MethodPost, "/rule-packs/missing/dry-run", body)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DryRun(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDryRun_EmptyContent_Returns400(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{"content": ""})
	c, rec := echoCtx(http.MethodPost, "/rule-packs/pack-1/dry-run", body)
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.DryRun(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (empty content)", rec.Code)
	}
}

func TestDryRun_InvalidBody_Returns400(t *testing.T) {
	store := newFakeStore()
	store.seedPack(samplePack())
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodPost, "/rule-packs/pack-1/dry-run", "{bad json")
	c.SetParamNames("id")
	c.SetParamValues("pack-1")
	if err := h.DryRun(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestInstall_HappyPath(t *testing.T) {
	spy := &hubSpy{}
	h := newTestHandler(newFakeStore(), spy)
	body := mustJSON(t, map[string]any{"packId": "pack-1", "pinVersion": "1.0.0"})
	c, rec := echoCtx(http.MethodPost, "/hooks/hook-1/rule-packs", body)
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.Install(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on install")
	}
}

func TestInstall_EmptyHookID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"packId": "p1", "pinVersion": "1.0"})
	c, rec := echoCtx(http.MethodPost, "/hooks//rule-packs", body)
	c.SetParamNames("hookId")
	c.SetParamValues("")
	if err := h.Install(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (empty hookId)", rec.Code)
	}
}

func TestInstall_MissingPackID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"pinVersion": "1.0"})
	c, rec := echoCtx(http.MethodPost, "/hooks/hook-1/rule-packs", body)
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.Install(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing packId)", rec.Code)
	}
}

func TestInstall_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.installErr = errors.New("db error")
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{"packId": "p1", "pinVersion": "1.0"})
	c, rec := echoCtx(http.MethodPost, "/hooks/hook-1/rule-packs", body)
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.Install(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestInstall_InvalidBody_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPost, "/hooks/hook-1/rule-packs", "{bad json")
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.Install(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestListInstallsForHook_Empty(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/hooks/hook-1/rule-packs", "")
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.ListInstallsForHook(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListInstallsForHook_WithInstall(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", PackID: "pack-1", BoundHookID: "hook-1", PinVersion: "1.0.0", Enabled: true})
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/hooks/hook-1/rule-packs", "")
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.ListInstallsForHook(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var installs []any
	json.NewDecoder(rec.Body).Decode(&installs)
	if len(installs) != 1 {
		t.Errorf("expected 1 install; got %d", len(installs))
	}
}

func TestListInstallsForHook_EmptyHookID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/hooks//rule-packs", "")
	c.SetParamNames("hookId")
	c.SetParamValues("")
	if err := h.ListInstallsForHook(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestListInstallsForHook_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("db error")
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/hooks/hook-1/rule-packs", "")
	c.SetParamNames("hookId")
	c.SetParamValues("hook-1")
	if err := h.ListInstallsForHook(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestPatchInstall_Enable(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1", Enabled: false})
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	enabled := true
	body := mustJSON(t, map[string]any{"enabled": enabled})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1", body)
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on PatchInstall")
	}
}

func TestPatchInstall_EmptyInstallID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"enabled": true})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/", body)
	c.SetParamNames("installId")
	c.SetParamValues("")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestPatchInstall_MissingEnabled_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1", body)
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (missing enabled)", rec.Code)
	}
}

func TestPatchInstall_NotFound_Returns404(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"enabled": true})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/missing", body)
	c.SetParamNames("installId")
	c.SetParamValues("missing")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestPatchInstall_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1"})
	store.updateInstErr = errors.New("db error")
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{"enabled": true})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1", body)
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestPatchInstall_InvalidBody_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1", "{bad json")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.PatchInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUninstallByID_HappyPath(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1"})
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	c, rec := echoCtx(http.MethodDelete, "/rule-pack-installs/inst-1", "")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UninstallByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on uninstall")
	}
}

func TestUninstallByID_EmptyInstallID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-pack-installs/", "")
	c.SetParamNames("installId")
	c.SetParamValues("")
	if err := h.UninstallByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUninstallByID_NotFound_Returns404(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-pack-installs/missing", "")
	c.SetParamNames("installId")
	c.SetParamValues("missing")
	if err := h.UninstallByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUninstallByID_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1"})
	store.deleteInstErr = errors.New("db error")
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodDelete, "/rule-pack-installs/inst-1", "")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UninstallByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpsertOverrides_HappyPath(t *testing.T) {
	spy := &hubSpy{}
	h := newTestHandler(newFakeStore(), spy)
	body := mustJSON(t, map[string]any{
		"overrides": []map[string]any{{"ruleId": "r1", "enabled": true}},
	})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1/overrides", body)
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UpsertOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation on upsert overrides")
	}
}

func TestUpsertOverrides_EmptyInstallID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	body := mustJSON(t, map[string]any{"overrides": []any{}})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs//overrides", body)
	c.SetParamNames("installId")
	c.SetParamValues("")
	if err := h.UpsertOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpsertOverrides_StoreError_Returns500(t *testing.T) {
	store := newFakeStore()
	store.upsertOvrErr = errors.New("db error")
	h := newTestHandler(store, nil)
	body := mustJSON(t, map[string]any{"overrides": []any{}})
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1/overrides", body)
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UpsertOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpsertOverrides_InvalidBody_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPatch, "/rule-pack-installs/inst-1/overrides", "{bad json")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UpsertOverrides(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestEffectiveRules_Found(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1"})
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/rule-pack-installs/inst-1/effective-rules", "")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.EffectiveRules(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestEffectiveRules_EmptyInstallID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodGet, "/rule-pack-installs//effective-rules", "")
	c.SetParamNames("installId")
	c.SetParamValues("")
	if err := h.EffectiveRules(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (empty installId)", rec.Code)
	}
}

func TestEffectiveRules_NotFound_Returns404(t *testing.T) {
	store := newFakeStore()
	store.loadForErr = errors.New("install not found")
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodGet, "/rule-pack-installs/missing/effective-rules", "")
	c.SetParamNames("installId")
	c.SetParamValues("missing")
	if err := h.EffectiveRules(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

// invalidateHookConfig — hub fan-out

func TestInvalidateHookConfig_NilHub(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	// Must not panic
	h.invalidateHookConfig(context.Background())
}

func TestInvalidateHookConfig_FansOutThreeThingTypes(t *testing.T) {
	spy := &hubSpy{}
	h := newTestHandler(newFakeStore(), spy)
	h.invalidateHookConfig(context.Background())
	calls := spy.seen()
	wantAIGW := false
	wantProxy := false
	wantAgent := false
	for _, c := range calls {
		if strings.Contains(c, "ai-gateway") {
			wantAIGW = true
		}
		if strings.Contains(c, "compliance-proxy") {
			wantProxy = true
		}
		if strings.Contains(c, "agent") {
			wantAgent = true
		}
	}
	if !wantAIGW || !wantProxy || !wantAgent {
		t.Errorf("expected all three thing types; got: %v", calls)
	}
}

func TestRulePackAuditSummary_Nil(t *testing.T) {
	if rulePackAuditSummary(nil) != nil {
		t.Error("expected nil for nil pack")
	}
}

func TestRulePackAuditSummary_Valid(t *testing.T) {
	p := samplePack()
	m := rulePackAuditSummary(&p)
	if m["name"] != "pii/rules" {
		t.Errorf("unexpected summary: %v", m)
	}
	if m["ruleCount"].(int) != 1 {
		t.Errorf("unexpected ruleCount: %v", m["ruleCount"])
	}
}

// emitAudit — nil writer

func TestEmitAudit_NilWriter(t *testing.T) {
	h := New(newFakeStore(), nil, nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	// Must not panic
	h.emitAudit(c, audit.Entry{})
}

func TestRegisterRoutes_DoesNotPanic(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	e := echo.New()
	g := e.Group("/api/admin")
	passthrough := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, passthrough)
}

// Avoid unused bytes import
var _ = bytes.NewReader

func TestUpgradeInstall_Advances_FiresInvalidation(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1", Enabled: true})
	store.upgradeRes = &rulepack.UpgradeResult{
		InstallID: "inst-1", PackName: "nexus/pii",
		FromVersion: "v1.0.0", ToVersion: "v2.0.0", Upgraded: true,
	}
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	c, rec := echoCtx(http.MethodPost, "/rule-pack-installs/inst-1/upgrade", "")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UpgradeInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"upgraded":true`) || !strings.Contains(rec.Body.String(), `"toVersion":"v2.0.0"`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if len(spy.seen()) == 0 {
		t.Error("expected hub invalidation when an upgrade changes the effective rule set")
	}
}

func TestUpgradeInstall_NoOp_SkipsInvalidation(t *testing.T) {
	store := newFakeStore()
	store.seedInstall(rulepack.Install{ID: "inst-1", BoundHookID: "h1", Enabled: true})
	store.upgradeRes = &rulepack.UpgradeResult{InstallID: "inst-1", Upgraded: false}
	spy := &hubSpy{}
	h := newTestHandler(store, spy)
	c, rec := echoCtx(http.MethodPost, "/rule-pack-installs/inst-1/upgrade", "")
	c.SetParamNames("installId")
	c.SetParamValues("inst-1")
	if err := h.UpgradeInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"upgraded":false`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if len(spy.seen()) != 0 {
		t.Error("no-op upgrade must NOT fan out hook invalidation")
	}
}

func TestUpgradeInstall_NotFound_Returns404(t *testing.T) {
	store := newFakeStore()
	store.upgradeErr = rulepack.ErrInstallNotFound
	h := newTestHandler(store, nil)
	c, rec := echoCtx(http.MethodPost, "/rule-pack-installs/missing/upgrade", "")
	c.SetParamNames("installId")
	c.SetParamValues("missing")
	if err := h.UpgradeInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpgradeInstall_EmptyInstallID_Returns400(t *testing.T) {
	h := newTestHandler(newFakeStore(), nil)
	c, rec := echoCtx(http.MethodPost, "/rule-pack-installs//upgrade", "")
	c.SetParamNames("installId")
	c.SetParamValues("")
	if err := h.UpgradeInstall(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}
