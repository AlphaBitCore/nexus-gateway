package configcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

func newDiscardLoggerForTest(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fixtureItem struct {
	Name string
	Cost int
}

func TestSnapshotCache_WithSnapshotLoggerWired(t *testing.T) {
	// Each option must populate the corresponding field. A refactor that
	// drops one would silently lose the logger (and produce default
	// slog.Default destination logs that don't honour service config).
	logger := newDiscardLoggerForTest(t)
	c := NewSnapshotCache[fixtureItem](
		func(context.Context) (map[string]fixtureItem, error) { return nil, nil },
		WithSnapshotLogger(logger),
	)
	if c.log != logger {
		t.Errorf("WithSnapshotLogger not applied: %p vs %p", c.log, logger)
	}
}

func TestSnapshotCache_ReloadIsLoadAlias(t *testing.T) {
	// Reload is documented as "an alias for Load" — pin the contract so
	// it doesn't silently diverge (e.g., someone adds a stale-check that
	// makes Reload short-circuit when Load wouldn't).
	loads := 0
	loader := func(context.Context) (map[string]fixtureItem, error) {
		loads++
		return map[string]fixtureItem{"k": {Name: "v"}}, nil
	}
	c := NewSnapshotCache[fixtureItem](loader)
	_ = c.Load(context.Background())
	_ = c.Reload(context.Background())
	if loads != 2 {
		t.Errorf("Reload should call loader: loads=%d, want 2", loads)
	}
}

func TestSnapshotCache_NilContextErrors(t *testing.T) {
	c := NewSnapshotCache[fixtureItem](
		func(context.Context) (map[string]fixtureItem, error) { return nil, nil },
	)
	//nolint:staticcheck // SA1012: intentionally passing nil ctx to verify rejection branch.
	if err := c.Load(nil); err == nil {
		t.Error("nil context should error")
	}
}

func TestSnapshotCache_LoaderReturnsNilMap_HandledAsEmpty(t *testing.T) {
	// Loader returning nil map (not error) must be coerced to empty map
	// — without this, downstream Get/All/Size would all dereference nil.
	c := NewSnapshotCache[fixtureItem](
		func(context.Context) (map[string]fixtureItem, error) { return nil, nil },
	)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Size() != 0 {
		t.Errorf("Size: %d, want 0", c.Size())
	}
	if got := c.All(); got == nil {
		t.Error("All() should return empty map, not nil")
	}
}

func TestSnapshotCache_ZeroValueGetSafe(t *testing.T) {
	// Defensive guards in Get/All/Size protect against an uninitialized
	// SnapshotCache (zero-value struct, no NewSnapshotCache call). The
	// guards prevent nil-deref if anyone bypasses the constructor.
	var c SnapshotCache[fixtureItem]
	if _, ok := c.Get("missing"); ok {
		t.Error("zero-value Get should return ok=false")
	}
	if got := c.All(); got != nil {
		t.Errorf("zero-value All should return nil, got %+v", got)
	}
	if got := c.Size(); got != 0 {
		t.Errorf("zero-value Size: %d, want 0", got)
	}
}

func TestSnapshotCache_LoadAndGet(t *testing.T) {
	loader := func(ctx context.Context) (map[string]fixtureItem, error) {
		return map[string]fixtureItem{
			"a": {Name: "alpha", Cost: 1},
			"b": {Name: "beta", Cost: 2},
		}, nil
	}
	c := NewSnapshotCache[fixtureItem](loader, WithSnapshotName("test"))
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, ok := c.Get("a"); !ok || got.Name != "alpha" {
		t.Errorf("Get(a): got %+v ok=%v, want alpha", got, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Error("Get(missing): expected ok=false")
	}
	if c.Size() != 2 {
		t.Errorf("Size: got %d, want 2", c.Size())
	}
	if c.Name() != "test" {
		t.Errorf("Name: got %q, want test", c.Name())
	}
}

func TestSnapshotCache_LoadError_KeepsPreviousSnapshot(t *testing.T) {
	var fail atomic.Bool
	loader := func(ctx context.Context) (map[string]fixtureItem, error) {
		if fail.Load() {
			return nil, errors.New("db down")
		}
		return map[string]fixtureItem{"a": {Name: "alpha"}}, nil
	}
	c := NewSnapshotCache[fixtureItem](loader)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("first load: %v", err)
	}
	fail.Store(true)
	if err := c.Load(context.Background()); err == nil {
		t.Fatal("expected load error, got nil")
	}
	if got, ok := c.Get("a"); !ok || got.Name != "alpha" {
		t.Errorf("after failed reload: snapshot lost, got %+v", got)
	}
}

func TestSnapshotCache_AtomicSwap_NoTornReads(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	loader := func(ctx context.Context) (map[string]fixtureItem, error) {
		v := version.Load()
		out := make(map[string]fixtureItem, 100)
		for i := range 100 {
			out[key(i)] = fixtureItem{Name: key(i), Cost: int(v)}
		}
		return out, nil
	}
	c := NewSnapshotCache[fixtureItem](loader)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := c.All()
				// All entries within a single snapshot must share the same Cost.
				var seen int
				for _, v := range snap {
					if seen == 0 {
						seen = v.Cost
					} else if v.Cost != seen {
						t.Errorf("torn snapshot: %d vs %d", v.Cost, seen)
						return
					}
				}
			}
		}()
	}

	for i := range 50 {
		version.Add(1)
		if err := c.Load(context.Background()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestSnapshotCache_OnLoadCallback(t *testing.T) {
	loader := func(ctx context.Context) (map[string]fixtureItem, error) {
		return map[string]fixtureItem{"x": {Name: "x"}, "y": {Name: "y"}}, nil
	}
	var gotName string
	var gotSize int
	c := NewSnapshotCache[fixtureItem](loader,
		WithSnapshotName("widgets"),
		WithSnapshotOnLoad(func(name string, size int) {
			gotName = name
			gotSize = size
		}),
	)
	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotName != "widgets" || gotSize != 2 {
		t.Errorf("onLoad: got name=%q size=%d, want widgets/2", gotName, gotSize)
	}
}

func TestSnapshotCache_NilLoaderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil loader")
		}
	}()
	NewSnapshotCache[fixtureItem](nil)
}

func key(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i/26))
}
