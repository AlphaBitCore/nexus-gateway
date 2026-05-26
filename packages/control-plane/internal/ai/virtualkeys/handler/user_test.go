package virtualkey

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// TestCurrentUserID_Present verifies the helper reads KeyID from AdminAuth.
func TestCurrentUserID_Present(t *testing.T) {
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	if got := currentUserID(c); got != "admin-1" {
		t.Errorf("currentUserID = %q; want admin-1", got)
	}
}

// TestCurrentUserID_Absent locks the "" fallback when AdminAuth is not
// attached.
func TestCurrentUserID_Absent(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	if got := currentUserID(c); got != "" {
		t.Errorf("currentUserID without auth = %q; want empty", got)
	}
}

// ListUserVirtualKeys (personal)

// TestListUserVirtualKeys_Unauthenticated covers the no-auth → 401 path.
func TestListUserVirtualKeys_Unauthenticated(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	if err := h.ListUserVirtualKeys(c); err != nil {
		t.Fatalf("ListUserVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d; want 401", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "authentication_error")
}

// TestListUserVirtualKeys_Happy covers the default happy path: scopes to
// caller's ownerID, vkType=personal, no q.
func TestListUserVirtualKeys_Happy(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs("admin-1", "personal").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("admin-1", "personal", 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "p", strPtr("admin-1"))...))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/user/virtual-keys", "")
	if err := h.ListUserVirtualKeys(c); err != nil {
		t.Fatalf("ListUserVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestListUserVirtualKeys_EnabledTrue + EnabledFalse cover both filter
// pointer branches.
func TestListUserVirtualKeys_EnabledTrue(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	// Store arg order: projectId, enabled, ownerId, vkType, vkStatus, q.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs(true, "admin-1", "personal").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(true, "admin-1", "personal", 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/user/virtual-keys?enabled=true", "")
	if err := h.ListUserVirtualKeys(c); err != nil {
		t.Fatalf("ListUserVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestListUserVirtualKeys_EnabledFalse(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	// Store arg order: projectId, enabled, ownerId, vkType, vkStatus, q.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs(false, "admin-1", "personal").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(false, "admin-1", "personal", 50, 0).
		WillReturnRows(pgxmock.NewRows(vkCols))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/user/virtual-keys?enabled=false", "")
	if err := h.ListUserVirtualKeys(c); err != nil {
		t.Fatalf("ListUserVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestListUserVirtualKeys_DBError covers the 500 envelope when count fails.
func TestListUserVirtualKeys_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "VirtualKey"`).
		WithArgs("admin-1", "personal").
		WillReturnError(errors.New("conn lost"))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/user/virtual-keys", "")
	if err := h.ListUserVirtualKeys(c); err != nil {
		t.Fatalf("ListUserVirtualKeys: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "server_error")
}

// CreateUserVirtualKey (personal)

// TestCreateUserVirtualKey_Unauthenticated covers the no-auth → 401 path.
func TestCreateUserVirtualKey_Unauthenticated(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"n"}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(r, rec)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestCreateUserVirtualKey_BindError covers the bad-JSON 400.
func TestCreateUserVirtualKey_BindError(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{nope`)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestCreateUserVirtualKey_EmptyName covers the missing-name 400.
func TestCreateUserVirtualKey_EmptyName(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"name":""}`)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestCreateUserVirtualKey_Happy covers the standard happy path: explicit
// enabled, allowedModels + the returned response containing the raw key.
func TestCreateUserVirtualKey_Happy(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-new", "mine", strPtr("admin-1"))...))

	body := `{"name":"mine","sourceApp":"cli","enabled":true,"rateLimitRpm":50,"allowedModels":["m-x"]}`
	c, rec := makeJSONReq(t, http.MethodPost, "/x", body)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"key":"nvk_`) {
		t.Errorf("body missing raw key: %s", rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

// TestCreateUserVirtualKey_DefaultsEnabledTrue covers the enabled-default
// branch (body omits the field → handler defaults to true).
func TestCreateUserVirtualKey_DefaultsEnabledTrue(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-new", "mine", strPtr("admin-1"))...))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"name":"mine"}`)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestCreateUserVirtualKey_DBError covers the 500 envelope when INSERT
// fails.
func TestCreateUserVirtualKey_DBError(t *testing.T) {
	h, mock, _, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "VirtualKey"`).
		WithArgs(anyN(13)...).
		WillReturnError(errors.New("constraint violation"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", `{"name":"n"}`)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if aud.count() != 0 {
		t.Errorf("expected no audit on DB failure")
	}
}

// UpdateUserVirtualKey (personal)

// TestUpdateUserVirtualKey_Unauthenticated covers the no-auth → 401 path.
func TestUpdateUserVirtualKey_Unauthenticated(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"enabled":true}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateUserVirtualKey_GetError covers the 500 path when resolveVK
// fails — note: user.go uses h.db.GetVirtualKey directly, and treats ANY
// error (including pgx.ErrNoRows mapped to nil) the same: it returns nil,
// nil → 404 from the "existing == nil" branch.
func TestUpdateUserVirtualKey_GetError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	// Force a non-ErrNoRows error so the err!=nil branch fires.
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("conn down"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateUserVirtualKey_NotFound covers the existing==nil 404 path
// (pgx.ErrNoRows → store returns (nil, nil)).
func TestUpdateUserVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestUpdateUserVirtualKey_Forbidden covers the OwnerID-mismatch 403 path.
func TestUpdateUserVirtualKey_Forbidden(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateUserVirtualKey_ForbiddenForNilOwner covers the OwnerID==nil
// branch of the ownership check.
func TestUpdateUserVirtualKey_ForbiddenForNilOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", nil)...))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateUserVirtualKey_BindError covers the bad-JSON 400.
func TestUpdateUserVirtualKey_BindError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("admin-1"))...))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{not-json`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestUpdateUserVirtualKey_Happy covers the full happy path including
// notifyVKInvalidate by-hash + audit + response shape.
func TestUpdateUserVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("admin-1"))...))

	body := `{"enabled":false,"rateLimitRpm":80,"allowedModels":["m-x","m-y"]}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.NotifyCalls()) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.NotifyCalls()), aud.count())
	}
}

// TestUpdateUserVirtualKey_DBUpdateFails covers the 500 envelope when the
// UPDATE itself fails.
func TestUpdateUserVirtualKey_DBUpdateFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).WithArgs(anyN(10)...).WillReturnError(errors.New("update boom"))

	c, rec := makeJSONReq(t, http.MethodPut, "/x", `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on UPDATE failure")
	}
}

// DeleteUserVirtualKey (personal)

// TestDeleteUserVirtualKey_Unauthenticated covers the no-auth → 401 path.
func TestDeleteUserVirtualKey_Unauthenticated(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteUserVirtualKey_GetError covers the 500 path when resolveVK fails.
func TestDeleteUserVirtualKey_GetError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("conn down"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteUserVirtualKey_NotFound covers the existing==nil 404 path.
func TestDeleteUserVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteUserVirtualKey_Forbidden covers the OwnerID-mismatch 403 path.
func TestDeleteUserVirtualKey_Forbidden(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteUserVirtualKey_ForbiddenForNilOwner covers the OwnerID==nil
// branch.
func TestDeleteUserVirtualKey_ForbiddenForNilOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", nil)...))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestDeleteUserVirtualKey_Happy covers the standard happy path: hub
// notify + audit + 200 response with "deleted":true.
func TestDeleteUserVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "mine", strPtr("admin-1"))...))
	mock.ExpectExec(`DELETE FROM "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Errorf("body missing deleted flag: %s", rec.Body.String())
	}
	if len(hub.NotifyCalls()) != 1 || aud.count() != 1 {
		t.Errorf("hub=%d audit=%d", len(hub.NotifyCalls()), aud.count())
	}
}

// TestDeleteUserVirtualKey_DBDeleteFails covers the 500 envelope when
// DELETE fails (no side effects).
func TestDeleteUserVirtualKey_DBDeleteFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "mine", strPtr("admin-1"))...))
	mock.ExpectExec(`DELETE FROM "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnError(errors.New("delete boom"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.DeleteUserVirtualKey(c); err != nil {
		t.Fatalf("DeleteUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DELETE failure")
	}
}

// RegenerateUserVirtualKey (personal)

// TestRegenerateUserVirtualKey_Unauthenticated covers the no-auth → 401.
func TestRegenerateUserVirtualKey_Unauthenticated(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	c := e.NewContext(r, rec)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateUserVirtualKey_GetErrorAs404 covers the err!=nil OR
// vk==nil → 404 branch in the handler. The handler collapses both cases
// to 404 because it OR's the conditions.
func TestRegenerateUserVirtualKey_GetErrorAs404(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnError(errors.New("conn down"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateUserVirtualKey_NotFound covers the vk==nil 404 path.
func TestRegenerateUserVirtualKey_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateUserVirtualKey_Forbidden covers the OwnerID-mismatch 403.
func TestRegenerateUserVirtualKey_Forbidden(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateUserVirtualKey_ForbiddenForNilOwner covers the OwnerID==nil
// branch.
func TestRegenerateUserVirtualKey_ForbiddenForNilOwner(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", nil)...))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestRegenerateUserVirtualKey_Happy covers the standard happy path: a
// fresh raw key, hub invalidate-by-OLD-hash + audit.
func TestRegenerateUserVirtualKey_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "mine", strPtr("admin-1"))...))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
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

// TestRegenerateUserVirtualKey_DBUpdateFails covers the 500 envelope when
// RegenerateVirtualKeyHash itself fails.
func TestRegenerateUserVirtualKey_DBUpdateFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "mine", strPtr("admin-1"))...))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("update boom"))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(hub.NotifyCalls()) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on update failure")
	}
}

// keep time-import-of-other-files alive (used by Update-Happy / Renew tests
// elsewhere via shared cols; harmless when the user_test.go alone is built).
var _ = time.Now
