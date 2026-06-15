package analyticsstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func fp(f float64) *float64 { return &f }
func i64(i int64) *int64    { return &i }
func sp(s string) *string   { return &s }

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

// ---- GetAnalyticsGroupBy ----

func TestGetAnalyticsGroupBy_InvalidKey(t *testing.T) {
	s, _ := newMock(t)
	if _, _, err := s.GetAnalyticsGroupBy(context.Background(), "bogus", nil, nil, "", nil); err == nil {
		t.Fatal("invalid group key must error")
	}
}

func TestGetAnalyticsGroupBy_Default(t *testing.T) {
	// provider key, no sum, no time, no params → source='ai-gateway' filter.
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT\(DISTINCT COALESCE\(routed_provider_name, provider_name\)\) FROM traffic_event WHERE source = 'ai-gateway'`).WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
	m.ExpectQuery(`SELECT COALESCE\(routed_provider_name, provider_name\) AS grp, COUNT\(\*\) AS cnt FROM traffic_event WHERE source = 'ai-gateway'`).WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt"}).AddRow("openai", 10).AddRow("anthropic", 5))
	res, total, err := s.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "", nil)
	if err != nil || total != 2 || len(res) != 2 || res[0].Group != "openai" || res[0].RequestCount != 10 {
		t.Fatalf("default groupBy: %+v total=%d err=%v", res, total, err)
	}
}

func TestGetAnalyticsGroupBy_TokensWithTimeAndSearch(t *testing.T) {
	// modelUsed + tokens sum + start/end + search + limit/offset.
	s, m := newMock(t)
	start, end := tNow.Add(-time.Hour), tNow
	m.ExpectQuery(`COUNT\(DISTINCT COALESCE\(routed_model_name, model_name\)\).*ILIKE \$3`).
		WithArgs(start, end, "%gpt%").WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`SELECT COALESCE\(routed_model_name, model_name\) AS grp.*SUM\(prompt_tokens\).*LIMIT 5 OFFSET 10`).
		WithArgs(start, end, "%gpt%").
		WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt", "pt", "ct", "tt"}).
			AddRow("gpt-4o", 7, i64(100), i64(200), i64(300)).
			AddRow("gpt-3.5", 3, nil, nil, nil)) // nil token sums → fields stay 0
	res, total, err := s.GetAnalyticsGroupBy(context.Background(), "modelUsed", &start, &end, "tokens",
		&AnalyticsGroupByParams{Search: "gpt", Limit: 5, Offset: 10})
	if err != nil || total != 1 || len(res) != 2 || res[0].TotalPromptTokens != 100 || res[0].TotalTokens != 300 || res[1].TotalTokens != 0 {
		t.Fatalf("tokens groupBy: %+v total=%d err=%v", res, total, err)
	}
}

func TestGetAnalyticsGroupBy_CostAndAllSources(t *testing.T) {
	// entityId is an allSources key (WHERE 1=1, no source filter) + cost sum.
	s, m := newMock(t)
	m.ExpectQuery(`COUNT\(DISTINCT entity_id\) FROM traffic_event WHERE 1=1`).WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`SELECT entity_id AS grp.*SUM\(estimated_cost_usd\)`).WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt", "cost", "tt", "gcs", "cns"}).
			AddRow("ent1", 4, fp(1.25), i64(50), 0.1, 0.05).
			AddRow("ent2", 2, nil, nil, 0.0, 0.0)) // nil cost/tt → 0
	res, _, err := s.GetAnalyticsGroupBy(context.Background(), "entityId", nil, nil, "cost", nil)
	if err != nil || len(res) != 2 || res[0].TotalCostUsd != 1.25 || res[0].GatewayCacheSavingsUsd != 0.1 || res[1].TotalCostUsd != 0 {
		t.Fatalf("cost groupBy: %+v %v", res, err)
	}
}

func TestGetAnalyticsGroupBy_Errors(t *testing.T) {
	// Count error.
	s, m := newMock(t)
	m.ExpectQuery(`COUNT\(DISTINCT`).WillReturnError(errors.New("boom"))
	if _, _, err := s.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "", nil); err == nil {
		t.Fatal("count error must surface")
	}
	// Data query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`COUNT\(DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`AS grp`).WillReturnError(errors.New("boom"))
	if _, _, err := s2.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "", nil); err == nil {
		t.Fatal("data error must surface")
	}
	// Scan error (default variant): 1-col row vs 2 dests.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`COUNT\(DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`AS grp`).WillReturnRows(pgxmock.NewRows([]string{"grp"}).AddRow("openai"))
	if _, _, err := s3.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "", nil); err == nil {
		t.Fatal("scan error must surface")
	}
	// tokens scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`COUNT\(DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m4.ExpectQuery(`AS grp`).WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt"}).AddRow("m", 1))
	if _, _, err := s4.GetAnalyticsGroupBy(context.Background(), "modelUsed", nil, nil, "tokens", nil); err == nil {
		t.Fatal("tokens scan error must surface")
	}
	// cost scan error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`COUNT\(DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m5.ExpectQuery(`AS grp`).WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt"}).AddRow("p", 1))
	if _, _, err := s5.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "cost", nil); err == nil {
		t.Fatal("cost scan error must surface")
	}
	// Mid-stream iterate error (rows.Err()).
	s6, m6 := newMock(t)
	m6.ExpectQuery(`COUNT\(DISTINCT`).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m6.ExpectQuery(`AS grp`).WillReturnRows(pgxmock.NewRows([]string{"grp", "cnt"}).AddRow("openai", 1).CloseError(errors.New("conn reset")))
	if _, _, err := s6.GetAnalyticsGroupBy(context.Background(), "provider", nil, nil, "", nil); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- GetProviderAnalyticsDetail ----

