package audit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// The audit-freshness check production code runs a single
// `SELECT COALESCE(MAX(timestamp), ...) FROM traffic_event WHERE source IN (…)`
// across the WHOLE traffic_event table — there is no way to scope it to
// test-owned rows. Prior versions of this suite worked around that by
// DELETE-ing every ai-gateway/compliance-proxy row, running the assertion,
// then re-INSERT-ing a snapshot. That sweep wiped a developer's dev DB on
// every pre-commit hook and was later gated behind NEXUS_DESTRUCTIVE_TESTS=1
// as a band-aid.
//
// The right fix is to mock the pgx surface. `pool` on the job is now typed
// against the `auditFreshnessQueryer` interface, which `pgxmock` satisfies,
// so we drive Run() with synthetic rows without touching any DB.

// runAuditFreshness builds the job with the supplied mock pool and a
// buffered slog logger. Returns the logger buffer + the metric counter
// snapshot the per-test assertions need.
func runAuditFreshness(t *testing.T, mock pgxmock.PgxPoolIface) (*opsmetrics.Registry, *bytes.Buffer, *AuditFreshnessCheck) {
	t.Helper()
	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// NewAuditFreshnessCheck takes *pgxpool.Pool concretely; pass nil and
	// substitute the mock through the interface field directly. The
	// production constructor only assigns `pool: pool`, no other setup
	// touches the pool, so this preserves every other initialization
	// (counter wiring, threshold/interval clamping).
	j := NewAuditFreshnessCheck(nil, 60*time.Second, 5*time.Minute, opsReg, bufLogger)
	j.pool = mock
	return opsReg, &buf, j
}

func staleFiredTotal(reg *opsmetrics.Registry) float64 {
	var fired float64
	for _, s := range reg.Collect() {
		if s.Name == "audit_pipeline.stale_fired_total" {
			fired += s.Value
		}
	}
	return fired
}

func TestAuditFreshnessCheck_Identity(t *testing.T) {
	job := NewAuditFreshnessCheck(nil, 0, 0, nil, testLogger())
	if job.ID() != "audit-freshness-check" {
		t.Errorf("ID = %q", job.ID())
	}
	if job.Interval() != 60*time.Second {
		t.Errorf("default Interval = %v, want 60s", job.Interval())
	}
	if job.RunOnStart() {
		t.Errorf("RunOnStart = true; expected false so a fresh deploy doesn't fire during warmup")
	}
}

// TestAuditFreshnessCheck_FreshRowDoesNotFire — MAX(timestamp) is recent,
// anyRow=true. Job must NOT log ERROR and must NOT bump the fires counter.
func TestAuditFreshnessCheck_FreshRowDoesNotFire(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	latest := time.Now().UTC()
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"latest", "lag_sec", "any_row"}).
			AddRow(latest, float64(10), true))

	_, buf, j := runAuditFreshness(t, mock)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if strings.Contains(logs, "audit pipeline appears stale") {
		t.Errorf("unexpected stale ERROR on a fresh row; logs:\n%s", logs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditFreshnessCheck_StaleRowFires — lagSec > threshold, anyRow=true.
// Job must log ERROR with event=audit_freshness_stale and Inc fires counter.
func TestAuditFreshnessCheck_StaleRowFires(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// 10 minutes is well past the 5 min threshold.
	latest := time.Now().UTC().Add(-10 * time.Minute)
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"latest", "lag_sec", "any_row"}).
			AddRow(latest, float64(600), true))

	opsReg, buf, j := runAuditFreshness(t, mock)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "audit pipeline appears stale") {
		t.Errorf("expected stale ERROR log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("expected level=ERROR in slog output; got:\n%s", logs)
	}
	if !strings.Contains(logs, "event=audit_freshness_stale") {
		t.Errorf("expected event=audit_freshness_stale; got:\n%s", logs)
	}
	if fired := staleFiredTotal(opsReg); fired != 1 {
		t.Errorf("stale_fired_total = %v, want 1", fired)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditFreshnessCheck_EmptyTableDoesNotFire — anyRow=false. Job must
// NOT log stale ERROR regardless of how stale lagSec looks. Prevents
// freshly seeded local DBs / brand-new prod environments from spamming
// stale alerts before any traffic arrives.
func TestAuditFreshnessCheck_EmptyTableDoesNotFire(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// anyRow=false signals an empty data-plane table; the production
	// COALESCE pushes lagSec into a huge value but the EXISTS guard must
	// short-circuit before any ERROR is emitted.
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"latest", "lag_sec", "any_row"}).
			AddRow(time.Unix(0, 0).UTC(), float64(1e9), false))

	opsReg, buf, j := runAuditFreshness(t, mock)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if strings.Contains(logs, "audit pipeline appears stale") {
		t.Errorf("did not expect stale ERROR on empty table; logs:\n%s", logs)
	}
	if fired := staleFiredTotal(opsReg); fired != 0 {
		t.Errorf("stale_fired_total = %v on empty table, want 0", fired)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditFreshnessCheck_QueryErrorReturned ensures a Postgres-side
// failure surfaces as a non-nil error from Run so the scheduler can
// retry on the next tick (scan failures, NOT chain-break-style data
// signals, are job errors).
func TestAuditFreshnessCheck_QueryErrorReturned(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnError(context.DeadlineExceeded)

	_, _, j := runAuditFreshness(t, mock)
	if err := j.Run(context.Background()); err == nil {
		t.Fatalf("Run: expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}
