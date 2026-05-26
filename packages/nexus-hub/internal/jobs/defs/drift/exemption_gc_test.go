package drift

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
)

// fakeRow is a tiny pgx.Row stand-in that lets us inject the COUNT(*) result
// or a scan error without spinning a live Postgres. pgxmock would be heavier
// than we need here.
type fakeRow struct {
	count int64
	err   error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("fakeRow: expected single dest")
	}
	p, ok := dest[0].(*int64)
	if !ok {
		return errors.New("fakeRow: dest is not *int64")
	}
	*p = r.count
	return nil
}

// fakeExemptionQuerier records the most recent QueryRow args + returns the
// programmed row. Used to assert the GC fires the count query with the
// expected sliding window.
type fakeExemptionQuerier struct {
	calls   int
	lastSQL string
	lastNow time.Time
	lastLow time.Time
	row     *fakeRow
}

func (f *fakeExemptionQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.calls++
	f.lastSQL = sql
	if len(args) == 2 {
		if t, ok := args[0].(time.Time); ok {
			f.lastNow = t
		}
		if t, ok := args[1].(time.Time); ok {
			f.lastLow = t
		}
	}
	if f.row == nil {
		return &fakeRow{}
	}
	return f.row
}

type fakeExemptionUpdater struct {
	lastReq *manager.UpdateConfigRequest
	err     error
	calls   int
}

func (f *fakeExemptionUpdater) UpdateConfig(_ context.Context, req manager.UpdateConfigRequest) (*manager.UpdateConfigResponse, error) {
	f.calls++
	f.lastReq = &req
	if f.err != nil {
		return nil, f.err
	}
	return &manager.UpdateConfigResponse{OK: true, Version: 2}, nil
}

func TestExemptionGC_Identity(t *testing.T) {
	j := NewExemptionGC(&fakeExemptionQuerier{}, &fakeExemptionUpdater{}, 5*time.Minute, testLogger())
	if j.ID() != "exemption-gc" {
		t.Errorf("ID = %q, want exemption-gc", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", j.Interval())
	}
}

func TestExemptionGC_IntervalDefault(t *testing.T) {
	j := NewExemptionGC(&fakeExemptionQuerier{}, &fakeExemptionUpdater{}, 0, testLogger())
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m default", j.Interval())
	}
}

// TestExemptionGC_NoExpiredIsNoOp — the steady-state hot path. Zero
// recently-expired grants must NOT fire an invalidate, otherwise the GC
// reverts to a per-tick noisy broadcast.
func TestExemptionGC_NoExpiredIsNoOp(t *testing.T) {
	q := &fakeExemptionQuerier{row: &fakeRow{count: 0}}
	up := &fakeExemptionUpdater{}
	j := NewExemptionGC(q, up, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if up.calls != 0 {
		t.Errorf("update calls = %d, want 0 when nothing expired", up.calls)
	}
	if q.calls != 1 {
		t.Errorf("query calls = %d, want 1", q.calls)
	}
}

// TestExemptionGC_RecentlyExpiredFiresInvalidate — at least one grant
// expired within the previous interval window, so the GC fires a Cat B
// invalidate (State=nil, action="gc") at compliance-proxy.exemptions.
func TestExemptionGC_RecentlyExpiredFiresInvalidate(t *testing.T) {
	q := &fakeExemptionQuerier{row: &fakeRow{count: 3}}
	up := &fakeExemptionUpdater{}
	j := NewExemptionGC(q, up, time.Minute, testLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if up.calls != 1 {
		t.Fatalf("update calls = %d, want 1", up.calls)
	}
	if up.lastReq == nil {
		t.Fatal("lastReq is nil")
	}
	if up.lastReq.ThingType != "compliance-proxy" || up.lastReq.ConfigKey != "exemptions" {
		t.Errorf("unexpected target: %s/%s", up.lastReq.ThingType, up.lastReq.ConfigKey)
	}
	if up.lastReq.State != nil {
		t.Errorf("State must be nil for Cat B invalidate; got %+v", up.lastReq.State)
	}
	if up.lastReq.Action != "gc" {
		t.Errorf("Action = %q, want gc", up.lastReq.Action)
	}
}

// TestExemptionGC_WindowMatchesInterval — the COUNT(*) lower bound must
// equal `now - interval` so the steady-state set stays bounded. This
// pins the sliding-window invariant.
func TestExemptionGC_WindowMatchesInterval(t *testing.T) {
	q := &fakeExemptionQuerier{row: &fakeRow{count: 0}}
	up := &fakeExemptionUpdater{}
	interval := 7 * time.Minute
	j := NewExemptionGC(q, up, interval, testLogger())
	before := time.Now().UTC()
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := time.Now().UTC()
	if q.lastNow.Before(before) || q.lastNow.After(after) {
		t.Errorf("lastNow %v outside [%v, %v]", q.lastNow, before, after)
	}
	gap := q.lastNow.Sub(q.lastLow)
	if gap != interval {
		t.Errorf("window gap = %v, want %v", gap, interval)
	}
}

// TestExemptionGC_QueryErrorPropagates — a transient DB error during
// the COUNT must surface with attribution prefix.
func TestExemptionGC_QueryErrorPropagates(t *testing.T) {
	sentinel := errors.New("db down")
	q := &fakeExemptionQuerier{row: &fakeRow{err: sentinel}}
	j := NewExemptionGC(q, &fakeExemptionUpdater{}, time.Minute, testLogger())
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}

// TestExemptionGC_UpdateErrorPropagates — when the count is positive
// but the Cat B invalidate UpdateConfig fails, the GC must surface that
// failure to the scheduler so the next tick retries.
func TestExemptionGC_UpdateErrorPropagates(t *testing.T) {
	q := &fakeExemptionQuerier{row: &fakeRow{count: 1}}
	sentinel := errors.New("hub down")
	up := &fakeExemptionUpdater{err: sentinel}
	j := NewExemptionGC(q, up, time.Minute, testLogger())
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}
