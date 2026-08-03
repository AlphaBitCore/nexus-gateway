// GetServiceURL — the peer service URL resolution endpoint
// (GET /api/internal/things/service-url/:thing_type). pgxmock-backed like the
// rest of hubapi_mgr_test.go; asserts the HTTP contract peerurl.Resolver
// consumes: 200 {thingType, privateUrl, publicUrl}, 403 for device-token
// callers, 400 for non-service types, 404 SERVICE_URL_NOT_REPORTED, and the
// most-recently-seen selection when several Things report.
package hubapi

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
)

// listThingsCols mirrors the 29-column SELECT shape of fleet/store.ListThings
// (thing_with_overrides CTE).
var listThingsCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"bound_user_id", "bound_user_display_name", "bound_user_email",
	"override_count", "override_stale_count", "has_killswitch_bypass",
}

// addServiceThingRow appends a Thing row whose metadata carries the given
// staticInfo JSON and whose last_seen_at is seen (nil allowed).
func addServiceThingRow(rows *pgxmock.Rows, id, ttype string, metadata []byte, seen *time.Time) *pgxmock.Rows {
	now := time.Now().UTC()
	return rows.AddRow(
		id, ttype, id, "1.0", "addr",
		"sso", "bearer", "http",
		"online", []byte(`{}`), []byte(`{}`), int64(1), int64(1),
		metadata, seen, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "linux", "6.1", "",
		"", "", "",
		int64(0), int64(0), false,
	)
}

// expectListThings arms the COUNT + list query pair ListThings issues.
func expectListThings(mock pgxmock.PgxPoolIface, total int, rows *pgxmock.Rows) {
	mock.ExpectQuery(`COUNT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(total)))
	mock.ExpectQuery(`FROM thing_with_overrides`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)
}

func staticInfoJSON(privateURL, publicURL string) []byte {
	return []byte(`{"staticInfo":{"privateUrl":"` + privateURL + `","publicUrl":"` + publicURL + `"}}`)
}

// Service-token caller resolving a reporting peer gets 200 with both URLs.
func TestGetServiceURL_ServiceToken_ReportedThing_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	seen := time.Now().UTC()
	expectListThings(mock, 1, addServiceThingRow(
		pgxmock.NewRows(listThingsCols),
		"gw-1", thingtype.AIGateway,
		staticInfoJSON("http://10.0.0.5:3050", "https://gw.example.com"),
		&seen,
	))

	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": thingtype.AIGateway})
	if err := h.GetServiceURL(c); err != nil {
		t.Fatalf("GetServiceURL: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["thingType"] != thingtype.AIGateway {
		t.Errorf("thingType=%v want %q", m["thingType"], thingtype.AIGateway)
	}
	if m["privateUrl"] != "http://10.0.0.5:3050" {
		t.Errorf("privateUrl=%v want the reported private URL", m["privateUrl"])
	}
	if m["publicUrl"] != "https://gw.example.com" {
		t.Errorf("publicUrl=%v want the reported public URL", m["publicUrl"])
	}
}

// Device-token callers (agents) are refused: the private URL is an internal
// address that must never be pushed toward end-user devices.
func TestGetServiceURL_DeviceToken_Returns403(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// No DB expectations — the gate fires before any query.

	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": thingtype.AIGateway})
	c.Set(thingContextKey, &store.Thing{ID: "agent-1", Type: thingtype.Agent})
	if err := h.GetServiceURL(c); err != nil {
		t.Fatalf("GetServiceURL: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 for a device-token caller", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("device-token caller must not reach the DB: %v", err)
	}
}

// Unknown or non-service types (agent included) are a 400 — the endpoint only
// resolves server Thing types.
func TestGetServiceURL_UnknownOrNonServiceType_Returns400(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)

	for _, typ := range []string{"", "bogus", thingtype.Agent, "  "} {
		c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": typ})
		if err := h.GetServiceURL(c); err != nil {
			t.Fatalf("GetServiceURL(%q): %v", typ, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("type %q: status=%d want 400", typ, rec.Code)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("invalid types must not reach the DB: %v", err)
	}
}

// No Thing of the type reports a URL yet → 404 with the machine-readable
// SERVICE_URL_NOT_REPORTED code the peerurl resolver maps to ErrNotReported.
// Covers all three non-reporting shapes: no rows at all, a row with no
// staticInfo, and a row whose staticInfo has empty URLs.
func TestGetServiceURL_NoReportingThing_Returns404NotReported(t *testing.T) {
	e := newTestEcho()
	seen := time.Now().UTC()
	cases := []struct {
		name string
		rows func() *pgxmock.Rows
	}{
		{"no things", func() *pgxmock.Rows { return pgxmock.NewRows(listThingsCols) }},
		{"no staticInfo", func() *pgxmock.Rows {
			return addServiceThingRow(pgxmock.NewRows(listThingsCols),
				"cp-1", thingtype.ControlPlane, []byte(`{}`), &seen)
		}},
		{"staticInfo without urls", func() *pgxmock.Rows {
			return addServiceThingRow(pgxmock.NewRows(listThingsCols),
				"cp-1", thingtype.ControlPlane, staticInfoJSON("", ""), &seen)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock := newInternalAPIMock(t)
			expectListThings(mock, 1, tc.rows())
			c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": thingtype.ControlPlane})
			if err := h.GetServiceURL(c); err != nil {
				t.Fatalf("GetServiceURL: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404; body=%s", rec.Code, rec.Body.String())
			}
			m := decodeResp(t, rec)
			if m["code"] != "SERVICE_URL_NOT_REPORTED" {
				t.Errorf("code=%v want SERVICE_URL_NOT_REPORTED", m["code"])
			}
		})
	}
}

// A DB failure surfaces as a 5xx, not a bogus empty URL.
func TestGetServiceURL_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`COUNT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))

	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": thingtype.AIGateway})
	if err := h.GetServiceURL(c); err != nil {
		t.Fatalf("GetServiceURL: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 on DB error", rec.Code)
	}
}

