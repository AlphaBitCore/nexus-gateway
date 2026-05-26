package shadow

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"

	audit "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
)

// newTestQueue spins up an on-disk, key-encrypted audit.Queue rooted in
// t.TempDir(). Mirrors the helper in audit/queue_extra_test.go so the
// offline fallback tests honour the "tests only touch their own data"
// binding (no cross-test pollution, no shared on-disk path).
func newTestQueue(t *testing.T) *audit.Queue {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	q, err := audit.NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("audit.NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestNewOfflineFallback_DefaultGracePeriod(t *testing.T) {
	q := newTestQueue(t)
	of := NewOfflineFallback(q, 0, silentLogger())
	if of == nil {
		t.Fatal("NewOfflineFallback returned nil")
	}
	want := 7 * 24 * time.Hour
	if of.gracePeriod != want {
		t.Errorf("gracePeriod: got %v, want %v (default)", of.gracePeriod, want)
	}
	if of.queue != q {
		t.Error("queue must be wired through")
	}
}

func TestNewOfflineFallback_NegativeGraceUsesDefault(t *testing.T) {
	// gracePeriod <= 0 must clamp to the 7-day default, including the
	// negative-input arm.
	q := newTestQueue(t)
	of := NewOfflineFallback(q, -1*time.Hour, silentLogger())
	want := 7 * 24 * time.Hour
	if of.gracePeriod != want {
		t.Errorf("gracePeriod: got %v, want %v (negative input clamps to default)", of.gracePeriod, want)
	}
}

func TestNewOfflineFallback_ExplicitGracePeriod(t *testing.T) {
	q := newTestQueue(t)
	of := NewOfflineFallback(q, 2*time.Hour, silentLogger())
	if of.gracePeriod != 2*time.Hour {
		t.Errorf("gracePeriod: got %v, want 2h", of.gracePeriod)
	}
}

func TestOfflineFallback_SaveLoadRoundTrip(t *testing.T) {
	q := newTestQueue(t)
	of := NewOfflineFallback(q, time.Hour, silentLogger())

	snap := &ConfigSnapshot{
		Version:   42,
		FetchedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		InterceptionDomains: []InterceptionDomainDTO{
			{ID: "d-1", HostPattern: "api.openai.com", Enabled: true},
		},
	}
	if err := of.SaveSnapshot(snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	loaded, err := of.LoadCached()
	if err != nil {
		t.Fatalf("LoadCached: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadCached returned nil after a successful save")
	}
	if loaded.Version != snap.Version {
		t.Errorf("Version: got %d, want %d", loaded.Version, snap.Version)
	}
	if len(loaded.InterceptionDomains) != 1 || loaded.InterceptionDomains[0].ID != "d-1" {
		t.Errorf("InterceptionDomains not round-tripped: %+v", loaded.InterceptionDomains)
	}
}

func TestOfflineFallback_LoadCached_EmptyDB(t *testing.T) {
	// Empty config_snapshots table → underlying ErrNoRows → wrapped
	// "load cached config" error per offline.go.
	q := newTestQueue(t)
	of := NewOfflineFallback(q, time.Hour, silentLogger())
	loaded, err := of.LoadCached()
	if err == nil {
		t.Fatal("LoadCached on empty DB must error (sql.ErrNoRows surfaces)")
	}
	if loaded != nil {
		t.Fatalf("on error, snapshot must be nil; got %+v", loaded)
	}
	if !strings.Contains(err.Error(), "load cached config") {
		t.Errorf("error must be wrapped with 'load cached config'; got %v", err)
	}
}

func TestOfflineFallback_LoadCached_UnmarshalError(t *testing.T) {
	// Saving a non-JSON blob via the Queue directly bypasses
	// SaveSnapshot's json.Marshal; LoadCached then hits the
	// json.Unmarshal error branch.
	q := newTestQueue(t)
	if err := q.SaveConfigSnapshot(3, "not-json"); err != nil {
		t.Fatalf("seed bad blob: %v", err)
	}
	of := NewOfflineFallback(q, time.Hour, silentLogger())
	loaded, err := of.LoadCached()
	if err == nil {
		t.Fatal("LoadCached on malformed JSON must error")
	}
	if loaded != nil {
		t.Fatalf("on error, snapshot must be nil; got %+v", loaded)
	}
	if !strings.Contains(err.Error(), "unmarshal cached config") {
		t.Errorf("error must be wrapped with 'unmarshal cached config'; got %v", err)
	}
}

func TestOfflineFallback_LoadCached_VersionStampedFromDB(t *testing.T) {
	// Even when the marshalled snapshot carries a different version,
	// LoadCached stamps it from the row column.
	q := newTestQueue(t)
	body, _ := json.Marshal(&ConfigSnapshot{Version: 1, FetchedAt: time.Now()})
	if err := q.SaveConfigSnapshot(99, string(body)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	of := NewOfflineFallback(q, time.Hour, silentLogger())
	loaded, err := of.LoadCached()
	if err != nil {
		t.Fatalf("LoadCached: %v", err)
	}
	if loaded.Version != 99 {
		t.Errorf("Version: got %d, want 99 (column wins)", loaded.Version)
	}
}

func TestOfflineFallback_SaveSnapshot_QueueError(t *testing.T) {
	// Closing the audit DB forces the underlying SaveConfigSnapshot to
	// fail on the next ExecContext. SaveSnapshot must propagate.
	q := newTestQueue(t)
	_ = q.Close()
	of := NewOfflineFallback(q, time.Hour, silentLogger())
	if err := of.SaveSnapshot(&ConfigSnapshot{Version: 1}); err == nil {
		t.Fatal("SaveSnapshot must error when underlying queue is closed")
	}
}

func TestOfflineFallback_IsStale_ZeroFetchedAt(t *testing.T) {
	of := NewOfflineFallback(newTestQueue(t), time.Hour, silentLogger())
	if !of.IsStale(&ConfigSnapshot{}) {
		t.Fatal("IsStale must be true when FetchedAt is the zero value")
	}
}

func TestOfflineFallback_IsStale_FreshSnapshot(t *testing.T) {
	of := NewOfflineFallback(newTestQueue(t), 1*time.Hour, silentLogger())
	snap := &ConfigSnapshot{FetchedAt: time.Now().Add(-5 * time.Minute)}
	if of.IsStale(snap) {
		t.Fatal("a 5-minute-old snapshot must NOT be stale under 1h grace")
	}
}

func TestOfflineFallback_IsStale_OldSnapshot(t *testing.T) {
	of := NewOfflineFallback(newTestQueue(t), 1*time.Hour, silentLogger())
	snap := &ConfigSnapshot{FetchedAt: time.Now().Add(-2 * time.Hour)}
	if !of.IsStale(snap) {
		t.Fatal("a 2-hour-old snapshot must be stale under 1h grace")
	}
}
