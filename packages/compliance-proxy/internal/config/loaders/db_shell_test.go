package loaders

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// db_shell_test.go covers the thin database/sql shell layer (LoadX
// functions) of each loader. The interesting business logic is owned by
// the pure helpers (decodeObservabilityResult, decodePayloadCaptureResult,
// buildAllowlistEntries, buildHookConfigsFromRows,
// decodeInterceptionDomainRows, attachInterceptionPaths) — those tests
// live in their respective *_test.go files.
//
// These tests assert the database/sql contract: query error wrapping,
// rows.Scan error wrapping, rows.Err propagation, and clean propagation
// of the scanned data into the pure helpers. They run against
// DATA-DOG/go-sqlmock so the package does not need a live Postgres.

// newSQLMock wraps a fresh sqlmock + sql.DB. The QueryMatcherEqual choice
// keeps the query patterns lenient — sqlmock's default regex matcher
// trips on Postgres-quoted identifiers like "interception_domain". The
// QueryMatcherEqual matcher pairs with mock.ExpectQuery(...) where the
// expected string is treated as a literal prefix.
func newSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func TestLoadObservabilityConfig_QueryErrorPropagates(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("planner err")
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = 'observability.config'`).
		WillReturnError(want)
	got, err := LoadObservabilityConfig(context.Background(), db)
	if got != nil {
		t.Errorf("err path must return nil Config; got %+v", got)
	}
	if !errors.Is(err, want) {
		t.Errorf("err must propagate; got: %v", err)
	}
}

func TestLoadObservabilityConfig_MissingRowReturnsDefault(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = 'observability.config'`).
		WillReturnError(sql.ErrNoRows)
	got, err := LoadObservabilityConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("ErrNoRows must NOT propagate: %v", err)
	}
	if got == nil || got.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("missing-row default wrong: %+v", got)
	}
	if got.Enabled {
		t.Errorf("missing-row default must leave tracing disabled, got %+v", got)
	}
}

func TestLoadObservabilityConfig_HappyPathDecodesValue(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = 'observability.config'`).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"otelEnabled":true,"samplingRate":0.5}`)))
	got, err := LoadObservabilityConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.Enabled || got.SamplingRate != 0.5 || got.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("decoded mismatch: %+v", got)
	}
}

func TestLoadPayloadCaptureConfig_QueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("conn refused")
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = $1`).
		WithArgs(systemMetadataPayloadCaptureKey).WillReturnError(want)
	_, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err == nil {
		t.Fatal("err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("err must wrap original; got: %v", err)
	}
	if !strings.Contains(err.Error(), "payload capture: query system_metadata") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadPayloadCaptureConfig_MissingRowReturnsDefault(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = $1`).
		WithArgs(systemMetadataPayloadCaptureKey).WillReturnError(sql.ErrNoRows)
	got, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("ErrNoRows must NOT propagate: %v", err)
	}
	if got.StoreRequestBody || got.StoreResponseBody {
		t.Errorf("missing row must yield capture-off default; got %+v", got)
	}
}

func TestLoadPayloadCaptureConfig_HappyPath(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = $1`).
		WithArgs(systemMetadataPayloadCaptureKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":262144,"maxRequestBytes":10485760,"maxResponseBytes":10485760}`)))
	got, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.StoreRequestBody || !got.StoreResponseBody {
		t.Errorf("happy path did not thread capture flags: %+v", got)
	}
}

const domainAllowlistQuery = `
		SELECT host_pattern, host_match_type
		FROM "interception_domain"
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`

