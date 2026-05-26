// Pgxmock + miniredis-driven coverage for CredentialCircuitFlushJob.
// Supplements the DB-backed credential_circuit_flush_db_test.go which
// skips without Postgres.

package retention

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

func TestCredentialCircuitFlush_Identity(t *testing.T) {
	j := NewCredentialCircuitFlush(nil, nil, "hub-1", 0, testLogger(), nil)
	if j.ID() != credCircuitFlushJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credCircuitFlushJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != 30*time.Second {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewCredentialCircuitFlush(nil, nil, "hub-1", 90*time.Second, testLogger(), nil)
	if j2.Interval() != 90*time.Second {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestCredentialCircuitFlush_Run_NilRedis(t *testing.T) {
	j := &CredentialCircuitFlushJob{rdb: nil, hubID: "hub-1", logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialCircuitFlush_Run_EmptyDirtySet(t *testing.T) {
	_, rdb := newMiniredisRdb(t)

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Rehydrate runs once on first cycle — empty DB → no UPDATEs.
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "circuitState", "circuitReason", "circuitOpenedAt"}))

	j := &CredentialCircuitFlushJob{
		pool:   mock,
		rdb:    rdb,
		hubID:  "hub-1",
		logger: testLogger(),
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialCircuitFlush_AtomicClaim_EmptySource(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	moved, err := j.atomicClaim(context.Background(), "src-set", "dst-set")
	if err != nil {
		t.Fatalf("atomicClaim: %v", err)
	}
	if moved != nil {
		t.Errorf("expected nil moved, got %v", moved)
	}
}

func TestCredentialCircuitFlush_AtomicClaim_Moves(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mini.SAdd("src-set", "a", "b", "c")
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}

	moved, err := j.atomicClaim(context.Background(), "src-set", "dst-set")
	if err != nil {
		t.Fatalf("atomicClaim: %v", err)
	}
	if len(moved) != 3 {
		t.Errorf("moved = %d, want 3", len(moved))
	}
	dst, _ := mini.SMembers("dst-set")
	if len(dst) != 3 {
		t.Errorf("dst size = %d, want 3", len(dst))
	}
}

func TestCredentialCircuitFlush_ReclaimInFlight_Empty(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reclaimInFlight(context.Background()); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}

func TestCredentialCircuitFlush_ReclaimInFlight_MovesBack(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	inFlightKey := credstate.InFlightSet("hub-1")
	mini.SAdd(inFlightKey, "cred-a", "cred-b")

	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reclaimInFlight(context.Background()); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	dirty, _ := mini.SMembers(credstate.CircuitDirtySet)
	if len(dirty) != 2 {
		t.Errorf("dirty size = %d, want 2", len(dirty))
	}
}

func TestCredentialCircuitFlush_FlushOne_ClosedPath(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Empty hash → CLOSED branch → single UPDATE reset.
	mock.ExpectExec(`UPDATE "Credential"`).WithArgs("cred-1", credstate.CircuitClosed).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.flushOne(context.Background(), "cred-1"); err != nil {
		t.Fatalf("flushOne: %v", err)
	}
}

func TestCredentialCircuitFlush_FlushOne_TransitionPath(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mini.HSet(credstate.CircuitKey("cred-2"), credstate.CircuitFieldState, "open")
	mini.HSet(credstate.CircuitKey("cred-2"), credstate.CircuitFieldOpenReason, "rate_limit")

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-2", "open", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.flushOne(context.Background(), "cred-2"); err != nil {
		t.Fatalf("flushOne: %v", err)
	}
}

func TestCredentialCircuitFlush_RehydrateFromDB_Empty(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "Credential"\s+WHERE "circuitState" != 'closed'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "circuitState", "reason", "circuitOpenedAt", "circuitNextProbeAt"}))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.rehydrateFromDB(context.Background(), credstate.InFlightSet("hub-1")); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
}

func TestCircuitFlushMetrics_NilSafe(t *testing.T) {
	var m *CircuitFlushMetrics // nil
	// Should not panic on nil receiver.
	m.cycle("any")
	m.flushed(5)
	m.reclaimed(3)
	m.rehydrate("any")
	m.setDirty(7)
	m.observe(time.Millisecond)
	m.transition("a", "b")
}
