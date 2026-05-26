package virtualkey

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// ListVirtualKeys (admin)

// TestListVirtualKeys_DefaultsAdmin covers the super-admin-style happy path —
// the isSuperAdmin check returns true, so OwnerID is not overridden, and the
// SQL count/list runs with no filters but limit/offset defaults.
func TestListVirtualKeys_DefaultsAdmin(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	// isSuperAdmin lookup
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	// COUNT(*) → 2
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
	// list
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols).
			AddRow(makeVKRow("vk-1", "alpha", nil)...).
			AddRow(makeVKRow("vk-2", "beta", nil)...))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/virtual-keys", "")
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data  []map[string]any `json:"data"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 2 || len(body.Data) != 2 {
		t.Errorf("body=%+v", body)
	}
}

// TestListVirtualKeys_NonAdminScopesToOwner covers the privilege-scoping
// branch: a non-super-admin caller's OwnerID is overridden to their own
// KeyID regardless of any ownerId query param.
func TestListVirtualKeys_NonAdminScopesToOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs("admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("admin-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/virtual-keys?ownerId=someone-else", "")
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestListVirtualKeys_AppliesFilters covers the projectId+ownerId+vkType+
// vkStatus+enabled=true+q filter set. ProjectID query param is honored even
// for super-admins.
func TestListVirtualKeys_AppliesFilters(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs("p-1", true, "u-x", "application", "active", "%search%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("p-1", true, "u-x", "application", "active", "%search%", 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	url := "/api/admin/virtual-keys?projectId=p-1&enabled=true&ownerId=u-x&vkType=application&vkStatus=active&q=search"
	c, rec := makeJSONReq(t, http.MethodGet, url, "")
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestListVirtualKeys_EnabledFalse pins the enabled=false branch (handler
// builds a distinct *bool the way enabled=true does not).
func TestListVirtualKeys_EnabledFalse(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/virtual-keys?enabled=false", "")
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestListVirtualKeys_DBError surfaces the 500 envelope when the count
// query fails.
func TestListVirtualKeys_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WillReturnError(errors.New("conn closed"))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/virtual-keys", "")
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "server_error")
}

// TestListVirtualKeys_NoAuth exercises the aa==nil branch — handlers can be
// invoked without admin auth on routes that opt out of the middleware.
func TestListVirtualKeys_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(r, rec)
	if err := h.ListVirtualKeys(c); err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// GetVirtualKey (admin)

// TestGetVirtualKey_HappySuper covers the super-admin happy path.
func TestGetVirtualKey_HappySuper(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestGetVirtualKey_HappyOwner covers a non-super-admin caller fetching
// their own VK (passes the ownership check).
func TestGetVirtualKey_HappyOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetVirtualKey_NotFound covers the vk==nil → 404 path.
func TestGetVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestGetVirtualKey_DBError surfaces the 500 envelope when resolveVK errors.
func TestGetVirtualKey_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("conn lost"))

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestGetVirtualKey_ForbiddenForNonOwner covers the cross-tenant deny path
// where a non-super-admin tries to look up someone else's VK.
func TestGetVirtualKey_ForbiddenForNonOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "authorization_error")
}

// TestGetVirtualKey_ForbiddenForNilOwnerID covers the OwnerID==nil branch
// of the ownership check.
func TestGetVirtualKey_ForbiddenForNilOwnerID(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", nil)...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodGet, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestGetVirtualKey_NoAuth allows a happy GET when admin auth is absent
// (the aa==nil short-circuit skips ownership checks).
func TestGetVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", nil)...))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.GetVirtualKey(c); err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// CreateVirtualKey (admin)

// TestCreateVirtualKey_HappyPersonal covers the personal-VK (default vkType)
// happy path: vkStatus stays "active", projectId/expiresAt are not required.
func TestCreateVirtualKey_HappyPersonal(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-new", "n", strPtr("admin-1"))...))

	body := `{"name":"n"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
	// Response must carry the raw key once.
	if !strings.Contains(rec.Body.String(), `"key":"nvk_`) {
		t.Errorf("response missing raw key: %s", rec.Body.String())
	}
}

// TestCreateVirtualKey_HappyApplication covers the application-VK happy
// path: projectId + expiresAt required, vkStatus becomes "pending".
func TestCreateVirtualKey_HappyApplication(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-app", "app", strPtr("admin-1"))...))

	future := time.Now().UTC().Add(30 * 24 * time.Hour)
	body := `{"name":"app","vkType":"application","projectId":"p-1","expiresAt":"` + future.Format(time.RFC3339) + `","enabled":false,"allowedModels":["m-1"]}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateVirtualKey_BindError covers the bad-JSON 400.
func TestCreateVirtualKey_BindError(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{nope`)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestCreateVirtualKey_EmptyName covers the missing-name 400.
func TestCreateVirtualKey_EmptyName(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"name":""}`)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestCreateVirtualKey_ApplicationWithoutProject covers the projectId-
// required-for-application 400.
func TestCreateVirtualKey_ApplicationWithoutProject(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"app","vkType":"application"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "projectId is required") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestCreateVirtualKey_ApplicationEmptyProject covers the empty-string
// projectId case (it should be rejected by the same guard).
func TestCreateVirtualKey_ApplicationEmptyProject(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"app","vkType":"application","projectId":""}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestCreateVirtualKey_ApplicationWithoutExpiresAt covers the expiresAt-
// required-for-application 400.
func TestCreateVirtualKey_ApplicationWithoutExpiresAt(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"app","vkType":"application","projectId":"p-1"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expiresAt is required") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestCreateVirtualKey_ApplicationExpiresAtTooFar covers the 3-month ceiling.
func TestCreateVirtualKey_ApplicationExpiresAtTooFar(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	far := time.Now().UTC().AddDate(2, 0, 0)
	body := `{"name":"app","vkType":"application","projectId":"p-1","expiresAt":"` + far.Format(time.RFC3339) + `"}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "must not exceed 3 months") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestCreateVirtualKey_DBError covers the 500 envelope when INSERT fails.
