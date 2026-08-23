package analytics

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// vendorBillReconRow is one provider×day reconciliation row for the read-only
// report. Nullable money/annotation columns are pointers so the UI can tell
// "not yet finalized / fetch failed" (null) apart from a genuine zero.
type vendorBillReconRow struct {
	ProviderID   string  `json:"providerId"`
	ProviderName string  `json:"providerName,omitempty"`
	Day          string  `json:"day"` // YYYY-MM-DD (UTC)
	OurBilledUSD float64 `json:"ourBilledUsd"`
	// OurVendorSpendUSD is the reconciliation basis DiffUSD/DiffPct are computed
	// from: all spend the gateway caused this vendor to charge, internal ops
	// included. OurInternalOpsUSD is that figure's internal-ops subset, so the
	// report can tell estimator error apart from routing overhead. Both are 0 on
	// rows written before the vendor-spend series shipped — such a row's
	// difference was computed on the old basis and is not comparable.
	OurVendorSpendUSD float64  `json:"ourVendorSpendUsd"`
	OurInternalOpsUSD float64  `json:"ourInternalOpsUsd"`
	VendorReportedUSD *float64 `json:"vendorReportedUsd"`
	DiffUSD           *float64 `json:"diffUsd"`
	DiffPct           *float64 `json:"diffPct"`
	ScopeKind         string   `json:"scopeKind"`
	// Coverage drives the UI honesty badge: scoped | org_only | fetch_failed |
	// priority_tier_undercount. Only `scoped` rows are alert-eligible.
	Coverage   string  `json:"coverage"`
	Status     string  `json:"status"` // open | reviewed
	ReviewedBy *string `json:"reviewedBy"`
	Note       *string `json:"note"`
	UpdatedAt  string  `json:"updatedAt"`
}

const vendorBillReconDefaultDays = 30

// vendorBillReconMaxLimit caps a single page. The report is one row per
// provider per day, so even the widest offered window is small; the cap exists
// to stop a hand-written `limit=100000` from turning the endpoint into an
// unbounded scan, not to constrain the UI.
const vendorBillReconMaxLimit = 500

// parsePageParams reads the optional limit/offset pair.
//
// Both are optional and, when absent, the response is every row in the window —
// the exact behaviour this endpoint shipped with, which keeps existing callers
// (and the 1.0 admin-API contract) working unchanged. A caller that does page
// gets a validation error rather than a silently-wrong page: unlike from/to,
// where an unparseable value harmlessly falls back to the default window, a
// misread limit or offset would return the wrong slice of data while looking
// completely normal.
func parsePageParams(c echo.Context) (limit, offset int, err error) {
	if v := c.QueryParam("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit <= 0 {
			return 0, 0, fmt.Errorf("limit must be a positive integer")
		}
		if limit > vendorBillReconMaxLimit {
			limit = vendorBillReconMaxLimit
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
	}
	return limit, offset, nil
}

