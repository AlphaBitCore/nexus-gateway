package drift

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// fakeSmartGroupStore satisfies the smartGroupStore interface for unit
// tests. Recorded writes are exposed so tests assert what landed in the
// cache without standing up Postgres.
type fakeSmartGroupStore struct {
	groups  []store.SmartGroupSnapshot
	devices []struct {
		ID  string
		Dev device.Device
	}
	listErr    error
	devicesErr error
	writeErr   error
	// (group_id, sorted_device_ids) captured per ReplaceSmartGroupCache call
	writes map[string][]string
	// Count of expired-membership evictions reported by the fake on each
	// call. Tests set this directly to simulate "N rows deleted".
	evictionsReported int
	evictionsErr      error
	evictCalls        int
}

func (f *fakeSmartGroupStore) EvictExpiredMemberships(_ context.Context) (int, error) {
	f.evictCalls++
	return f.evictionsReported, f.evictionsErr
}

func (f *fakeSmartGroupStore) ListSmartGroups(_ context.Context) ([]store.SmartGroupSnapshot, error) {
	return f.groups, f.listErr
}

func (f *fakeSmartGroupStore) LoadDevicesForSmartGroupEval(_ context.Context) ([]struct {
	ID  string
	Dev device.Device
}, error) {
	return f.devices, f.devicesErr
}

func (f *fakeSmartGroupStore) ReplaceSmartGroupCache(_ context.Context, groupID string, deviceIDs []string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.writes == nil {
		f.writes = map[string][]string{}
	}
	cp := make([]string, len(deviceIDs))
	copy(cp, deviceIDs)
	sort.Strings(cp)
	f.writes[groupID] = cp
	return nil
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSmartGroupRecompute_HappyPath(t *testing.T) {
	fake := &fakeSmartGroupStore{
		groups: []store.SmartGroupSnapshot{
			{ID: "g-darwin", Predicate: device.Predicate{All: []device.Leaf{
				{Field: "os", Op: "eq", Value: "darwin"},
			}}},
			{ID: "g-singapore", Predicate: device.Predicate{All: []device.Leaf{
				{Field: "primaryIp", Op: "cidr", Value: "10.32.0.0/16"},
			}}},
		},
		devices: []struct {
			ID  string
			Dev device.Device
		}{
			{ID: "dev-mac-sg", Dev: device.Device{OS: "darwin", PrimaryIP: "10.32.4.1"}},
			{ID: "dev-mac-fra", Dev: device.Device{OS: "darwin", PrimaryIP: "10.33.4.1"}},
			{ID: "dev-linux-sg", Dev: device.Device{OS: "linux", PrimaryIP: "10.32.4.2"}},
		},
	}
	j := NewSmartGroupRecompute(fake, time.Second, nilLogger())

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fake.writes["g-darwin"]; len(got) != 2 || got[0] != "dev-mac-fra" || got[1] != "dev-mac-sg" {
		t.Errorf("g-darwin members = %v, want [dev-mac-fra dev-mac-sg]", got)
	}
	if got := fake.writes["g-singapore"]; len(got) != 2 || got[0] != "dev-linux-sg" || got[1] != "dev-mac-sg" {
		t.Errorf("g-singapore members = %v, want [dev-linux-sg dev-mac-sg]", got)
	}
}

func TestSmartGroupRecompute_PredicateErrorSkipsGroupOnly(t *testing.T) {
	fake := &fakeSmartGroupStore{
		groups: []store.SmartGroupSnapshot{
			{ID: "g-bad", Predicate: device.Predicate{All: []device.Leaf{
				{Field: "notARealField", Op: "eq", Value: "x"},
			}}},
			{ID: "g-good", Predicate: device.Predicate{All: []device.Leaf{
				{Field: "os", Op: "eq", Value: "darwin"},
			}}},
		},
		devices: []struct {
			ID  string
			Dev device.Device
		}{
			{ID: "dev-mac", Dev: device.Device{OS: "darwin"}},
		},
	}
	j := NewSmartGroupRecompute(fake, time.Second, nilLogger())

	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected joined error from g-bad")
	}
	// g-good must still have been recomputed.
	if got := fake.writes["g-good"]; len(got) != 1 || got[0] != "dev-mac" {
		t.Errorf("g-good members = %v, want [dev-mac]", got)
	}
	// g-bad must NOT have written cache rows (stale > wrong).
	if _, present := fake.writes["g-bad"]; present {
		t.Errorf("g-bad should not have been written on predicate error; got %v", fake.writes["g-bad"])
	}
}

func TestSmartGroupRecompute_ListErrorAborts(t *testing.T) {
	fake := &fakeSmartGroupStore{
		groups:  nil,
		listErr: errors.New("db down"),
	}
	j := NewSmartGroupRecompute(fake, time.Second, nilLogger())
	if err := j.Run(context.Background()); err == nil {
		t.Error("expected error when ListSmartGroups fails with empty result")
	}
}

func TestSmartGroupRecompute_EmptyFleetNoLogNoise(t *testing.T) {
	// No smart groups = silent no-op (common case in production
	// fleets that haven't started using S2 yet).
	fake := &fakeSmartGroupStore{}
	j := NewSmartGroupRecompute(fake, time.Second, nilLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Errorf("empty fleet should not error, got %v", err)
	}
	if len(fake.writes) != 0 {
		t.Errorf("expected no cache writes, got %v", fake.writes)
	}
}

func TestSmartGroupRecompute_DefaultInterval(t *testing.T) {
	j := NewSmartGroupRecompute(&fakeSmartGroupStore{}, 0, nilLogger())
	if j.Interval() != 60*time.Second {
		t.Errorf("Interval=%v, want 60s", j.Interval())
	}
}

// TestSmartGroupRecompute_EvictExpiredMembershipsFirst pins the invariant that
// expired static memberships are evicted on every tick before the smart-group
// recompute reads the live fleet. Even when no smart groups exist, the eviction
// sweep still runs.
func TestSmartGroupRecompute_EvictExpiredMembershipsFirst(t *testing.T) {
	fake := &fakeSmartGroupStore{evictionsReported: 3}
	j := NewSmartGroupRecompute(fake, time.Second, nilLogger())
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.evictCalls != 1 {
		t.Errorf("EvictExpiredMemberships called %d times, want 1", fake.evictCalls)
	}
}