func TestLoadDomainAllowlist_QueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("planner err")
	mock.ExpectQuery(domainAllowlistQuery).WillReturnError(want)
	_, err := LoadDomainAllowlist(context.Background(), db)
	if err == nil {
		t.Fatal("err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("err must wrap original; got: %v", err)
	}
	if !strings.Contains(err.Error(), "load domain allowlist") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadDomainAllowlist_ScanErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	// Two columns expected by the scan; deliver only one to force a
	// scan err.
	mock.ExpectQuery(domainAllowlistQuery).
		WillReturnRows(sqlmock.NewRows([]string{"host_pattern"}).AddRow("only-one"))
	_, err := LoadDomainAllowlist(context.Background(), db)
	if err == nil {
		t.Fatal("scan err must surface")
	}
	if !strings.Contains(err.Error(), "scan domain allowlist row") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadDomainAllowlist_RowsErrPropagates(t *testing.T) {
	// rows.Err() after a successful Next() loop returning the trailing
	// iteration error. sqlmock's RowError lets us inject that.
	db, mock := newSQLMock(t)
	mock.ExpectQuery(domainAllowlistQuery).
		WillReturnRows(sqlmock.NewRows([]string{"host_pattern", "host_match_type"}).
			AddRow("api.openai.com", "EXACT").
			RowError(0, errors.New("rows iteration broke")))
	_, err := LoadDomainAllowlist(context.Background(), db)
	if err == nil {
		t.Fatal("rows.Err must propagate")
	}
}

func TestLoadDomainAllowlist_HappyPath(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(domainAllowlistQuery).
		WillReturnRows(sqlmock.NewRows([]string{"host_pattern", "host_match_type"}).
			AddRow("api.openai.com", "EXACT").
			AddRow("*.anthropic.com", "GLOB").
			AddRow(`^regex$`, "REGEX")) // dropped by normaliser
	got, err := LoadDomainAllowlist(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("REGEX must drop; got %v", got)
	}
	if got[0] != "api.openai.com:443" || got[1] != "*.anthropic.com:443" {
		t.Errorf("happy path threading: %v", got)
	}
}

// hookCols mirrors the SELECT column list. Helper keeps the sqlmock
// NewRows call readable.
var hookCols = []string{
	"id", "name", "type", "implementationId", "stage",
	"category", "endpoint", "script", "config",
	"priority", "timeoutMs", "failBehavior", "enabled",
	"applicableIngress",
}

func TestLoadHookConfigs_QueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("planner err")
	mock.ExpectQuery(hookConfigQuery).WillReturnError(want)
	_, err := LoadHookConfigs(context.Background(), db)
	if err == nil || !errors.Is(err, want) {
		t.Errorf("err must wrap original; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configloader: query HookConfig") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadHookConfigs_ScanErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	// Only 1 column where 14 are scanned — forces a scan err.
	mock.ExpectQuery(hookConfigQuery).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("x"))
	_, err := LoadHookConfigs(context.Background(), db)
	if err == nil {
		t.Fatal("scan err must surface")
	}
	if !strings.Contains(err.Error(), "configloader: scan HookConfig row") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadHookConfigs_RowsErrPropagates(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(hookConfigQuery).
		WillReturnRows(sqlmock.NewRows(hookCols).
			AddRow("h1", "n", "builtin", "noop", "request",
				nil, nil, nil, nil,
				0, 1000, "fail-open", true,
				"{ALL}").
			RowError(0, errors.New("iter broke")))
	_, err := LoadHookConfigs(context.Background(), db)
	if err == nil {
		t.Fatal("rows.Err must propagate")
	}
	if !strings.Contains(err.Error(), "configloader: iterate HookConfig") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadHookConfigs_HappyPath(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(hookConfigQuery).
		WillReturnRows(sqlmock.NewRows(hookCols).
			AddRow("h1", "filter", "builtin", "keyword-filter", "request",
				"content-safety", nil, nil, `{"keywords":["foo"]}`,
				10, 3000, "fail-closed", true,
				"{ALL}").
			AddRow("h2", "min", "builtin", "noop", "response",
				nil, nil, nil, nil,
				20, 500, "fail-open", true,
				"{COMPLIANCE_PROXY,AGENT}"))
	got, err := LoadHookConfigs(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: %d", len(got))
	}
	if got[0].ID != "h1" || got[0].Priority != 10 || got[0].FailBehavior != "fail-closed" {
		t.Errorf("first row threading: %+v", got[0])
	}
	if v, ok := got[0].Config["keywords"]; !ok {
		t.Errorf("Config jsonb not decoded: %+v", got[0].Config)
	} else if arr, _ := v.([]any); len(arr) != 1 || arr[0] != "foo" {
		t.Errorf("Config jsonb contents wrong: %+v", v)
	}
	wantIngress := []string{"COMPLIANCE_PROXY", "AGENT"}
	if len(got[1].ApplicableIngress) != 2 ||
		got[1].ApplicableIngress[0] != wantIngress[0] ||
		got[1].ApplicableIngress[1] != wantIngress[1] {
		t.Errorf("ApplicableIngress not threaded: %v", got[1].ApplicableIngress)
	}
}

const interceptionDomainsQuery = `
		SELECT id, name, host_pattern, host_match_type::text, adapter_id,
		       COALESCE(network_zone::text, 'PUBLIC'),
		       COALESCE(default_path_action::text, 'PROCESS'),
		       COALESCE(on_adapter_error::text, 'FAIL_OPEN'),
		       enabled, priority, updated_at,
		       streaming_mode, streaming_chunk_bytes, streaming_hook_timeout_ms,
		       streaming_max_buffer_bytes, streaming_fail_behavior,
		       capture_request_body, capture_response_body, raw_body_spill_enabled
		FROM "interception_domain"
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`

const interceptionPathsQuery = `
		SELECT id, domain_id, to_jsonb(path_pattern)::text,
		       match_type::text, action::text
		FROM "interception_path"
		ORDER BY domain_id ASC, id ASC
	`

var interceptionDomainCols = []string{
	"id", "name", "host_pattern", "host_match_type", "adapter_id",
	"network_zone", "default_path_action", "on_adapter_error",
	"enabled", "priority", "updated_at",
	"streaming_mode", "streaming_chunk_bytes", "streaming_hook_timeout_ms",
	"streaming_max_buffer_bytes", "streaming_fail_behavior",
	"capture_request_body", "capture_response_body", "raw_body_spill_enabled",
}

var interceptionPathCols = []string{"id", "domain_id", "patterns_json", "match_type", "action"}

func TestLoadInterceptionDomainsFull_DomainQueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("planner err")
	mock.ExpectQuery(interceptionDomainsQuery).WillReturnError(want)
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil || !errors.Is(err, want) {
		t.Errorf("err must wrap original; got: %v", err)
	}
	if !strings.Contains(err.Error(), "load interception domains") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_DomainScanErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("x"))
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil {
		t.Fatal("scan err must surface")
	}
	if !strings.Contains(err.Error(), "scan interception domain") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_DomainRowsErrPropagates(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true).RowError(0, errors.New("iter broke")))
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil {
		t.Fatal("rows.Err must propagate")
	}
	if !strings.Contains(err.Error(), "iterate interception domains") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_EmptyDomainsShortCircuits(t *testing.T) {
	// Zero domains → return without firing the path query at all. This
	// is the load-bearing short-circuit that prevents an unnecessary
	// round-trip on a fresh deploy.
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(sqlmock.NewRows(interceptionDomainCols))
	got, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
	// If the test reaches here without sqlmock yelling about an
	// unfulfilled expectation, the path query was not issued — which
	// is the invariant we care about.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("path query should NOT fire on empty domains: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_PathQueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true))
	want := errors.New("path query err")
	mock.ExpectQuery(interceptionPathsQuery).WillReturnError(want)
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil || !errors.Is(err, want) {
		t.Errorf("err must wrap original; got: %v", err)
	}
	if !strings.Contains(err.Error(), "load interception paths") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_PathScanErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true))
	mock.ExpectQuery(interceptionPathsQuery).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("p"))
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil {
		t.Fatal("scan err must surface")
	}
	if !strings.Contains(err.Error(), "scan interception path") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_PathRowsErrPropagates(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true))
	mock.ExpectQuery(interceptionPathsQuery).
		WillReturnRows(sqlmock.NewRows(interceptionPathCols).
			AddRow("p1", "d1", `["/x"]`, "PREFIX", "PROCESS").
			RowError(0, errors.New("path iter broke")))
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil {
		t.Fatal("rows.Err must propagate")
	}
	if !strings.Contains(err.Error(), "iterate interception paths") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