var summaryCols = []string{"c1", "c2", "c3", "avg", "tt", "pt", "ct", "cost", "us", "ttfb", "uptot"}
var byModelCols = []string{"model", "cnt", "avg", "tt", "pt", "ct", "cost", "us", "up"}
var byProjCols = []string{"id", "name", "code", "cnt", "avg", "tt", "pt", "ct", "cost", "us", "up"}
var byVKCols = []string{"id", "name", "prefix", "cnt", "avg", "tt", "pt", "ct", "cost", "us", "up"}
var dailyCols = []string{"day", "cnt", "err", "tokens", "cost"}
var statusCols = []string{"code", "cnt"}

func summaryRow(total, errc, cache int, nonNil bool) []any {
	if !nonNil {
		return []any{total, errc, cache, nil, nil, nil, nil, nil, nil, nil, nil}
	}
	return []any{total, errc, cache, fp(12.5), i64(300), i64(100), i64(200), fp(1.5), fp(2), fp(3), fp(4)}
}

func TestGetProviderAnalyticsDetail_Happy(t *testing.T) {
	s, m := newMock(t)
	// Each list query returns one good row + one bad row (skipped via scan!=nil).
	m.ExpectQuery(`SELECT COUNT\(\*\), COUNT`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(summaryCols).AddRow(summaryRow(10, 2, 3, true)...))
	m.ExpectQuery(`COALESCE\(NULLIF\(a.routed_model_name`).WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(byModelCols).
			AddRow("gpt-4o", 5, fp(1), i64(1), i64(1), i64(1), fp(1), fp(1), fp(1)).
			AddRow("bad", "not-an-int", nil, nil, nil, nil, nil, nil, nil)) // cnt bad → skip
	m.ExpectQuery(`INNER JOIN "Project"`).WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(byProjCols).
			AddRow("p1", sp("Proj"), sp("PC"), 3, fp(1), i64(1), i64(1), i64(1), fp(1), fp(1), fp(1)).
			AddRow("p2", nil, nil, 1, nil, nil, nil, nil, nil, nil, nil)) // nil name/code branch
	m.ExpectQuery(`INNER JOIN "VirtualKey"`).WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(byVKCols).
			AddRow("vk1", sp("Key"), sp("pre"), 2, fp(1), i64(1), i64(1), i64(1), fp(1), fp(1), fp(1)).
			AddRow("vk2", nil, nil, 1, nil, nil, nil, nil, nil, nil, nil)) // nil name/prefix branch
	m.ExpectQuery(`DATE_TRUNC\('day'`).WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(dailyCols).
			AddRow(tNow, 4, 1, i64(10), fp(0.5)).
			AddRow("bad-day", 0, 0, nil, nil)) // day bad type → skip
	m.ExpectQuery(`SELECT a.status_code, COUNT`).WithArgs("prov1").
		WillReturnRows(pgxmock.NewRows(statusCols).AddRow(200, 8).AddRow("bad", 1)) // code bad → skip

	res, err := s.GetProviderAnalyticsDetail(context.Background(), "prov1", nil, nil)
	if err != nil {
		t.Fatalf("GetProviderAnalyticsDetail: %v", err)
	}
	if res.Summary["totalRequests"] != 10 || res.Summary["errorRate"].(float64) == 0 {
		t.Fatalf("summary wrong: %+v", res.Summary)
	}
	if len(res.ByModel) != 1 || len(res.ByProject) != 2 || len(res.ByVirtualKey) != 2 || len(res.Daily) != 1 || len(res.ByStatus) != 1 {
		t.Fatalf("list lengths wrong: model=%d proj=%d vk=%d daily=%d status=%d",
			len(res.ByModel), len(res.ByProject), len(res.ByVirtualKey), len(res.Daily), len(res.ByStatus))
	}
	// Project p2 nil name/code → nil in map.
	if res.ByProject[1]["projectName"] != nil || res.ByVirtualKey[1]["name"] != nil {
		t.Fatalf("nil name/code not propagated: %+v %+v", res.ByProject[1], res.ByVirtualKey[1])
	}
}

