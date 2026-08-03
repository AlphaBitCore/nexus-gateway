package cachelayer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// expectVKNotFound registers one "row absent" response for a VK hash.
// pgxmock fails any Query beyond the registered expectations, so the
// expectation count is the assertion: it pins how many database
// round-trips the code under test is allowed to make.
func expectVKNotFound(mock pgxmock.PgxPoolIface, hash string) {
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(hash).
		WillReturnError(pgx.ErrNoRows)
}

// TestNewLayer_NegativeCacheCapacityFailureIsFatal pins the named failure
// mode: an unbuildable negative cache must abort construction rather than
// hand back a Layer whose first virtual-key lookup dereferences nil.
func TestNewLayer_NegativeCacheCapacityFailureIsFatal(t *testing.T) {
	orig := negVKCapacity
	negVKCapacity = 0
	defer func() { negVKCapacity = orig }()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	l, err := NewWithPool(store.NewWithPgxPool(mock), mock, discardLogger(), Config{})
	if l != nil {
		t.Errorf("want nil Layer on construction failure, got %#v", l)
	}
	if err == nil || !strings.Contains(err.Error(), "build vk negative cache") {
		t.Fatalf("want a wrapped 'build vk negative cache' error, got %v", err)
	}
}

func TestGetVirtualKeyByHash_UnknownKeyIsNegativelyCached(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: time.Minute})
	expectVKNotFound(mock, "h-ghost")

	for i := range 5 {
		_, err := l.GetVirtualKeyByHash(context.Background(), "h-ghost")
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("lookup %d: want pgx.ErrNoRows (vkauth's keyring loop switches on it), got %v", i, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
	if got := l.Stats().NegativeVirtualKeysSize; got != 1 {
		t.Errorf("NegativeVirtualKeysSize = %d, want 1", got)
	}
}

func TestGetVirtualKeyByHash_NegativeClearedByInvalidate(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: time.Minute})
	expectVKNotFound(mock, "h-new")
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-new"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("seed negative: %v", err)
	}

	// An admin creating the key reaches the gateway as a Hub `virtual_keys`
	// push carrying its hash. The negative entry must not outlive it.
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-new").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "h-new")...))
	l.InvalidateVirtualKeys("h-new")

	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-new")
	if err != nil {
		t.Fatalf("after invalidate the newly created key must resolve, got %v", err)
	}
	if vk.ID != "vk-1" {
		t.Errorf("vk.ID = %q, want vk-1", vk.ID)
	}
	if got := l.Stats().NegativeVirtualKeysSize; got != 0 {
		t.Errorf("NegativeVirtualKeysSize = %d, want 0 after invalidate", got)
	}
}

func TestGetVirtualKeyByHash_NegativeExpiresAtVKTTL(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: 30 * time.Second})
	base := time.Unix(1_700_000_000, 0)
	clock := base
	l.now = func() time.Time { return clock }

	expectVKNotFound(mock, "h-later")
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-later"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("seed negative: %v", err)
	}

	// Still inside the window: no database round-trip is permitted.
	clock = base.Add(29 * time.Second)
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-later"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("within TTL: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("within TTL must not re-query: %v", err)
	}

	// Past the window the key is re-read, so a key created in the meantime
	// is honoured without waiting for an invalidation push.
	clock = base.Add(31 * time.Second)
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-later").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-late", "h-later")...))
	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-later")
	if err != nil {
		t.Fatalf("past TTL the key must be re-read, got %v", err)
	}
	if vk.ID != "vk-late" {
		t.Errorf("vk.ID = %q, want vk-late", vk.ID)
	}
}

