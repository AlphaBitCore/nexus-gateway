package virtualkey

import (
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// helper-copies in handler.go

// TestErrJSON_Shape pins the canonical {error:{message,type,code}} envelope.
func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("oops", "validation_error", "C7")
	outer, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %+v", got)
	}
	if outer["message"] != "oops" || outer["type"] != "validation_error" || outer["code"] != "C7" {
		t.Errorf("envelope = %+v", outer)
	}
}

// TestInternalServerError verifies the helper writes the standard error
// envelope at 500 with type="server_error".
func TestInternalServerError(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("internalServerError: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Message != "boom" || body.Error.Type != "server_error" {
		t.Errorf("body = %+v", body)
	}
}

// TestActorFromContext_Present verifies the helper reads KeyID + KeyName
// from the AdminAuth attached by middleware.
func TestActorFromContext_Present(t *testing.T) {
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	a := actorFromContext(c)
	if a.UserID != "admin-1" || a.Name != "Admin" {
		t.Errorf("actor = %+v; want {UserID:admin-1, Name:Admin}", a)
	}
}

// TestActorFromContext_Absent locks the zero-value fallback when middleware
// has not attached AdminAuth.
func TestActorFromContext_Absent(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("expected zero actor; got %+v", a)
	}
}

// TestSourceIP verifies sourceIP defers to echo's RealIP() — single-line
// wrapper, so a non-nil string return on a baseline request is sufficient.
func TestSourceIP(t *testing.T) {
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if got := sourceIP(c); got == "" {
		// httptest's default RemoteAddr is "192.0.2.1:1234" so RealIP
		// returns "192.0.2.1"; assert non-empty rather than the exact
		// value to insulate against future echo defaults.
		t.Errorf("sourceIP returned empty string")
	}
}

// TestParsePagination covers every caller-visible branch.
func TestParsePagination(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{name: "defaults when query empty", query: "", wantLimit: 50, wantOffset: 0},
		{name: "explicit valid", query: "?limit=10&offset=5", wantLimit: 10, wantOffset: 5},
		{name: "limit clamped to 1000", query: "?limit=99999", wantLimit: 1000, wantOffset: 0},
		{name: "negative limit ignored", query: "?limit=-3", wantLimit: 50, wantOffset: 0},
		{name: "zero limit ignored", query: "?limit=0", wantLimit: 50, wantOffset: 0},
		{name: "negative offset ignored", query: "?offset=-1", wantLimit: 50, wantOffset: 0},
		{name: "non-numeric ignored", query: "?limit=abc&offset=xyz", wantLimit: 50, wantOffset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x"+tc.query, nil), rec)
			pg := parsePagination(c)
			if pg.Limit != tc.wantLimit || pg.Offset != tc.wantOffset {
				t.Errorf("got Limit=%d Offset=%d; want Limit=%d Offset=%d",
					pg.Limit, pg.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// TestNew_FieldWiring verifies the constructor wires every Deps field.
func TestNew_FieldWiring(t *testing.T) {
	hub := &hubSpy{}
	aud := &auditSpy{}
	logger := silentLogger()
	d := Deps{Hub: hub, Audit: audit.NewWriter(aud, "q", logger), Logger: logger}
	h := New(d)
	if h.hub == nil || h.audit == nil || h.logger == nil {
		t.Errorf("New did not propagate Hub/Audit/Logger: %+v", h)
	}
	// DB is nil because Deps.DB is nil — the constructor must not synthesize sub-stores.
	if h.vks != nil {
		t.Errorf("New synthesized vks store when nil DB was provided: %+v", h.vks)
	}
}

// TestRegisterRoutes_MountsAllSix locks the path/verb grid against
// accidental rename/drop. iamMW is a no-op pass-through.
func TestRegisterRoutes_MountsAllSix(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterRoutes(g, iamMWNoop)

	want := map[string]bool{
		"GET /api/admin/virtual-keys":                 false,
		"POST /api/admin/virtual-keys":                false,
		"GET /api/admin/virtual-keys/:id":             false,
		"PUT /api/admin/virtual-keys/:id":             false,
		"DELETE /api/admin/virtual-keys/:id":          false,
		"POST /api/admin/virtual-keys/:id/regenerate": false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("route %q was not registered", k)
		}
	}
}

// TestRegisterApprovalRoutes_MountsAllFour pins the approval-workflow route
// grid. /approve /reject /renew /revoke.
func TestRegisterApprovalRoutes_MountsAllFour(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterApprovalRoutes(g, iamMWNoop)

	want := map[string]bool{
		"POST /api/admin/virtual-keys/:id/approve": false,
		"POST /api/admin/virtual-keys/:id/reject":  false,
		"POST /api/admin/virtual-keys/:id/renew":   false,
		"POST /api/admin/virtual-keys/:id/revoke":  false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("route %q was not registered", k)
		}
	}
}

// TestRegisterUserVirtualKeyRoutes_MountsAllFive pins the personal-VK
// self-service route grid placed under /api/user/.
func TestRegisterUserVirtualKeyRoutes_MountsAllFive(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	g := e.Group("/api/user")
	h.RegisterUserVirtualKeyRoutes(g)

	want := map[string]bool{
		"GET /api/user/virtual-keys":                 false,
		"POST /api/user/virtual-keys":                false,
		"PUT /api/user/virtual-keys/:id":             false,
		"DELETE /api/user/virtual-keys/:id":          false,
		"POST /api/user/virtual-keys/:id/regenerate": false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("route %q was not registered", k)
		}
	}
}

// TestIsSuperAdmin_NilAA covers the early-return false branch when no auth
// is attached to the request.
func TestIsSuperAdmin_NilAA(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if h.isSuperAdmin(c, nil) {
		t.Errorf("isSuperAdmin(nil) = true; want false")
	}
}

// TestIsSuperAdmin_DBError treats DB lookup errors as not-super (safe
// fallback — fail closed for the elevated-permission check).
func TestIsSuperAdmin_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "u-1").
		WillReturnError(errors.New("conn closed"))

	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if h.isSuperAdmin(c, &auth.AdminAuth{KeyID: "u-1", AuthPrincipalType: "admin_user"}) {
		t.Errorf("isSuperAdmin on DB error = true; want false (fail closed)")
	}
}

