package trafficstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

// trafficEventRow builds a column set + value row matching scanOneTrafficEvent's
// 89 destinations. Only the non-nullable scan targets carry values; everything
// else is SQL NULL (nil) which the pointer / json.RawMessage / []string targets
// accept. extra appends additional nil columns for GetTrafficEvent's payload
// JOIN (request/response body + spill refs).
func trafficEventRow(extra int) *pgxmock.Rows {
	const n = 89
	cols := make([]string, n+extra)
	vals := make([]any, n+extra)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
		vals[i] = nil
	}
	vals[0] = "evt1"       // ID (string)
	vals[1] = "ai-gateway" // Source (string)
	vals[2] = tNow         // Timestamp (time.Time)
	vals[66] = tNow        // CreatedAt (time.Time)
	return pgxmock.NewRows(cols).AddRow(vals...)
}

// GetTrafficEventForNormalize must feed the view-time normalize the INGRESS
// frame: AdapterType from a.ingress_format (NOT the upstream provider's
// adapter_type) and Path from a.path (NOT target_path). The stored bodies are
// client-facing, so any other choice mis-selects the codec for cross-protocol
// routes and renders raw generic-http JSON. This pins that mapping.
func TestGetTrafficEventForNormalize(t *testing.T) {
	s, m := newMock(t)
	cols := []string{"ingress_format", "model", "path", "req_body", "req_enc", "resp_body", "resp_enc", "req_ct", "resp_ct"}
	m.ExpectQuery(`COALESCE\(a.ingress_format`).WithArgs("evt1").
		WillReturnRows(pgxmock.NewRows(cols).AddRow(
			"anthropic", "claude-opus-4-7", "/v1/messages",
			[]byte(`{"contents":1}`), "", []byte(`{"content":[]}`), "",
			"application/json", "application/json"))

	in, err := s.GetTrafficEventForNormalize(context.Background(), "evt1")
	if err != nil {
		t.Fatalf("GetTrafficEventForNormalize: %v", err)
	}
	if !in.Found {
		t.Fatal("Found=false, want true")
	}
	if in.AdapterType != "anthropic" {
		t.Errorf("AdapterType=%q, want %q (must be ingress_format, not the upstream provider adapter)", in.AdapterType, "anthropic")
	}
	if in.Path != "/v1/messages" {
		t.Errorf("Path=%q, want %q (must be the ingress path, not target_path)", in.Path, "/v1/messages")
	}
	if in.Model != "claude-opus-4-7" {
		t.Errorf("Model=%q, want claude-opus-4-7", in.Model)
	}
	if string(in.RequestBody) != `{"contents":1}` || string(in.ResponseBody) != `{"content":[]}` {
		t.Errorf("bodies decoded wrong: req=%q resp=%q", in.RequestBody, in.ResponseBody)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestGetTrafficEventForNormalize_NotFoundAndError(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`COALESCE\(a.ingress_format`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	in, err := s.GetTrafficEventForNormalize(context.Background(), "gone")
	if err != nil {
		t.Fatalf("ErrNoRows must map to Found=false, not error: %v", err)
	}
	if in == nil || in.Found {
		t.Fatalf("want non-nil NormalizeInput with Found=false, got %+v", in)
	}

	s2, m2 := newMock(t)
	m2.ExpectQuery(`COALESCE\(a.ingress_format`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s2.GetTrafficEventForNormalize(context.Background(), "x"); err == nil {
		t.Fatal("want error from query failure, got nil")
	}
}

func allFiltersParams() TrafficEventListParams {
	start, end := tNow.Add(-time.Hour), tNow
	return TrafficEventListParams{
		DBSources: []string{"ai-gateway"}, Provider: "openai", EntityID: "e1", OrgID: "o1",
		EntityType: "user", ProjectID: "p1", VirtualKeyID: "vk1", ModelUsed: "gpt", RequestID: "r1",
		HookDecision: "allow", ResponseHookDecision: "allow", StatusCode: ip(200), CacheStatus: sp("HIT"),
		TargetHost: "api.openai.com", Path: "/v1/chat", SourceProcess: "curl", BumpStatus: "bumped",
		ComplianceTags: []string{"pii"}, APIKeyFingerprint: "fp", UsageExtractionStatus: "ok",
		ThingID: "t1", RoutingRuleID: "rr1", ErrorCode: "PROVIDER_ERROR", ExcludeInternal: true,
		StartTime: &start, EndTime: &end, Limit: 20, Offset: 0,
	}
}

func TestListTrafficEvents_AllFilters(t *testing.T) {
	s, m := newMock(t)
	// 25 where-args (every filter set, StatusCode wins over StatusRange,
	// ExcludeInternal adds no placeholder); data query adds limit+offset → 27.
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event a WHERE`).WithArgs(anyArgs(25)...).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM traffic_event a\s+LEFT JOIN "Model"`).WithArgs(anyArgs(27)...).
		WillReturnRows(trafficEventRow(0))
	ev, total, err := s.ListTrafficEvents(context.Background(), allFiltersParams())
	if err != nil || total != 1 || len(ev) != 1 || ev[0].ID != "evt1" {
		t.Fatalf("ListTrafficEvents all-filters: %+v total=%d err=%v", ev, total, err)
	}
}

func TestListTrafficEvents_DefaultSourcesStatusRange(t *testing.T) {
	n := len(domain.AllDataPlaneDBSources()) // default source set when DBSources empty
	for _, rng := range []string{"2xx", "4xx", "5xx"} {
		t.Run(rng, func(t *testing.T) {
			s, m := newMock(t)
			m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(n)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
			m.ExpectQuery(`FROM traffic_event a`).WithArgs(anyArgs(n + 2)...).WillReturnRows(trafficEventRow(0))
			if _, _, err := s.ListTrafficEvents(context.Background(), TrafficEventListParams{StatusRange: rng}); err != nil {
				t.Fatalf("status range %s: %v", rng, err)
			}
		})
	}
}

func TestListTrafficEvents_Errors(t *testing.T) {
	// Count error.
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.ListTrafficEvents(context.Background(), TrafficEventListParams{DBSources: []string{"agent"}}); err == nil {
		t.Fatal("count error must surface")
	}
	// Data query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM traffic_event a`).WillReturnError(errors.New("boom"))
	if _, _, err := s2.ListTrafficEvents(context.Background(), TrafficEventListParams{DBSources: []string{"agent"}}); err == nil {
		t.Fatal("data error must surface")
	}
	// Scan error (1-col row vs 89 dests).
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM traffic_event a`).WithArgs(anyArgs(3)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("x"))
	if _, _, err := s3.ListTrafficEvents(context.Background(), TrafficEventListParams{DBSources: []string{"agent"}}); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestGetTrafficEvent(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`LEFT JOIN traffic_event_payload p`).WithArgs("evt1").WillReturnRows(trafficEventRow(6))
	got, err := s.GetTrafficEvent(context.Background(), "evt1")
	if err != nil || got == nil || got.ID != "evt1" {
		t.Fatalf("GetTrafficEvent: %+v %v", got, err)
	}
	m.ExpectQuery(`LEFT JOIN traffic_event_payload`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if got, err := s.GetTrafficEvent(context.Background(), "gone"); err != nil || got != nil {
		t.Fatalf("not-found should be (nil,nil): %+v %v", got, err)
	}
	m.ExpectQuery(`LEFT JOIN traffic_event_payload`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.GetTrafficEvent(context.Background(), "x"); err == nil {
		t.Fatal("db error must surface")
	}
}

func TestGetTrafficEventNormalized(t *testing.T) {
	s, m := newMock(t)
	normCols := []string{"traffic_event_id", "request_normalized", "response_normalized",
		"request_status", "response_status", "request_error_reason", "response_error_reason",
		"request_redaction_spans", "response_redaction_spans",
		"normalize_version", "created_at"}
	reqSpans := []byte(`[{"contentAddress":"messages.0.content.0","start":0,"end":10}]`)
	m.ExpectQuery(`FROM traffic_event_normalized`).WithArgs("evt1").
		WillReturnRows(pgxmock.NewRows(normCols).AddRow("evt1", []byte(`{}`), []byte(`{}`), sp("ok"), sp("ok"), nil, nil, reqSpans, nil, "1", tNow))
	got, err := s.GetTrafficEventNormalized(context.Background(), "evt1")
	if err != nil || got == nil || got.TrafficEventID != "evt1" || got.NormalizeVersion != "1" {
		t.Fatalf("GetTrafficEventNormalized: %+v %v", got, err)
	}
	if string(got.RequestRedactionSpans) != string(reqSpans) || got.ResponseRedactionSpans != nil {
		t.Fatalf("redaction spans not scanned through: req=%s resp=%v", got.RequestRedactionSpans, got.ResponseRedactionSpans)
	}
	m.ExpectQuery(`traffic_event_normalized`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	if got, err := s.GetTrafficEventNormalized(context.Background(), "gone"); err != nil || got != nil {
		t.Fatalf("not-found should be (nil,nil): %+v %v", got, err)
	}
	m.ExpectQuery(`traffic_event_normalized`).WithArgs("x").WillReturnError(errors.New("boom"))
	if _, err := s.GetTrafficEventNormalized(context.Background(), "x"); err == nil {
		t.Fatal("db error must surface")
	}
}

var adminAuditCols = []string{"id", "sequenceNumber", "timestamp", "actorId", "actorLabel", "actorRole",
	"sourceIp", "action", "entityType", "entityId", "beforeState", "afterState",
	"nexusRequestId"}

func adminAuditRow() []any {
	return []any{"a1", 7, tNow, "u1", "Alice", sp("admin"), sp("1.2.3.4"), "update", "VirtualKey", sp("vk1"),
		[]byte(`{}`), []byte(`{}`), sp("nr1")}
}

func TestListAdminAuditLogs(t *testing.T) {
	// All filters → buildAdminAuditWhere 8 args; data adds limit+offset → 10.
	s, m := newMock(t)
	start, end := tNow.Add(-time.Hour), tNow
	p := AdminAuditLogListParams{StartTime: &start, EndTime: &end, ActorID: "u1", ActorLabel: "Alice",
		ActorRole: "admin", Action: "update", EntityType: "VirtualKey", NexusRequestID: "nr1", Limit: 20}
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM "AdminAuditLog"`).WithArgs(anyArgs(8)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM "AdminAuditLog"`).WithArgs(anyArgs(10)...).WillReturnRows(pgxmock.NewRows(adminAuditCols).AddRow(adminAuditRow()...))
	ev, total, err := s.ListAdminAuditLogs(context.Background(), p)
	if err != nil || total != 1 || len(ev) != 1 || ev[0].ID != "a1" {
		t.Fatalf("ListAdminAuditLogs: %+v total=%d err=%v", ev, total, err)
	}

	// Count error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := s2.ListAdminAuditLogs(context.Background(), AdminAuditLogListParams{}); err == nil {
		t.Fatal("count error must surface")
	}
	// Data query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnError(errors.New("boom"))
	if _, _, err := s3.ListAdminAuditLogs(context.Background(), AdminAuditLogListParams{}); err == nil {
		t.Fatal("data error must surface")
	}
	// Scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m4.ExpectQuery(`FROM "AdminAuditLog"`).WithArgs(0, 0).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("a1"))
	if _, _, err := s4.ListAdminAuditLogs(context.Background(), AdminAuditLogListParams{}); err == nil {
		t.Fatal("scan error must surface")
	}
	// Mid-stream iterate error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m5.ExpectQuery(`FROM "AdminAuditLog"`).WithArgs(0, 0).WillReturnRows(pgxmock.NewRows(adminAuditCols).AddRow(adminAuditRow()...).CloseError(errors.New("conn reset")))
	if _, _, err := s5.ListAdminAuditLogs(context.Background(), AdminAuditLogListParams{}); err == nil {
		t.Fatal("iterate error must surface")
	}
}

func TestExportAdminAuditLogs(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "AdminAuditLog".*LIMIT \$1`).WithArgs(1000).WillReturnRows(pgxmock.NewRows(adminAuditCols).AddRow(adminAuditRow()...))
	got, err := s.ExportAdminAuditLogs(context.Background(), AdminAuditLogListParams{}, 1000)
	if err != nil || len(got) != 1 {
		t.Fatalf("ExportAdminAuditLogs: %+v %v", got, err)
	}
	m.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnError(errors.New("boom"))
	if _, err := s.ExportAdminAuditLogs(context.Background(), AdminAuditLogListParams{}, 1000); err == nil {
		t.Fatal("query error must surface")
	}
	// Scan error during export (1-col row vs 16 dests).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "AdminAuditLog"`).WithArgs(1000).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("a1"))
	if _, err := s2.ExportAdminAuditLogs(context.Background(), AdminAuditLogListParams{}, 1000); err == nil {
		t.Fatal("export scan error must surface")
	}
}

func TestListDistinctAuditEventTypes(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT DISTINCT "entityType", "action"`).
		WillReturnRows(pgxmock.NewRows([]string{"entityType", "action"}).AddRow("VirtualKey", "create").AddRow("Provider", "delete"))
	got, err := s.ListDistinctAuditEventTypes(context.Background())
	if err != nil || len(got) != 2 || got[0].EntityType != "VirtualKey" {
		t.Fatalf("ListDistinctAuditEventTypes: %+v %v", got, err)
	}
	m.ExpectQuery(`SELECT DISTINCT`).WillReturnError(errors.New("boom"))
	if _, err := s.ListDistinctAuditEventTypes(context.Background()); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c"}).AddRow("x", "y", "z"))
	if _, err := s2.ListDistinctAuditEventTypes(context.Background()); err == nil {
		t.Fatal("scan error must surface")
	}
}