func TestLoadInterceptionDomainsFull_HappyPathThreadsPaths(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true))
	mock.ExpectQuery(interceptionPathsQuery).
		WillReturnRows(sqlmock.NewRows(interceptionPathCols).
			AddRow("p1", "d1", `["/v1/chat/*"]`, "PREFIX", "PROCESS").
			AddRow("p2", "d1", `["/v1/embeddings"]`, "EXACT", "BLOCK"))
	got, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("domain count: %d", len(got))
	}
	if len(got[0].Paths) != 2 {
		t.Errorf("path count: %d, paths=%v", len(got[0].Paths), got[0].Paths)
	}
}

func TestLoadInterceptionDomainsFull_HappyPathMalformedPathJSON(t *testing.T) {
	// A corrupt PatternsJSON must propagate from attachInterceptionPaths
	// — the DB shell does not catch it independently. Covers the wiring
	// of that path-decode error.
	db, mock := newSQLMock(t)
	mock.ExpectQuery(interceptionDomainsQuery).
		WillReturnRows(domainRowOK(true))
	mock.ExpectQuery(interceptionPathsQuery).
		WillReturnRows(sqlmock.NewRows(interceptionPathCols).
			AddRow("p1", "d1", `not json`, "PREFIX", "PROCESS"))
	_, err := LoadInterceptionDomainsFull(context.Background(), db)
	if err == nil {
		t.Fatal("malformed PatternsJSON must surface")
	}
	if !strings.Contains(err.Error(), "decode path_pattern") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

// domainRowOK builds a single happy-path domain row for the
// interception_domain SELECT. enabled controls the bool column.
func domainRowOK(enabled bool) *sqlmock.Rows {
	return sqlmock.NewRows(interceptionDomainCols).
		AddRow("d1", "openai", "api.openai.com", "EXACT", "openai-v1",
			"PUBLIC", "PROCESS", "FAIL_OPEN",
			enabled, 10, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
			nil, nil, nil,
			nil, nil,
			nil, nil, nil)
}
