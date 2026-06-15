package expiry

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// ctorLazyPool builds a pgxpool that defers connecting. It is enough for
// constructor-wiring tests that only read getters and never touch the
// database, so they run without a TEST_DATABASE_URL (unlike the DB-backed
// behaviour tests).
func ctorLazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://localhost:5432/unused")
	if err != nil {
		t.Fatalf("build lazy pool: %v", err)
	}
	return p
}

// TestEnrollmentTokenCleanup_Constructor covers NewEnrollmentTokenCleanup,
// which the behaviour tests bypass by building the struct literal directly.
func TestEnrollmentTokenCleanup_Constructor(t *testing.T) {
	p := ctorLazyPool(t)
	defer p.Close()
	j := NewEnrollmentTokenCleanup(store.New(p), 2*time.Hour, testLogger())
	if j.ID() != enrollmentCleanupJobID || j.Interval() != 2*time.Hour {
		t.Errorf("constructor wiring: id=%q interval=%v", j.ID(), j.Interval())
	}
	if j.Name() == "" || j.Description() == "" {
		t.Error("Name/Description should be non-empty")
	}
}

// TestOverrideExpiry_ConstructorWithRegistry covers the reg != nil branch of
// NewOverrideExpiry (the row-failure counter registration) that the Identity
// test leaves uncovered by passing a nil registry.
func TestOverrideExpiry_ConstructorWithRegistry(t *testing.T) {
	p := ctorLazyPool(t)
	defer p.Close()
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	j := NewOverrideExpiry(store.New(p), nil, time.Minute, reg, testLogger())
	if j.ID() != "override-expiry" || j.Interval() != time.Minute {
		t.Errorf("constructor wiring: id=%q interval=%v", j.ID(), j.Interval())
	}
}