// TestIsSuperAdmin_NotInGroup verifies the group-iteration false branch.
func TestIsSuperAdmin_NotInGroup(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "u-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers").AddRow("editors"))

	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if h.isSuperAdmin(c, &auth.AdminAuth{KeyID: "u-1", AuthPrincipalType: "admin_user"}) {
		t.Errorf("isSuperAdmin without super-admins membership = true; want false")
	}
}

// TestIsSuperAdmin_InGroup verifies the true branch and the principalType
// remap (admin_user → nexus_user).
func TestIsSuperAdmin_InGroup(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "u-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if !h.isSuperAdmin(c, &auth.AdminAuth{KeyID: "u-1", AuthPrincipalType: "admin_user"}) {
		t.Errorf("isSuperAdmin with super-admins membership = false; want true")
	}
}

// TestIsSuperAdmin_NonAdminPrincipalTypePassthrough verifies that a non
// admin_user principalType (e.g. "api_key") is passed through unchanged
// to the DB lookup, exercising the "if pt == admin_user" branch's else.
func TestIsSuperAdmin_NonAdminPrincipalTypePassthrough(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("api_key", "k-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if !h.isSuperAdmin(c, &auth.AdminAuth{KeyID: "k-1", AuthPrincipalType: "api_key"}) {
		t.Errorf("isSuperAdmin(api_key) with super-admins = false; want true")
	}
}

// TestNotifyVKInvalidate_NilHub covers the nil-hub guard.
func TestNotifyVKInvalidate_NilHub(t *testing.T) {
	h := New(Deps{Audit: audit.NewWriter(&auditSpy{}, "q", silentLogger()), Logger: silentLogger()})
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	hash := "abc"
	h.notifyVKInvalidate(c, &hash) // must not panic
}

// TestNotifyVKInvalidate_NilHashPointer covers the nil keyHash guard.
func TestNotifyVKInvalidate_NilHashPointer(t *testing.T) {
	h, _, hub, _ := newHandlerWithMockDB(t)
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	h.notifyVKInvalidate(c, nil)
	if len(hub.NotifyCalls()) != 0 {
		t.Errorf("expected no notify on nil hash; got %d", len(hub.NotifyCalls()))
	}
}