// TestGetVirtualKeyByHash_NegativeUsesDefaultVKTTL guards the defaulting
// path. Config{} leaves VKTTL zero, and newLayer must substitute the 30s
// default BEFORE copying it to the negative cache — a zero negative TTL
// would mean "never expires", so a key created later would stay invisible
// until an invalidation push happened to arrive.
func TestGetVirtualKeyByHash_NegativeUsesDefaultVKTTL(t *testing.T) {
	mock, l := newMockLayer(t, Config{})
	base := time.Unix(1_700_000_000, 0)
	clock := base
	l.now = func() time.Time { return clock }

	expectVKNotFound(mock, "h-default")
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-default"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("seed negative: %v", err)
	}

	// One second past the 30s default: the negative must have expired, so
	// the row is re-read rather than the absence being served forever.
	clock = base.Add(31 * time.Second)
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-default").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-def", "h-default")...))
	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-default")
	if err != nil {
		t.Fatalf("default TTL did not bound the negative entry: %v", err)
	}
	if vk.ID != "vk-def" {
		t.Errorf("vk.ID = %q, want vk-def", vk.ID)
	}
}

func TestGetVirtualKeyByHash_TransientErrorIsNotNegativelyCached(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: time.Minute})
	boom := errors.New("connection reset by peer")
	mock.ExpectQuery(`FROM "VirtualKey"`).WithArgs("h-flap").WillReturnError(boom)
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-flap"); !errors.Is(err, boom) {
		t.Fatalf("first lookup: want the transport error, got %v", err)
	}
	if got := l.Stats().NegativeVirtualKeysSize; got != 0 {
		t.Fatalf("a transport failure must not be cached as absence; size = %d", got)
	}

	// The very next request retries and sees the real row.
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-flap").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-flap", "h-flap")...))
	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-flap")
	if err != nil {
		t.Fatalf("retry after transport error: %v", err)
	}
	if vk.ID != "vk-flap" {
		t.Errorf("vk.ID = %q, want vk-flap", vk.ID)
	}
}

func TestPurgeVirtualKeys_ClearsNegativeEntries(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 8, VKTTL: time.Minute})
	expectVKNotFound(mock, "h-purge")
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-purge"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("seed negative: %v", err)
	}

	// A bulk VK migration arrives as a Hub push with no ids -> full purge.
	l.PurgeVirtualKeys()
	if got := l.Stats().NegativeVirtualKeysSize; got != 0 {
		t.Fatalf("NegativeVirtualKeysSize = %d, want 0 after purge", got)
	}

	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-purge").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-purged", "h-purge")...))
	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-purge")
	if err != nil {
		t.Fatalf("after purge the key must be re-read: %v", err)
	}
	if vk.ID != "vk-purged" {
		t.Errorf("vk.ID = %q, want vk-purged", vk.ID)
	}
}

// TestNegativeVKCache_DoesNotEvictResolvedKeys is the security property that
// justifies a separate arena: an unauthenticated caller spraying distinct
// unknown keys must not be able to push resolved keys out of the positive
// cache and turn their traffic back into database load.
func TestNegativeVKCache_DoesNotEvictResolvedKeys(t *testing.T) {
	mock, l := newMockLayer(t, Config{VKCapacity: 4, VKTTL: time.Minute})
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs("h-good").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-good", "h-good")...))
	if _, err := l.GetVirtualKeyByHash(context.Background(), "h-good"); err != nil {
		t.Fatalf("seed positive: %v", err)
	}

	// Spray far more distinct unknown keys than the positive cache holds.
	for i := range 50 {
		hash := "h-spray-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		expectVKNotFound(mock, hash)
		if _, err := l.GetVirtualKeyByHash(context.Background(), hash); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("spray %d: %v", i, err)
		}
	}

	// The legitimate key is still served from cache — no further
	// expectation is registered, so a re-query would fail here.
	vk, err := l.GetVirtualKeyByHash(context.Background(), "h-good")
	if err != nil {
		t.Fatalf("resolved key was evicted by the unknown-key spray: %v", err)
	}
	if vk.ID != "vk-good" {
		t.Errorf("vk.ID = %q, want vk-good", vk.ID)
	}
}