func TestCreateVirtualKey_DBError(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnError(errors.New("constraint violation"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"name":"n"}`)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if aud.count() != 0 {
		t.Errorf("expected no audit on DB failure")
	}
}

// TestCreateVirtualKey_NoAuth exercises the aa==nil branch — ownerID stays
// nil and the INSERT still succeeds.
func TestCreateVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "n", nil)...))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"n","enabled":true,"allowedModels":["m1"]}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(r, rec)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d", rec.Code)
	}
}

// UpdateVirtualKey (admin)

// TestUpdateVirtualKey_NotFound covers the early-return 404 path.
func TestUpdateVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestUpdateVirtualKey_GetError surfaces the 500 envelope when the existing
// lookup itself errors.
func TestUpdateVirtualKey_GetError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("boom"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateVirtualKey_ForbiddenForNonOwner covers the cross-tenant deny
// path.
func TestUpdateVirtualKey_ForbiddenForNonOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "authorization_error")
}

// TestUpdateVirtualKey_HappySuperAdmin covers the full happy path including
// ai-gateway invalidate-by-hash + audit. Super-admin bypasses ownership.
func TestUpdateVirtualKey_HappySuperAdmin(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("u-other"))...))

	body := `{"enabled":false,"sourceApp":"x","rateLimitRpm":100,"allowedModels":["m-2"],"expiresAt":null}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.NotifyCalls()) != 1 {
		t.Errorf("hub notify count = %d; want 1", len(hub.NotifyCalls()))
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

// TestUpdateVirtualKey_HappyOwner covers a non-super-admin caller updating
// their own VK with the standard expiresAt date format (YYYY-MM-DD).
func TestUpdateVirtualKey_HappyOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("admin-1"))...))

	body := `{"enabled":true,"expiresAt":"2026-12-31"}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateVirtualKey_BadExpiresAt covers the bad-date-format 400.
func TestUpdateVirtualKey_BadExpiresAt(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	body := `{"expiresAt":"not-a-date"}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateVirtualKey_BindError covers the bad-JSON 400.
func TestUpdateVirtualKey_BindError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{not-json`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateVirtualKey_BodyReadFailure exercises the io.ReadAll error
// branch by attaching a request body whose Read always errors.
func TestUpdateVirtualKey_BodyReadFailure(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	req := httptest.NewRequest(http.MethodPut, "/x", failingReader{})
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateVirtualKey_DBUpdateFails covers the 500 envelope when UPDATE
// fails.
func TestUpdateVirtualKey_DBUpdateFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).WithArgs(anyN(10)...).WillReturnError(errors.New("update boom"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on UPDATE failure")
	}
}

// TestUpdateVirtualKey_NoAuthSkipsOwnership exercises the aa==nil branch —
// no ownership check, no isSuperAdmin call.
func TestUpdateVirtualKey_NoAuthSkipsOwnership(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("u-other"))...))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("u-other"))...))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"enabled":true}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// DeleteVirtualKey (admin)

// TestDeleteVirtualKey_Happy covers the super-admin happy path with hub
// invalidate + audit.
func TestDeleteVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`DELETE FROM "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.NotifyCalls()) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.NotifyCalls()), aud.count())
	}
}

// TestDeleteVirtualKey_NotFound covers the 404 path.
func TestDeleteVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestDeleteVirtualKey_GetError covers the 500 path when resolveVK fails.
func TestDeleteVirtualKey_GetError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("conn lost"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteVirtualKey_Forbidden covers the cross-tenant deny path.
func TestDeleteVirtualKey_Forbidden(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteVirtualKey_DBDeleteFails covers the 500 envelope when DELETE
// fails (no side effects).
func TestDeleteVirtualKey_DBDeleteFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`DELETE FROM "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnError(errors.New("delete boom"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DELETE failure")
	}
}

// TestDeleteVirtualKey_NoAuth exercises the aa==nil branch — no ownership
// check, no isSuperAdmin call.
func TestDeleteVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectExec(`DELETE FROM "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteVirtualKey(c); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// RegenerateVirtualKey (admin)

// TestRegenerateVirtualKey_HappySuper covers the super-admin happy path:
// produces a new raw key + invalidates the OLD hash on the data plane.
func TestRegenerateVirtualKey_HappySuper(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"key":"nvk_`) {
		t.Errorf("body missing raw key: %s", rec.Body.String())
	}
	if len(hub.NotifyCalls()) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.NotifyCalls()), aud.count())
	}
}

// TestRegenerateVirtualKey_NotFound covers the get-error → 404 path.
func TestRegenerateVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateVirtualKey_Forbidden covers the cross-tenant deny path.
func TestRegenerateVirtualKey_Forbidden(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("viewers"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateVirtualKey_DBUpdateFails covers the 500 envelope when the
// UPDATE itself fails (no audit, no hub).
func TestRegenerateVirtualKey_DBUpdateFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("update boom"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on update failure")
	}
}

// TestRegenerateVirtualKey_NoAuth exercises the aa==nil branch.
func TestRegenerateVirtualKey_NoAuth(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// Keep the audit import alive; used by all subtests via newHandlerWithMockDB.
var _ = audit.Entry{}