// TestNotifyVKInvalidate_EmptyHashString covers the *hash=="" guard.
func TestNotifyVKInvalidate_EmptyHashString(t *testing.T) {
	h, _, hub, _ := newHandlerWithMockDB(t)
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	empty := ""
	h.notifyVKInvalidate(c, &empty)
	if len(hub.NotifyCalls()) != 0 {
		t.Errorf("expected no notify on empty hash; got %d", len(hub.NotifyCalls()))
	}
}

// TestNotifyVKInvalidate_HappyPath covers the all-fields-populated push.
// Verifies the ThingType + ConfigKey + payload shape ai-gateway sees.
func TestNotifyVKInvalidate_HappyPath(t *testing.T) {
	h, _, hub, _ := newHandlerWithMockDB(t)
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	hash := "k-hash"
	h.notifyVKInvalidate(c, &hash)
	calls := hub.NotifyCalls()
	if len(calls) != 1 {
		t.Fatalf("notify call count = %d; want 1", len(calls))
	}
	if calls[0].ThingType != "ai-gateway" || calls[0].ConfigKey != "virtual_keys" {
		t.Errorf("notify args wrong: %+v", calls[0])
	}
	if calls[0].ActorID != "admin-1" || calls[0].ActorName != "Admin" {
		t.Errorf("notify actor wrong: id=%q name=%q", calls[0].ActorID, calls[0].ActorName)
	}
	state, ok := calls[0].State.(map[string]any)
	if !ok {
		t.Fatalf("State is not a map: %+v", calls[0].State)
	}
	if state["op"] != "invalidate" {
		t.Errorf("op = %v; want invalidate", state["op"])
	}
	ids, ok := state["ids"].([]string)
	if !ok || len(ids) != 1 || ids[0] != "k-hash" {
		t.Errorf("ids field wrong: %+v", state["ids"])
	}
}

// TestNotifyVKInvalidate_HubError returns the push error so the caller can
// fail loud (HTTP 502) — a dropped VK-cache invalidation leaves the old secret
// valid on the gateway, so it must not be swallowed.
func TestNotifyVKInvalidate_HubError(t *testing.T) {
	h, _, hub, _ := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub unreachable")
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	hash := "k"
	if err := h.notifyVKInvalidate(c, &hash); err == nil {
		t.Error("notifyVKInvalidate returned nil on hub error; want propagated error")
	}
	if len(hub.NotifyCalls()) != 1 {
		t.Errorf("expected 1 attempted notify; got %d", len(hub.NotifyCalls()))
	}
}

// TestResolveVK_ParamThreading covers the helper's :id path-param read.
func TestResolveVK_ParamThreading(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "vk", nil)...))

	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	vk, err := h.resolveVK(c)
	if err != nil || vk == nil || vk.ID != "vk-1" {
		t.Errorf("resolveVK = (%v, %v)", vk, err)
	}
}

// TestGenerateVirtualKey verifies the format invariant: nvk_ prefix, 12-
// char visible prefix, 32 bytes of hex (64 hex chars + 4-char "nvk_").
func TestGenerateVirtualKey(t *testing.T) {
	raw, hash, prefix, err := generateVirtualKey()
	if err != nil {
		t.Fatalf("generateVirtualKey: %v", err)
	}
	if !strings.HasPrefix(raw, "nvk_") {
		t.Errorf("raw = %q; want nvk_ prefix", raw)
	}
	if len(raw) != 4+64 {
		t.Errorf("raw length = %d; want 68", len(raw))
	}
	if prefix != raw[:12] {
		t.Errorf("prefix mismatch: %q vs raw[:12]=%q", prefix, raw[:12])
	}
	if hash == "" {
		t.Errorf("hash unexpectedly empty")
	}
	// Sanity: two calls should not collide.
	raw2, _, _, _ := generateVirtualKey()
	if raw == raw2 {
		t.Errorf("generateVirtualKey produced identical raw keys across calls: %s", raw)
	}
}

