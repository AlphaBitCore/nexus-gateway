package analytics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

var vendorBillReconCols = []string{
	"provider_id", "provider_name", "day", "our_billed_usd", "vendor_reported_usd",
	"diff_usd", "diff_pct", "scope_kind", "coverage", "status", "reviewed_by", "note", "updated_at",
}

func vbDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestVendorBillReconciliation_ReturnsRowsWithNullsPreserved(t *testing.T) {
	mock, h := newMockHandler(t)
	ts := vbDay(2026, 7, 17)
	f := func(v float64) *float64 { return &v }
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vendorBillReconCols).
			AddRow("prov1", "OpenAI", ts, 9.0, f(10.0), f(1.0), f(0.1), "project", "scoped", "open", nil, nil, ts).
			AddRow("prov2", "Anthropic", ts, 5.0, nil, nil, nil, "org", "fetch_failed", "open", nil, nil, ts))

	c, rec := echoCtx(http.MethodGet, "/analytics/vendor-bill-reconciliation")
	if err := h.VendorBillReconciliation(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rows []vendorBillReconRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(resp.Rows))
	}
	if resp.Rows[0].Coverage != "scoped" || resp.Rows[0].VendorReportedUSD == nil || *resp.Rows[0].VendorReportedUSD != 10 {
		t.Fatalf("scoped row wrong: %+v", resp.Rows[0])
	}
	if resp.Rows[0].Day != "2026-07-17" {
		t.Errorf("day format = %q", resp.Rows[0].Day)
	}
	// fetch_failed row must keep vendor/diff as null (not coerced to 0).
	if resp.Rows[1].Coverage != "fetch_failed" || resp.Rows[1].VendorReportedUSD != nil || resp.Rows[1].DiffUSD != nil {
		t.Fatalf("fetch_failed row must have null vendor/diff: %+v", resp.Rows[1])
	}
}

func TestVendorBillReconciliation_NilPool(t *testing.T) {
	h := &Handler{}
	c, rec := echoCtx(http.MethodGet, "/analytics/vendor-bill-reconciliation")
	_ = h.VendorBillReconciliation(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil pool must 500, got %d", rec.Code)
	}
}

func TestVendorBillReconciliation_QueryError(t *testing.T) {
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(echo.ErrInternalServerError)
	c, rec := echoCtx(http.MethodGet, "/analytics/vendor-bill-reconciliation?from=2026-07-01&to=2026-07-18")
	_ = h.VendorBillReconciliation(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("query error must 500, got %d", rec.Code)
	}
}

func TestVendorBillReconciliation_ScanError(t *testing.T) {
	mock, h := newMockHandler(t)
	// our_billed_usd delivered as a non-numeric string → Scan into float64 fails.
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(vendorBillReconCols).
			AddRow("prov1", "OpenAI", vbDay(2026, 7, 17), "not-a-number", nil, nil, nil, "org", "fetch_failed", "open", nil, nil, vbDay(2026, 7, 17)))
	c, rec := echoCtx(http.MethodGet, "/analytics/vendor-bill-reconciliation")
	_ = h.VendorBillReconciliation(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("scan error must 500, got %d", rec.Code)
	}
}

func reviewCtx(t *testing.T, providerID, day, body, subject string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	c.SetParamNames("providerId", "day")
	c.SetParamValues(providerID, day)
	// Identity must be attached the way the real AdminAuth middleware attaches
	// it. An earlier version of this helper set the JWT claims key instead —
	// the same key the handler then read — so handler and test agreed with each
	// other while disagreeing with production, and every review silently
	// recorded an empty reviewer.
	if subject != "" {
		middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: subject + "-id", KeyName: subject})
	}
	return c, rec
}

func TestVendorBillReconciliationReview_SetsReviewedWithActor(t *testing.T) {
	mock, h := newMockHandler(t)
	mock.ExpectExec(`UPDATE vendor_bill_reconciliation`).
		WithArgs("admin-1", "looks fine", "prov1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c, rec := reviewCtx(t, "prov1", "2026-07-17", `{"note":"looks fine"}`, "admin-1")
	if err := h.VendorBillReconciliationReview(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reviewedBy":"admin-1"`) {
		t.Errorf("response missing acting admin: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The review action exists to record WHO accepted a difference. Without an
// authenticated identity there is nothing to record, so the write must be
// refused rather than stamping a blank reviewer (the shipped behaviour until
// 2026-07-21, which produced rows reading "Reviewed by" with no one named).
func TestVendorBillReconciliationReview_NoIdentityIsRejected(t *testing.T) {
	_, h := newMockHandler(t)
	c, rec := reviewCtx(t, "prov1", "2026-07-17", `{}`, "") // no AdminAuth attached
	_ = h.VendorBillReconciliationReview(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing admin identity must 401, got %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"reviewedBy":""`) {
		t.Error("must not report an empty reviewer")
	}
}

// A principal with no display name still has an id; the audit trail must fall
// back to it rather than going blank.
func TestVendorBillReconciliationReview_FallsBackToKeyID(t *testing.T) {
	mock, h := newMockHandler(t)
	mock.ExpectExec(`UPDATE vendor_bill_reconciliation`).
		WithArgs("key-42", nil, "prov1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	e := echo.New()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	c.SetParamNames("providerId", "day")
	c.SetParamValues("prov1", "2026-07-17")
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "key-42"}) // no KeyName

	if err := h.VendorBillReconciliationReview(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"reviewedBy":"key-42"`) {
		t.Errorf("expected the key id as reviewer, got %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestVendorBillReconciliationReview_NotFound(t *testing.T) {
	mock, h := newMockHandler(t)
	mock.ExpectExec(`UPDATE vendor_bill_reconciliation`).
		WithArgs("admin-1", nil, "ghost", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	c, rec := reviewCtx(t, "ghost", "2026-07-17", `{}`, "admin-1")
	_ = h.VendorBillReconciliationReview(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no row must 404, got %d", rec.Code)
	}
}

func TestVendorBillReconciliationReview_BadDay(t *testing.T) {
	_, h := newMockHandler(t)
	c, rec := reviewCtx(t, "prov1", "not-a-date", `{}`, "admin-1")
	_ = h.VendorBillReconciliationReview(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad day must 400, got %d", rec.Code)
	}
}

func TestVendorBillReconciliationReview_NilPool(t *testing.T) {
	h := &Handler{}
	c, rec := reviewCtx(t, "prov1", "2026-07-17", `{}`, "admin-1")
	_ = h.VendorBillReconciliationReview(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil pool must 500, got %d", rec.Code)
	}
}

func TestVendorBillReconciliationReview_UpdateError(t *testing.T) {
	mock, h := newMockHandler(t)
	mock.ExpectExec(`UPDATE vendor_bill_reconciliation`).
		WithArgs("admin-1", nil, "prov1", pgxmock.AnyArg()).
		WillReturnError(echo.ErrInternalServerError)
	c, rec := reviewCtx(t, "prov1", "2026-07-17", `{}`, "admin-1")
	_ = h.VendorBillReconciliationReview(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update error must 500, got %d", rec.Code)
	}
}
