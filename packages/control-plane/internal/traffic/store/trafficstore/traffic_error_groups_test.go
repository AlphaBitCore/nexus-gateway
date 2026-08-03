package trafficstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

var errGroupCols = []string{"error_code", "status_range", "provider", "model",
	"sample_reason", "cnt", "affected_end_users", "first_seen", "last_seen"}

var errBucketCols = []string{"error_code", "status_range", "provider", "model", "bucket_ts", "cnt"}

func errGroupsParams() TrafficErrorGroupsParams {
	return TrafficErrorGroupsParams{
		From:           tNow.Add(-24 * time.Hour),
		To:             tNow,
		DBSources:      []string{"ai-gateway"},
		BucketInterval: 5 * time.Minute,
	}
}

// TestListTrafficErrorGroups_GroupsAndBuckets pins the aggregation contract:
// groups come back keyed by (errorCode, statusRange, provider, model) with
// counts and distinct end-user counts, and the second-pass sparkline buckets
// attach to the RIGHT group (keyed match, not positional).
func TestListTrafficErrorGroups_GroupsAndBuckets(t *testing.T) {
	s, m := newMock(t)
	first := tNow.Add(-2 * time.Hour)

	m.ExpectQuery(`GROUP BY 1, 2, 3, 4\s+ORDER BY cnt DESC`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("context_overflow", "4xx", "openai", "gpt-5.6", "prompt too large", 7, 2, first, tNow).
			AddRow("", "5xx", "anthropic", "claude-opus-4-7", "", 3, 0, first, tNow))

	m.ExpectQuery(`date_bin\(\$4, sub\.ts`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(errBucketCols).
			AddRow("context_overflow", "4xx", "openai", "gpt-5.6", first, 4).
			AddRow("context_overflow", "4xx", "openai", "gpt-5.6", first.Add(5*time.Minute), 3).
			AddRow("", "5xx", "anthropic", "claude-opus-4-7", first, 3))

	got, err := s.ListTrafficErrorGroups(context.Background(), errGroupsParams())
	if err != nil {
		t.Fatalf("ListTrafficErrorGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("groups = %d, want 2", len(got))
	}
	g := got[0]
	if g.ErrorCode != "context_overflow" || g.StatusRange != "4xx" ||
		g.Provider != "openai" || g.Model != "gpt-5.6" {
		t.Errorf("group key = %+v", g)
	}
	if g.Count != 7 || g.AffectedEndUsers == nil || *g.AffectedEndUsers != 2 || g.SampleReason != "prompt too large" {
		t.Errorf("group facts = count %d users %v reason %q", g.Count, g.AffectedEndUsers, g.SampleReason)
	}
	if len(g.Buckets) != 2 || g.Buckets[0].Count != 4 || g.Buckets[1].Count != 3 {
		t.Errorf("buckets misattached: %+v", g.Buckets)
	}
	// The unclassified 5xx group must receive ITS bucket, not the other group's.
	if len(got[1].Buckets) != 1 || got[1].Buckets[0].Count != 3 {
		t.Errorf("second group buckets = %+v, want the single 5xx bin", got[1].Buckets)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestListTrafficErrorGroups_EmptySkipsBucketQuery(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols))
	got, err := s.ListTrafficErrorGroups(context.Background(), errGroupsParams())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty window: %+v err=%v", got, err)
	}
	// No second ExpectQuery registered: ExpectationsWereMet fails if the
	// bucket query ran anyway.
	if err := m.ExpectationsWereMet(); err != nil {
		t.Errorf("bucket query must be skipped on zero groups: %v", err)
	}
}