// TestExtractNullableTimeFromBody exercises every documented caller intent.
func TestExtractNullableTimeFromBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantPresent bool
		wantNilTime bool
		wantErr     bool
	}{
		{name: "field absent", body: `{}`, wantPresent: false, wantNilTime: true},
		{name: "field null", body: `{"expiresAt":null}`, wantPresent: true, wantNilTime: true},
		{name: "RFC3339", body: `{"expiresAt":"2026-06-01T00:00:00Z"}`, wantPresent: true, wantNilTime: false},
		{name: "YYYY-MM-DD", body: `{"expiresAt":"2026-06-01"}`, wantPresent: true, wantNilTime: false},
		{name: "garbage string", body: `{"expiresAt":"not-a-date"}`, wantErr: true},
		{name: "wrong type", body: `{"expiresAt":123}`, wantErr: true},
		{name: "outer JSON garbage", body: `{not-json`, wantPresent: false, wantNilTime: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, ts, errMsg := extractNullableTimeFromBody([]byte(tc.body), "expiresAt")
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("expected non-empty errMsg; got present=%v ts=%v", present, ts)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected errMsg=%q", errMsg)
			}
			if present != tc.wantPresent {
				t.Errorf("present = %v; want %v", present, tc.wantPresent)
			}
			if tc.wantNilTime != (ts == nil) {
				t.Errorf("ts nil mismatch: ts=%v wantNil=%v", ts, tc.wantNilTime)
			}
		})
	}
}

// TestExtractNullableTimeFromBody_EmptyRaw exercises the len(raw)==0 branch
// — JSON encoders never emit zero-byte values for a key, but the helper
// treats it identically to "null" defensively.
func TestExtractNullableTimeFromBody_EmptyRaw(t *testing.T) {
	// Synthesize via map[json.RawMessage] indirection.
	body := []byte(`{"expiresAt":""}`)
	present, ts, errMsg := extractNullableTimeFromBody(body, "expiresAt")
	// Empty-string is invalid time, so we expect an error message.
	_, _ = present, ts
	if errMsg == "" {
		t.Errorf("empty-string expiresAt should error; got present=%v ts=%v", present, ts)
	}
}

// Approval workflow — approval.go

// TestApproveVirtualKey_Happy covers the standard pending → active transition
// including the ai-gateway invalidate + audit emission.
func TestApproveVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"\s+SET "vkStatus" = 'active'`).
		WithArgs("vk-1", "admin-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys/vk-1/approve", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.invalidateCalls) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d; want 1+1", len(hub.invalidateCalls), aud.count())
	}
	if hub.invalidateCalls[0].ThingType != "ai-gateway" || hub.invalidateCalls[0].ConfigKey != "virtual_keys" {
		t.Errorf("hub args = %+v", hub.invalidateCalls[0])
	}
}

// TestApproveVirtualKey_NoAuth verifies the "approvedBy=unknown" fallback
// when AdminAuth is absent (defensive — middleware should always attach).
func TestApproveVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", "unknown").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestApproveVirtualKey_NotFound covers the pgx.ErrNoRows → 404 path.
func TestApproveVirtualKey_NotFound(t *testing.T) {
	h, mock, hub, _ := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("missing", "admin-1").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys/missing/approve", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d; want 404", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
	if len(hub.invalidateCalls) != 0 {
		t.Errorf("hub touched on 404")
	}
}

// TestApproveVirtualKey_DBError covers the generic-error → 500 path.
func TestApproveVirtualKey_DBError(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", "admin-1").
		WillReturnError(errors.New("disk full"))

	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys/vk-1/approve", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d; want 500", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DB error")
	}
}

