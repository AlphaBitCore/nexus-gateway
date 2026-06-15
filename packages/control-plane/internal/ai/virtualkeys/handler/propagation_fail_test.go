package virtualkey

// F-0099 regression: security-sensitive virtual-key writes must fail loud
// (HTTP 502) when the Category B invalidation push to Hub fails, instead of
// returning 2xx while the data plane keeps honoring a revoked/expired key.
// Each test asserts: (a) the CP DB write committed (truth preserved), (b) the
// response is 502 with the propagation_error envelope, and (c) NO success audit
// row was written.

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// TestApproveVirtualKey_HubFailure502 — approve commits to the DB but the
// virtual_keys invalidation fails → 502, no success audit.
func TestApproveVirtualKey_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.invalidateErr = errors.New("hub unreachable")
	mock.ExpectExec(`UPDATE "VirtualKey"\s+SET "vkStatus" = 'active'`).
		WithArgs("vk-1", "admin-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys/vk-1/approve", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.ApproveVirtualKey(c); err != nil {
		t.Fatalf("ApproveVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if len(hub.invalidateCalls) != 1 {
		t.Errorf("invalidate attempts=%d; want 1", len(hub.invalidateCalls))
	}
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0 (must not log success on push failure)", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB write did not happen as expected: %v", err)
	}
}

// TestRevokeVirtualKey_HubFailure502 — revoke is the highest-stakes path: a
// dropped invalidation leaves the revoked key authenticating on the gateway.
func TestRevokeVirtualKey_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	hub.invalidateErr = errors.New("hub down")
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RevokeVirtualKey(c); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB revoke did not commit: %v", err)
	}
}

// TestRenewVirtualKey_HubFailure502 — renew commits the new expiry but the
// push fails → 502 so the admin retries rather than believing it took effect.
func TestRenewVirtualKey_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-x", "active", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	hub.invalidateErr = errors.New("hub timeout")
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
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB renew did not commit: %v", err)
	}
}

// TestDeleteVirtualKey_HubNotifyFailure502 — the targeted invalidate-by-hash
// path (notifyVKInvalidate / NotifyConfigChange) must also fail loud: a dropped
// push after delete leaves the deleted key's secret valid in the gateway cache.
func TestDeleteVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub unreachable")
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
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if len(hub.NotifyCalls()) != 1 {
		t.Errorf("notify attempts=%d; want 1", len(hub.NotifyCalls()))
	}
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB delete did not commit: %v", err)
	}
}

func TestUpdateVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub down")
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("u-other"))...))

	body := `{"enabled":false,"allowedModels":["m-2"]}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestRegenerateVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub down")
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "ok", strPtr("u-other"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateVirtualKey(c); err != nil {
		t.Fatalf("RegenerateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0 (key rotated in DB but admin must retry)", aud.count())
	}
}

func TestUpdateUserVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub down")
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`UPDATE "VirtualKey"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "new", strPtr("admin-1"))...))

	body := `{"enabled":false,"allowedModels":["m-x"]}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateUserVirtualKey(c); err != nil {
		t.Fatalf("UpdateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}

func TestDeleteUserVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub down")
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
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB delete did not commit: %v", err)
	}
}

func TestRegenerateUserVirtualKey_HubNotifyFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.notifyErr = errors.New("hub down")
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "mine", strPtr("admin-1"))...))
	mock.ExpectExec(`UPDATE "VirtualKey"`).
		WithArgs("vk-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := makeJSONReq(t, http.MethodPost, "/x", "")
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.RegenerateUserVirtualKey(c); err != nil {
		t.Fatalf("RegenerateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "HUB_PROPAGATION_FAILED", "propagation_error")
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
}
