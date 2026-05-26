// Mocked unit tests for CredentialHealthRollupJob.
//
// CredentialHealthRollupJob.pool is typed against the narrow
// healthRollupPool interface. pgxmock satisfies it, so each test
// here drives Run() with synthetic SELECT result sets and asserts the
// exact UPDATE the job emits — no Postgres needed and no foreign-row
// risk.

package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// staticThresholds is a thresholdsReader implementation that returns a
// fixed value — keeps tests immune to whatever's in system_metadata.
type staticThresholds struct{ t credstate.Thresholds }

func (s staticThresholds) Thresholds(_ context.Context) credstate.Thresholds { return s.t }

// healthRollupCounts is the shape of the row collect() expects. Fields
// are listed in production-query column order so test cases stay
// readable.
type healthRollupCounts struct {
	credentialID                   string
	shortSamples, shortSuccess     int
	shortAuth, shortRate, short5xx int
	shortTimeout, shortClient      int
	shortLast                      *time.Time
	longSamples, longSuccess       int
}

// expectCollect wires the SELECT FROM traffic_event aggregate to return
// `rows`. The query is matched by the FROM traffic_event substring so a
// minor whitespace edit upstream does not break the test.
func expectCollect(mock pgxmock.PgxPoolIface, rows []healthRollupCounts) {
	cols := []string{
		"credential_id",
		"short_samples", "short_success", "short_auth", "short_rate",
		"short_5xx", "short_timeout", "short_client",
		"short_last",
		"long_samples", "long_success",
	}
	r := pgxmock.NewRows(cols)
	for _, x := range rows {
		r = r.AddRow(
			x.credentialID,
			x.shortSamples, x.shortSuccess, x.shortAuth, x.shortRate,
			x.short5xx, x.shortTimeout, x.shortClient,
			x.shortLast,
			x.longSamples, x.longSuccess,
		)
	}
	mock.ExpectQuery(`FROM\s+traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(r)
}

// expectPriorStatus wires the SELECT FROM Credential prior-status read
// to return `priors` — one row per credential id the job will look up.
func expectPriorStatus(mock pgxmock.PgxPoolIface, priors map[string]string) {
	r := pgxmock.NewRows([]string{"id", "healthStatus", "healthStatusChangedAt"})
	for id, status := range priors {
		var changedAt *time.Time
		r = r.AddRow(id, status, changedAt)
	}
	mock.ExpectQuery(`FROM\s+"Credential"\s+WHERE\s+id\s+=\s+ANY`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(r)
}

// newHealthRollupTestJob wires a CredentialHealthRollupJob backed by the
// supplied pgxmock pool + thresholds. Mirrors the original
// newHealthRollupJob helper.
func newHealthRollupTestJob(t *testing.T, mock pgxmock.PgxPoolIface, thr credstate.Thresholds) *CredentialHealthRollupJob {
	t.Helper()
	// NewCredentialHealthRollup demands *pgxpool.Pool concretely; pass nil
	// and rewire the field through the interface.
	j := NewCredentialHealthRollup(nil, staticThresholds{t: thr}, 5*time.Minute, testLogger(), nil)
	j.pool = mock
	return j
}

// readHealthStatusUpdate captures the (id, status) arrays the job
// passed into batchUpdate so per-test assertions can pin the exact
// transition without round-tripping the DB.
type healthUpdateCapture struct {
	ids      []string
	statuses []string
}

// expectBatchUpdateCapture is expectBatchUpdate plus a callback that
// records the args. pgxmock's WithArgs supports custom matchers, which
// we use here to capture by reference.
func expectBatchUpdateCapture(mock pgxmock.PgxPoolIface, cap *healthUpdateCapture) {
	mock.ExpectExec(`UPDATE\s+"Credential"`).
		WithArgs(
			argCapturer{cap: cap, kind: "ids"},
			argCapturer{cap: cap, kind: "statuses"},
			pgxmock.AnyArg(), // rate5m
			pgxmock.AnyArg(), // rate1h
			pgxmock.AnyArg(), // samples
			pgxmock.AnyArg(), // dominants
			pgxmock.AnyArg(), // trends
			pgxmock.AnyArg(), // changes
			pgxmock.AnyArg(), // now
		).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
}

// argCapturer is a pgxmock matcher that snapshots the supplied
// argument into a healthUpdateCapture. Always matches (Match returns
// true) — we just want the side-effect snapshot.
type argCapturer struct {
	cap  *healthUpdateCapture
	kind string
}

func (a argCapturer) Match(v interface{}) bool {
	switch a.kind {
	case "ids":
		if arr, ok := v.([]string); ok {
			a.cap.ids = arr
		}
	case "statuses":
		if arr, ok := v.([]string); ok {
			a.cap.statuses = arr
		}
	}
	return true
}

// TestHealthRollup_HealthyClassification — 20 samples, all 200 → healthy.
func TestHealthRollup_HealthyClassification(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	credID := "cred-healthy-1"
	expectCollect(mock, []healthRollupCounts{{
		credentialID: credID,
		shortSamples: 20, shortSuccess: 20,
		longSamples: 20, longSuccess: 20,
	}})
	expectPriorStatus(mock, map[string]string{credID: credstate.HealthUnknown})
	cap := &healthUpdateCapture{}
	expectBatchUpdateCapture(mock, cap)

	j := newHealthRollupTestJob(t, mock, credstate.DefaultThresholds)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(cap.ids) != 1 || cap.ids[0] != credID {
		t.Errorf("ids = %v, want [%q]", cap.ids, credID)
	}
	if len(cap.statuses) != 1 || cap.statuses[0] != credstate.HealthHealthy {
		t.Errorf("status = %v, want [%q]", cap.statuses, credstate.HealthHealthy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestHealthRollup_DominantAuthFail — 10 samples, 9 are 401 → unavailable.
func TestHealthRollup_DominantAuthFail(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	credID := "cred-auth-1"
	expectCollect(mock, []healthRollupCounts{{
		credentialID: credID,
		shortSamples: 10, shortSuccess: 1, shortAuth: 9,
		longSamples: 10, longSuccess: 1,
	}})
	expectPriorStatus(mock, map[string]string{credID: credstate.HealthUnknown})
	cap := &healthUpdateCapture{}
	expectBatchUpdateCapture(mock, cap)

	j := newHealthRollupTestJob(t, mock, credstate.DefaultThresholds)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.statuses) != 1 || cap.statuses[0] != credstate.HealthUnavailable {
		t.Errorf("status = %v, want [%q]", cap.statuses, credstate.HealthUnavailable)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestHealthRollup_ExcludesRateLimit — the collect() query already
// strips 429s out of `samples` (see production SQL FILTER clauses), so
// a credential whose only traffic was 429s never appears in the rolled
// slice at all. The mock returns zero rows; Run() must exit on the
// `len(rolled) == 0` short-circuit and emit no priorStatus / batchUpdate
// query.
func TestHealthRollup_ExcludesRateLimit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectCollect(mock, nil)

	j := newHealthRollupTestJob(t, mock, credstate.DefaultThresholds)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations (must have run only collect): %v", err)
	}
}

// TestHealthRollup_CollectingBelowMinSamples — 4 successful samples →
// status = collecting (below the default min of 5).
func TestHealthRollup_CollectingBelowMinSamples(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	credID := "cred-collecting-1"
	expectCollect(mock, []healthRollupCounts{{
		credentialID: credID,
		shortSamples: 4, shortSuccess: 4,
		longSamples: 4, longSuccess: 4,
	}})
	expectPriorStatus(mock, map[string]string{credID: credstate.HealthUnknown})
	cap := &healthUpdateCapture{}
	expectBatchUpdateCapture(mock, cap)

	j := newHealthRollupTestJob(t, mock, credstate.DefaultThresholds)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.statuses) != 1 || cap.statuses[0] != credstate.HealthCollecting {
		t.Errorf("status = %v, want [%q]", cap.statuses, credstate.HealthCollecting)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestHealthRollup_TransitionFromHealthyToDegraded covers the
// statusChanged path. Prior=healthy, current samples are dominated by
// 5xx → status must move off healthy and the batchUpdate `changed`
// flag must be set true.
func TestHealthRollup_TransitionFromHealthyToDegraded(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	credID := "cred-transition-1"
	// 20 samples, only 2 success, 18 × 5xx → ratio 10% → unavailable.
	expectCollect(mock, []healthRollupCounts{{
		credentialID: credID,
		shortSamples: 20, shortSuccess: 2, short5xx: 18,
		longSamples: 20, longSuccess: 2,
	}})
	expectPriorStatus(mock, map[string]string{credID: credstate.HealthHealthy})

	var got healthUpdateCapture
	// capture the `changed` slice through a dedicated matcher
	var changedSlice []bool
	mock.ExpectExec(`UPDATE\s+"Credential"`).
		WithArgs(
			argCapturer{cap: &got, kind: "ids"},
			argCapturer{cap: &got, kind: "statuses"},
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			boolSliceCapturer{out: &changedSlice},
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	j := newHealthRollupTestJob(t, mock, credstate.DefaultThresholds)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.statuses) != 1 {
		t.Fatalf("statuses captured = %v", got.statuses)
	}
	if got.statuses[0] == credstate.HealthHealthy {
		t.Errorf("status still healthy after 90%% 5xx; expected degraded or unavailable")
	}
	if len(changedSlice) != 1 || !changedSlice[0] {
		t.Errorf("changed flag = %v, want [true]", changedSlice)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// boolSliceCapturer is a pgxmock matcher that copies the supplied
// `[]bool` argument into `*out`. Like argCapturer but typed for the
// `changed` parameter.
type boolSliceCapturer struct {
	out *[]bool
}

func (b boolSliceCapturer) Match(v interface{}) bool {
	if arr, ok := v.([]bool); ok {
		*b.out = arr
	}
	return true
}