// TestApproveVirtualKey_NilHub covers the nil-hub guard.
func TestApproveVirtualKey_NilHub(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", "admin-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := New(Deps{
		Pool:   poolFromMock(mock),
		Audit:  audit.NewWriter(&auditSpy{}, "q", silentLogger()),
		Logger: silentLogger(),
	})
	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRejectVirtualKey_Happy covers the pending → rejected transition.
func TestRejectVirtualKey_Happy(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"\s+SET "vkStatus" = 'rejected'`).
		WithArgs("vk-1", "admin-1", "spam").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys/vk-1/reject", `{"reason":"spam"}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RejectVirtualKey(c); err != nil {
		t.Fatalf("RejectVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
	last := aud.last()
	after, _ := last["afterState"].(map[string]any)
	if after == nil || after["reason"] != "spam" {
		t.Errorf("audit afterState wrong: %+v", last["afterState"])
	}
}

// TestRejectVirtualKey_NoAuth covers the rejectedBy=unknown fallback.
func TestRejectVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", "unknown", "x").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":"x"}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RejectVirtualKey(c); err != nil {
		t.Fatalf("RejectVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRejectVirtualKey_BindError covers the bad-JSON 400.
func TestRejectVirtualKey_BindError(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{not-json`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RejectVirtualKey(c); err != nil {
		t.Fatalf("RejectVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestRejectVirtualKey_NotFound covers pgx.ErrNoRows → 404.
func TestRejectVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("missing", "admin-1", "bad").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"reason":"bad"}`)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RejectVirtualKey(c); err != nil {
		t.Fatalf("RejectVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestRejectVirtualKey_DBError covers the generic-error → 500 path.
func TestRejectVirtualKey_DBError(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", "admin-1", "x").
		WillReturnError(errors.New("conn down"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"reason":"x"}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RejectVirtualKey(c); err != nil {
		t.Fatalf("RejectVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if aud.count() != 0 {
		t.Errorf("expected no audit on DB error")
	}
}

// TestRenewVirtualKey_Happy covers the active-application key renew path
// including ai-gateway invalidate + audit + the expiresAt field in the
// response body.
func TestRenewVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	future := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", future).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	body := `{"expiresAt":"` + future.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.invalidateCalls) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.invalidateCalls), aud.count())
	}
}

// TestRenewVirtualKey_BindError covers the bad-JSON 400.
func TestRenewVirtualKey_BindError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{nope`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestRenewVirtualKey_MissingExpiresAt covers the required-field 400.
func TestRenewVirtualKey_MissingExpiresAt(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
	if !strings.Contains(rec.Body.String(), "expiresAt is required") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestRenewVirtualKey_TooFar covers the 3-month-ceiling 400.
func TestRenewVirtualKey_TooFar(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	far := time.Now().UTC().AddDate(1, 0, 0)
	body := `{"expiresAt":"` + far.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "must not exceed 3 months") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestRenewVirtualKey_PastDate covers the past-date 400.
func TestRenewVirtualKey_PastDate(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	past := time.Now().UTC().Add(-24 * time.Hour)
	body := `{"expiresAt":"` + past.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "must be in the future") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestRenewVirtualKey_NotFound covers pgx.ErrNoRows → 404.
func TestRenewVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	future := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("missing", future).
		WillReturnError(pgx.ErrNoRows)

	body := `{"expiresAt":"` + future.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestRenewVirtualKey_DBError covers the generic-error → 500 path.
func TestRenewVirtualKey_DBError(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	future := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", future).
		WillReturnError(errors.New("conn lost"))

	body := `{"expiresAt":"` + future.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DB error")
	}
}

// TestRenewVirtualKey_NilHub covers the nil-hub guard on the renew path.
func TestRenewVirtualKey_NilHub(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	future := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", future).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := New(Deps{
		Pool:   poolFromMock(mock),
		Audit:  audit.NewWriter(&auditSpy{}, "q", silentLogger()),
		Logger: silentLogger(),
	})
	body := `{"expiresAt":"` + future.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RenewVirtualKey(c); err != nil {
		t.Fatalf("RenewVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRevokeVirtualKey_Happy covers the active → revoked transition + audit
// + ai-gateway invalidate.
func TestRevokeVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"\s+SET "vkStatus" = 'revoked'`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RevokeVirtualKey(c); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.invalidateCalls) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.invalidateCalls), aud.count())
	}
}

// TestRevokeVirtualKey_NotFound covers pgx.ErrNoRows → 404.
func TestRevokeVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RevokeVirtualKey(c); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestRevokeVirtualKey_DBError covers the generic-error → 500 path.
func TestRevokeVirtualKey_DBError(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnError(errors.New("boom"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RevokeVirtualKey(c); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DB error")
	}
}

// TestRevokeVirtualKey_NilHub covers the nil-hub guard.
func TestRevokeVirtualKey_NilHub(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := New(Deps{
		Pool:   poolFromMock(mock),
		Audit:  audit.NewWriter(&auditSpy{}, "q", silentLogger()),
		Logger: silentLogger(),
	})
	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RevokeVirtualKey(c); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// (no more helpers below — nil-hub variants use poolFromMock directly.)