// VendorBillReconciliation returns provider×day vendor-bill reconciliation rows
// over a window (default trailing 30 days). Read-only; part of the Cost /
// Analytics surface, gated on analytics.read.
//
// Paging is opt-in through limit/offset; `total` always reports how many rows
// the window holds in full, so a pager can size itself without a second call
// and an unpaged caller can tell that it received everything.
func (h *Handler) VendorBillReconciliation(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusInternalServerError, errJSON("database not available", "db_unavailable", ""))
	}
	ctx := c.Request().Context()

	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := to.AddDate(0, 0, -vendorBillReconDefaultDays)
	if v := c.QueryParam("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = t
		}
	}
	if v := c.QueryParam("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = t
		}
	}

	limit, offset, err := parsePageParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "bad_request", ""))
	}

	var total int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM vendor_bill_reconciliation WHERE day >= $1 AND day <= $2
	`, from, to).Scan(&total); err != nil {
		h.logger.Error("vendor bill reconciliation: count failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
	}

	// LIMIT NULL is "no limit" in Postgres, so the unpaged path stays a single
	// query shape rather than a second hand-built statement that could drift
	// from this one.
	var limitArg any
	if limit > 0 {
		limitArg = limit
	}
	rows, err := h.pool.Query(ctx, `
		SELECT r.provider_id,
		       COALESCE(p."displayName", p.name, r.provider_id) AS provider_name,
		       r.day, r.our_billed_usd, r.our_vendor_spend_usd, r.our_internal_ops_usd,
		       r.vendor_reported_usd, r.diff_usd, r.diff_pct,
		       r.scope_kind, r.coverage, r.status, r.reviewed_by, r.note, r.updated_at
		FROM vendor_bill_reconciliation r
		LEFT JOIN "Provider" p ON p.id = r.provider_id
		WHERE r.day >= $1 AND r.day <= $2
		ORDER BY r.day DESC, provider_name ASC
		LIMIT $3 OFFSET $4
	`, from, to, limitArg, offset)
	if err != nil {
		h.logger.Error("vendor bill reconciliation: query failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
	}
	defer rows.Close()

	out := []vendorBillReconRow{}
	for rows.Next() {
		var r vendorBillReconRow
		var day, updatedAt time.Time
		if err := rows.Scan(&r.ProviderID, &r.ProviderName, &day, &r.OurBilledUSD,
			&r.OurVendorSpendUSD, &r.OurInternalOpsUSD,
			&r.VendorReportedUSD, &r.DiffUSD, &r.DiffPct, &r.ScopeKind, &r.Coverage,
			&r.Status, &r.ReviewedBy, &r.Note, &updatedAt); err != nil {
			h.logger.Error("vendor bill reconciliation: scan failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("scan failed", "server_error", ""))
		}
		r.Day = day.Format("2006-01-02")
		r.UpdatedAt = updatedAt.Format(time.RFC3339)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("query failed", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{"rows": out, "total": total})
}

// VendorBillReconciliationReview marks one provider×day row reviewed, stamping
// the acting admin (from the JWT subject) and an optional note. The reconcile
// job preserves this review state across its self-healing re-runs.
func (h *Handler) VendorBillReconciliationReview(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusInternalServerError, errJSON("database not available", "db_unavailable", ""))
	}
	ctx := c.Request().Context()

	providerID := c.Param("providerId")
	day, err := time.Parse("2006-01-02", c.Param("day"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid day (want YYYY-MM-DD)", "bad_request", ""))
	}

	var body struct {
		Note string `json:"note"`
	}
	_ = c.Bind(&body) // note is optional

	// Identity comes from the AdminAuth middleware on the /api/admin group —
	// the same source killswitch, passthrough, and every other admin handler
	// use. It covers both authentication paths (admin_user session and
	// api_key), which a raw JWT-claims read does not: this route's chain never
	// populates the JWT claims key, so the previous jwt.ClaimsFrom lookup was
	// always nil and silently stamped an empty reviewer, losing the one piece
	// of information the review action exists to record.
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return c.JSON(http.StatusUnauthorized, errJSON("no authenticated admin identity", "unauthorized", ""))
	}
	// Prefer the human-readable name; fall back to the id so the audit trail is
	// never blank for a principal that has no name set.
	reviewedBy := aa.KeyName
	if reviewedBy == "" {
		reviewedBy = aa.KeyID
	}
	if reviewedBy == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("authenticated identity carries no name or id", "unauthorized", ""))
	}
	var note any
	if body.Note != "" {
		note = body.Note
	}

	tag, err := h.pool.Exec(ctx, `
		UPDATE vendor_bill_reconciliation
		SET status = 'reviewed', reviewed_by = $1, note = $2, updated_at = now()
		WHERE provider_id = $3 AND day = $4
	`, reviewedBy, note, providerID, day)
	if err != nil {
		h.logger.Error("vendor bill reconciliation review: update failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("update failed", "server_error", ""))
	}
	if tag.RowsAffected() == 0 {
		return c.JSON(http.StatusNotFound, errJSON("reconciliation row not found", "not_found", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"status":     "reviewed",
		"reviewedBy": reviewedBy,
		"providerId": providerID,
		"day":        day.Format("2006-01-02"),
	})
}