func TestGetProviderAnalyticsDetail_SummaryZeroAndTimeClause(t *testing.T) {
	// totalCount=0 → errorRate/cacheHitRate stay 0; nil pointers → df/di return 0.
	// start+end set → time clause both branches.
	s, m := newMock(t)
	start, end := tNow.Add(-time.Hour), tNow
	m.ExpectQuery(`SELECT COUNT\(\*\), COUNT`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(summaryCols).AddRow(summaryRow(0, 0, 0, false)...))
	m.ExpectQuery(`COALESCE\(NULLIF\(a.routed_model_name`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(byModelCols))
	m.ExpectQuery(`INNER JOIN "Project"`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(byProjCols))
	m.ExpectQuery(`INNER JOIN "VirtualKey"`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(byVKCols))
	m.ExpectQuery(`DATE_TRUNC\('day'`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(dailyCols))
	m.ExpectQuery(`SELECT a.status_code, COUNT`).WithArgs("prov1", start, end).WillReturnRows(pgxmock.NewRows(statusCols))
	res, err := s.GetProviderAnalyticsDetail(context.Background(), "prov1", &start, &end)
	if err != nil || res.Summary["errorRate"].(float64) != 0 || res.Summary["avgLatencyMs"].(float64) != 0 {
		t.Fatalf("zero summary: %+v %v", res.Summary, err)
	}
}

func TestGetProviderAnalyticsDetail_QueryErrors(t *testing.T) {
	type stage struct {
		name string
		// re of the query that should error; everything before returns empty.
		errRe string
		setup func(m pgxmock.PgxPoolIface)
	}
	emptySummary := func(m pgxmock.PgxPoolIface) {
		m.ExpectQuery(`SELECT COUNT\(\*\), COUNT`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(summaryCols).AddRow(summaryRow(1, 0, 0, false)...))
	}
	emptyByModel := func(m pgxmock.PgxPoolIface) {
		m.ExpectQuery(`COALESCE\(NULLIF\(a.routed_model_name`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(byModelCols))
	}
	emptyByProj := func(m pgxmock.PgxPoolIface) {
		m.ExpectQuery(`INNER JOIN "Project"`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(byProjCols))
	}
	emptyByVK := func(m pgxmock.PgxPoolIface) {
		m.ExpectQuery(`INNER JOIN "VirtualKey"`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(byVKCols))
	}
	emptyDaily := func(m pgxmock.PgxPoolIface) {
		m.ExpectQuery(`DATE_TRUNC\('day'`).WithArgs("prov1").WillReturnRows(pgxmock.NewRows(dailyCols))
	}

	cases := []stage{
		{"summary", `SELECT COUNT\(\*\), COUNT`, func(m pgxmock.PgxPoolIface) {}},
		{"byModel", `COALESCE\(NULLIF\(a.routed_model_name`, emptySummary},
		{"byProject", `INNER JOIN "Project"`, func(m pgxmock.PgxPoolIface) { emptySummary(m); emptyByModel(m) }},
		{"byVK", `INNER JOIN "VirtualKey"`, func(m pgxmock.PgxPoolIface) { emptySummary(m); emptyByModel(m); emptyByProj(m) }},
		{"daily", `DATE_TRUNC\('day'`, func(m pgxmock.PgxPoolIface) { emptySummary(m); emptyByModel(m); emptyByProj(m); emptyByVK(m) }},
		{"status", `SELECT a.status_code, COUNT`, func(m pgxmock.PgxPoolIface) {
			emptySummary(m)
			emptyByModel(m)
			emptyByProj(m)
			emptyByVK(m)
			emptyDaily(m)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMock(t)
			tc.setup(m)
			m.ExpectQuery(tc.errRe).WithArgs("prov1").WillReturnError(errors.New("boom"))
			if _, err := s.GetProviderAnalyticsDetail(context.Background(), "prov1", nil, nil); err == nil {
				t.Fatalf("%s query error must surface", tc.name)
			}
		})
	}
}
