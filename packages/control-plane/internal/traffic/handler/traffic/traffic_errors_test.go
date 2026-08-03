package traffic

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	pgxmock "github.com/pashagolub/pgxmock/v4"
)

// classifyTrafficErrorAttribution is the triage taxonomy the whole
// error-governance view hangs off — pin every vocabulary member to its
// bucket so a silently re-bucketed code fails a named test, not a UI review.
func TestClassifyTrafficErrorAttribution(t *testing.T) {
	cases := []struct {
		code, statusRange, want string
	}{
		// ours: routing / translation / provider-config failures.
		{"ROUTING_NO_MATCH", "4xx", "ours"},
		{"USAGE_QUERY_FAILED", "5xx", "ours"},
		{"no_compatible_provider", "4xx", "ours"},
		{"not_implemented", "4xx", "ours"},
		{"endpoint_unsupported", "4xx", "ours"},
		{"context_overflow", "4xx", "ours"},
		// auth_failed = upstream rejected OUR configured provider credential.
		{"auth_failed", "4xx", "ours"},
		// client: caller-side failures.
		{"AUTH_INVALID", "4xx", "client"},
		{"AUTH_KEY_EXPIRED", "4xx", "client"},
		{"RATE_LIMITED", "4xx", "client"},
		{"QUOTA_EXCEEDED", "4xx", "client"},
		{"CLIENT_CLOSED", "4xx", "client"},
		{"invalid_request", "4xx", "client"},
		// BUMP_FAILED = compliance-proxy TLS-interception failure (infra).
		{"BUMP_FAILED", "5xx", "ours"},
		// POLICY_DENIED = compliance block doing its job (caller-side).
		{"POLICY_DENIED", "4xx", "client"},
		// upstream: provider-side failures.
		{"PROVIDER_UNAVAILABLE", "5xx", "upstream"},
		{"PROVIDER_RATE_LIMITED", "4xx", "upstream"},
		{"PROVIDER_ERROR", "4xx", "upstream"},
		// unclassified split by status class.
		{"", "5xx", "upstream"},
		{"", "4xx", "client"},
		// unknown future codes fall through the status split, never panic.
		{"SOME_FUTURE_CODE", "5xx", "upstream"},
		{"SOME_FUTURE_CODE", "4xx", "client"},
		// the overflow pseudo-class is unattributable — always "mixed",
		// never misfiled into the status-range fallback.
		{"__overflow__", "4xx", "mixed"},
		{"__overflow__", "5xx", "mixed"},
	}
	for _, c := range cases {
		if got := classifyTrafficErrorAttribution(c.code, c.statusRange); got != c.want {
			t.Errorf("classify(%q, %s) = %s, want %s", c.code, c.statusRange, got, c.want)
		}
	}
}

func TestErrorGroupsBucketInterval(t *testing.T) {
	base := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if got := errorGroupsBucketInterval(base, base.Add(24*time.Hour)); got != 5*time.Minute {
		t.Errorf("24h window interval = %v, want 5m", got)
	}
	if got := errorGroupsBucketInterval(base, base.Add(48*time.Hour)); got != 5*time.Minute {
		t.Errorf("48h window interval = %v, want 5m", got)
	}
	if got := errorGroupsBucketInterval(base, base.Add(7*24*time.Hour)); got != time.Hour {
		t.Errorf("7d window interval = %v, want 1h", got)
	}
	// Daily bins cap the bucket cardinality for arbitrary API windows
	// (from/to are unrestricted inputs — a 1-year window must not stream
	// thousands of bins per group).
	if got := errorGroupsBucketInterval(base, base.Add(30*24*time.Hour)); got != 24*time.Hour {
		t.Errorf("30d window interval = %v, want 24h", got)
	}
}

var trafficErrGroupCols = []string{"error_code", "status_range", "provider", "model",
	"sample_reason", "cnt", "affected_end_users", "first_seen", "last_seen"}

func errAnyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func TestListTrafficErrorGroups_Handler_HappyAndAttributionFilter(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	now := time.Now().UTC()
	rows := func() *pgxmock.Rows {
		return pgxmock.NewRows(trafficErrGroupCols).
			AddRow("context_overflow", "4xx", "openai", "gpt-5.6", "too large", 7, 2, now, now).
			AddRow("AUTH_INVALID", "4xx", "", "", "bad key", 4, 0, now, now)
	}
	buckets := pgxmock.NewRows([]string{"error_code", "status_range", "provider", "model", "bucket_ts", "cnt"})

	// The default (auto) mode consults the rollup-5m watermark first; an
	// absent watermark falls back to the direct scan these arms exercise.
	expectNoRollupWatermark := func() {
		mock.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).
			WithArgs("rollup-5m").
			WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	}

	// Arm 1: no attribution filter — both groups return, each stamped.
	expectNoRollupWatermark()
	mock.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(errAnyArgs(3)...).WillReturnRows(rows())
	mock.ExpectQuery(`date_bin`).WithArgs(errAnyArgs(8)...).WillReturnRows(buckets)

	c, rec := echoCtxQ(http.MethodGet, "/traffic/errors/groups",
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z&source=vk")
	if err := h.ListTrafficErrorGroups(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ErrorCode   string `json:"errorCode"`
			Attribution string `json:"attribution"`
		} `json:"data"`
		BucketSeconds int    `json:"bucketSeconds"`
		DataSource    string `json:"dataSource"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("groups = %d, want 2", len(out.Data))
	}
	if out.Data[0].Attribution != "ours" || out.Data[1].Attribution != "client" {
		t.Errorf("attributions = %s/%s, want ours/client", out.Data[0].Attribution, out.Data[1].Attribution)
	}
	if out.BucketSeconds != 300 {
		t.Errorf("bucketSeconds = %d, want 300 (24h window)", out.BucketSeconds)
	}
	if out.DataSource != "direct" {
		t.Errorf("dataSource = %q, want direct (watermark-missing fallback)", out.DataSource)
	}

	// Arm 2: ?attribution=ours narrows to the routing-fault group only.
	expectNoRollupWatermark()
	mock.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(errAnyArgs(3)...).WillReturnRows(rows())
	mock.ExpectQuery(`date_bin`).WithArgs(errAnyArgs(8)...).WillReturnRows(
		pgxmock.NewRows([]string{"error_code", "status_range", "provider", "model", "bucket_ts", "cnt"}))
	c2, rec2 := echoCtxQ(http.MethodGet, "/traffic/errors/groups",
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z&attribution=ours")
	if err := h.ListTrafficErrorGroups(c2); err != nil {
		t.Fatalf("handler arm2: %v", err)
	}
	var out2 struct {
		Data []struct {
			ErrorCode string `json:"errorCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode arm2: %v", err)
	}
	if len(out2.Data) != 1 || out2.Data[0].ErrorCode != "context_overflow" {
		t.Errorf("attribution=ours filter: %+v, want only context_overflow", out2.Data)
	}
}

func TestListTrafficErrorGroups_Handler_Validation(t *testing.T) {
	h, _ := newHandlerWithMock(t)
	for _, q := range []string{
		"",                                      // missing both
		"from=2026-07-16T00:00:00Z",             // missing to
		"from=nonsense&to=2026-07-17T00:00:00Z", // bad from
		"from=2026-07-16T00:00:00Z&to=nonsense", // bad to
		"from=2026-07-17T00:00:00Z&to=2026-07-16T00:00:00Z",                    // from after to
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z&attribution=nobody", // bad attribution
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z&mode=rollup-only",   // bad mode
	} {
		c, rec := echoCtxQ(http.MethodGet, "/traffic/errors/groups", q)
		if err := h.ListTrafficErrorGroups(c); err != nil {
			t.Fatalf("handler(%q): %v", q, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status %d, want 400", q, rec.Code)
		}
	}
}

// TestListTrafficErrorGroups_Handler_ModeDirect: ?mode=direct must bypass the
// rollup path entirely (no watermark read) and label the response "direct" —
// the verification escape hatch the prod parity check depends on.
func TestListTrafficErrorGroups_Handler_ModeDirect(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(errAnyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(trafficErrGroupCols).
			AddRow("AUTH_INVALID", "4xx", "", "", "bad key", 4, 1, now, now))
	mock.ExpectQuery(`date_bin`).WithArgs(errAnyArgs(8)...).
		WillReturnRows(pgxmock.NewRows([]string{"error_code", "status_range", "provider", "model", "bucket_ts", "cnt"}))

	c, rec := echoCtxQ(http.MethodGet, "/traffic/errors/groups",
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z&mode=direct")
	if err := h.ListTrafficErrorGroups(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		DataSource string `json:"dataSource"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DataSource != "direct" {
		t.Errorf("dataSource = %q, want direct", out.DataSource)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestListTrafficErrorGroups_Handler_DBError(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).
		WithArgs("rollup-5m").
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))
	mock.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WillReturnError(errors.New("db down"))
	c, rec := echoCtxQ(http.MethodGet, "/traffic/errors/groups",
		"from=2026-07-16T00:00:00Z&to=2026-07-17T00:00:00Z")
	if err := h.ListTrafficErrorGroups(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
}

// TestListTrafficErrorGroups_Handler_WindowSnap: unaligned from/to must snap
// outward to whole bucket boundaries (both read paths see the snapped
// window) and the response must report the effective window.
func TestListTrafficErrorGroups_Handler_WindowSnap(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// Zero groups → the direct path skips its second (bucket) query.
	mock.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(errAnyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(trafficErrGroupCols))

	// 24h window → 5m bins; edges at :03:27 must snap to :00:00 / :05:00.
	c, rec := echoCtxQ(http.MethodGet, "/traffic/errors/groups",
		"from=2026-07-16T10:03:27Z&to=2026-07-17T10:03:27Z&mode=direct")
	if err := h.ListTrafficErrorGroups(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		EffectiveFrom string `json:"effectiveFrom"`
		EffectiveTo   string `json:"effectiveTo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.EffectiveFrom != "2026-07-16T10:00:00Z" {
		t.Errorf("effectiveFrom = %q, want snapped-down 10:00:00Z", out.EffectiveFrom)
	}
	if out.EffectiveTo != "2026-07-17T10:05:00Z" {
		t.Errorf("effectiveTo = %q, want snapped-up 10:05:00Z", out.EffectiveTo)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