// Two Things of the type report URLs → the most recently seen wins,
// regardless of row order (fresh-first AND fresh-last), and a nil
// last_seen_at row never beats one with a timestamp.
func TestGetServiceURL_TwoReportingThings_MostRecentlySeenWins(t *testing.T) {
	e := newTestEcho()
	older := time.Now().UTC().Add(-time.Hour)
	newer := time.Now().UTC()

	type rowSpec struct {
		id   string
		meta []byte
		seen *time.Time
	}
	freshRow := rowSpec{"gw-fresh", staticInfoJSON("http://10.0.0.9:3050", "https://fresh.example.com"), &newer}
	staleRow := rowSpec{"gw-stale", staticInfoJSON("http://10.0.0.5:3050", "https://stale.example.com"), &older}
	neverSeen := rowSpec{"gw-never", staticInfoJSON("http://10.0.0.3:3050", "https://never.example.com"), nil}

	cases := []struct {
		name string
		rows []rowSpec
	}{
		{"stale first", []rowSpec{staleRow, freshRow}},
		{"fresh first", []rowSpec{freshRow, staleRow}},
		{"nil last_seen_at loses", []rowSpec{neverSeen, freshRow}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock := newInternalAPIMock(t)
			rows := pgxmock.NewRows(listThingsCols)
			for _, r := range tc.rows {
				rows = addServiceThingRow(rows, r.id, thingtype.AIGateway, r.meta, r.seen)
			}
			expectListThings(mock, len(tc.rows), rows)

			c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"thing_type": thingtype.AIGateway})
			if err := h.GetServiceURL(c); err != nil {
				t.Fatalf("GetServiceURL: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
			}
			m := decodeResp(t, rec)
			if m["publicUrl"] != "https://fresh.example.com" {
				t.Errorf("publicUrl=%v want the most-recently-seen Thing's URL", m["publicUrl"])
			}
			if m["privateUrl"] != "http://10.0.0.9:3050" {
				t.Errorf("privateUrl=%v want the most-recently-seen Thing's URL", m["privateUrl"])
			}
		})
	}
}