func TestListTrafficErrorGroups_Validation(t *testing.T) {
	s, _ := newMock(t)
	p := errGroupsParams()
	p.To = p.From // from == to
	if _, err := s.ListTrafficErrorGroups(context.Background(), p); err == nil {
		t.Error("from==to must error")
	}
	p = errGroupsParams()
	p.BucketInterval = 0
	if _, err := s.ListTrafficErrorGroups(context.Background(), p); err == nil {
		t.Error("zero bucket interval must error")
	}
	p = errGroupsParams()
	p.From, p.To = time.Time{}, time.Time{}
	if _, err := s.ListTrafficErrorGroups(context.Background(), p); err == nil {
		t.Error("zero from/to must error")
	}
}

func TestListTrafficErrorGroups_Errors(t *testing.T) {
	// Group query error.
	s, m := newMock(t)
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WillReturnError(errors.New("boom"))
	if _, err := s.ListTrafficErrorGroups(context.Background(), errGroupsParams()); err == nil {
		t.Error("group query error must surface")
	}
	// Group scan error (wrong column count).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow("x"))
	if _, err := s2.ListTrafficErrorGroups(context.Background(), errGroupsParams()); err == nil {
		t.Error("group scan error must surface")
	}
	// Bucket query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("RATE_LIMITED", "4xx", "", "", "", 1, 0, tNow, tNow))
	m3.ExpectQuery(`date_bin`).WithArgs(anyArgs(8)...).WillReturnError(errors.New("boom"))
	if _, err := s3.ListTrafficErrorGroups(context.Background(), errGroupsParams()); err == nil {
		t.Error("bucket query error must surface")
	}
	// Bucket scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`GROUP BY 1, 2, 3, 4`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("RATE_LIMITED", "4xx", "", "", "", 1, 0, tNow, tNow))
	m4.ExpectQuery(`date_bin`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow("x"))
	if _, err := s4.ListTrafficErrorGroups(context.Background(), errGroupsParams()); err == nil {
		t.Error("bucket scan error must surface")
	}
}

// TestListTrafficEvents_ExactAndNoneSentinels pins the drill-down class
// boundary: modelExact emits an exact COALESCE match (never ILIKE), and the
// __none__ sentinels for provider/modelExact emit empty-COALESCE predicates
// with no bind args — absent dimensions are selectable, not wildcards.
func TestListTrafficEvents_ExactAndNoneSentinels(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`COALESCE\(a\.routed_model_name, a\.model_name\) = \$2`).
		WithArgs(pgxmock.AnyArg(), "gpt-4o").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m.ExpectQuery(`COALESCE\(a\.routed_model_name, a\.model_name\) = \$2.*ORDER BY`).
		WithArgs(pgxmock.AnyArg(), "gpt-4o", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(trafficEventRow(0))
	if _, _, err := s.ListTrafficEvents(context.Background(), TrafficEventListParams{
		DBSources: []string{"ai-gateway"}, ModelExact: "gpt-4o", Limit: 20,
	}); err != nil {
		t.Fatalf("modelExact: %v", err)
	}

	s2, m2 := newMock(t)
	m2.ExpectQuery(`COALESCE\(a\.routed_provider_name, a\.provider_name, ''\) = ''.*COALESCE\(a\.routed_model_name, a\.model_name, ''\) = ''`).
		WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m2.ExpectQuery(`COALESCE\(a\.routed_model_name, a\.model_name, ''\) = ''.*ORDER BY`).
		WithArgs(anyArgs(3)...).
		WillReturnRows(trafficEventRow(0))
	if _, _, err := s2.ListTrafficEvents(context.Background(), TrafficEventListParams{
		DBSources: []string{"ai-gateway"}, Provider: FilterNone, ModelExact: FilterNone, Limit: 20,
	}); err != nil {
		t.Fatalf("none sentinels: %v", err)
	}
	if err := m2.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// Default sources: empty DBSources must still bind a non-empty source set
// (all data-plane writers) rather than matching nothing.
func TestListTrafficErrorGroups_DefaultSources(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`a\.source = ANY\(\$3\)`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols))
	p := errGroupsParams()
	p.DBSources = nil
	if _, err := s.ListTrafficErrorGroups(context.Background(), p); err != nil {
		t.Fatalf("default sources: %v", err)
	}
}
