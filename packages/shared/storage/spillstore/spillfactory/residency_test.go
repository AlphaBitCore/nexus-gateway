package spillfactory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// statStore is a SpillStore whose only interesting behaviour is Stat.
type statStore struct {
	spillstore.SpillStore
	stats spillstore.Stats
	err   error
}

func (s statStore) Backend() string { return "localfs" }
func (s statStore) Stat(context.Context) (spillstore.Stats, error) {
	return s.stats, s.err
}

func localfsCfg() FactoryConfig {
	cfg := FactoryConfig{Enabled: true, Backend: "localfs"}
	cfg.Localfs.Root = "/tmp/nexus-spill"
	return cfg
}

// The distinction this whole type exists for: "we could not look" must not read as
// "the store is empty". A zero-valued Residency struct would report objectCount 0
// on a store holding a retention window's worth of bodies.
func TestDescribeWithResidency_FailedMeasurementLeavesResidencyNil(t *testing.T) {
	st := statStore{err: errors.New("permission denied on /tmp/nexus-spill")}
	a := DescribeWithResidency(context.Background(), localfsCfg(), st)

	if a.Residency != nil {
		t.Fatalf("a failed Stat must leave Residency nil, got %+v", a.Residency)
	}
	if !strings.Contains(a.Effect, "Residency could not be measured") ||
		!strings.Contains(a.Effect, "permission denied") {
		t.Errorf("the reason must reach the operator through Effect, got: %q", a.Effect)
	}
	if !a.Configured {
		t.Error("a Stat failure must not change where bodies go")
	}
}

// A measurable store reports its contents, with the age window formatted for a
// human reading an introspection response.
func TestDescribeWithResidency_ReportsContents(t *testing.T) {
	oldest := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	newest := time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC)
	st := statStore{stats: spillstore.Stats{
		Backend: "localfs", ObjectCount: 214, TotalBytes: 5 << 20,
		OldestAt: oldest, NewestAt: newest,
	}}
	a := DescribeWithResidency(context.Background(), localfsCfg(), st)

	if a.Residency == nil {
		t.Fatal("want residency for a measurable store")
	}
	if a.Residency.ObjectCount != 214 || a.Residency.TotalBytes != 5<<20 {
		t.Errorf("counts = %d/%d; want 214/%d", a.Residency.ObjectCount, a.Residency.TotalBytes, 5<<20)
	}
	if a.Residency.OldestAt != "2026-07-20T08:30:00Z" || a.Residency.NewestAt != "2026-07-28T06:00:00Z" {
		t.Errorf("age window = %q..%q", a.Residency.OldestAt, a.Residency.NewestAt)
	}
	if a.Residency.Truncated {
		t.Error("a complete scan must not report Truncated")
	}
}

// Partial numbers plus an error: both must survive, because a truncated count with
// its reason attached is useful and a truncated count alone is misleading.
func TestDescribeWithResidency_PartialNumbersKeepTheirReason(t *testing.T) {
	st := statStore{
		stats: spillstore.Stats{ObjectCount: 50_000, TotalBytes: 1 << 30, Truncated: true, ScanLimit: 50_000},
		err:   context.DeadlineExceeded,
	}
	a := DescribeWithResidency(context.Background(), localfsCfg(), st)

	if a.Residency == nil {
		t.Fatal("partial numbers must still be reported")
	}
	if !a.Residency.Truncated || a.Residency.ScanLimit != 50_000 {
		t.Errorf("the bound must be reported: %+v", a.Residency)
	}
	if !strings.Contains(a.Residency.Error, context.DeadlineExceeded.Error()) {
		t.Errorf("Error = %q; want the deadline reason", a.Residency.Error)
	}
}

// No backend: there is nothing to measure and nothing to claim.
func TestDescribeWithResidency_NoStoreIsUnchanged(t *testing.T) {
	a := DescribeWithResidency(context.Background(), FactoryConfig{}, nil)
	if a.Configured || a.Residency != nil {
		t.Errorf("no backend must stay unconfigured with no residency: %+v", a)
	}
}
