package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
)

// newOfflineCacheForTest opens a fresh on-disk SQLite config cache (the
// SQLCipher driver without a PRAGMA key runs as plain SQLite). Returns the
// cache plus the raw DB handle so tests can backdate updated_at to exercise
// the stale path. Closed on t.Cleanup so the file disappears with the temp
// dir ("tests only touch their own data").
func newOfflineCacheForTest(t *testing.T) (*shadow.Cache, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cfgcache.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c, err := shadow.NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c, db
}

func discardCacheLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCachePersist_SavesOnSuccessfulApply(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	var gotRaw []byte
	inner := func(_ context.Context, raw []byte, _ int64) ([]byte, error) {
		gotRaw = raw
		return nil, nil
	}
	w := cachePersist("hooks", inner, func() *shadow.Cache { return cache }, discardCacheLogger())

	if _, err := w(context.Background(), []byte(`{"hookConfigs":[]}`), 7); err != nil {
		t.Fatalf("wrapped apply: %v", err)
	}
	if string(gotRaw) != `{"hookConfigs":[]}` {
		t.Errorf("inner must receive the raw bytes; got %q", gotRaw)
	}
	cc, err := cache.Load("hooks")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cc == nil {
		t.Fatal("expected a cached entry after a successful apply")
	}
	if string(cc.State) != `{"hookConfigs":[]}` || cc.Version != 7 {
		t.Errorf("cached entry: state=%q version=%d", cc.State, cc.Version)
	}
}

func TestCachePersist_SkipsEmptyPayload(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	w := cachePersist("hooks",
		func(context.Context, []byte, int64) ([]byte, error) { return nil, nil },
		func() *shadow.Cache { return cache }, discardCacheLogger())

	if _, err := w(context.Background(), nil, 1); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cc, _ := cache.Load("hooks")
	if cc != nil {
		t.Errorf("an empty payload must not overwrite the cache; got %+v", cc)
	}
}

func TestCachePersist_SkipsOnInnerError(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	wantErr := errors.New("apply failed")
	w := cachePersist("hooks",
		func(context.Context, []byte, int64) ([]byte, error) { return nil, wantErr },
		func() *shadow.Cache { return cache }, discardCacheLogger())

	if _, err := w(context.Background(), []byte(`{"x":1}`), 1); !errors.Is(err, wantErr) {
		t.Fatalf("inner error must propagate; got %v", err)
	}
	cc, _ := cache.Load("hooks")
	if cc != nil {
		t.Errorf("a failed apply must not be cached; got %+v", cc)
	}
}

func TestCachePersist_NilCacheNoPanic(t *testing.T) {
	w := cachePersist("hooks",
		func(context.Context, []byte, int64) ([]byte, error) { return nil, nil },
		func() *shadow.Cache { return nil }, discardCacheLogger())

	if _, err := w(context.Background(), []byte(`{"x":1}`), 1); err != nil {
		t.Fatalf("apply must succeed even when the cache is not yet open; got %v", err)
	}
}

func TestCachePersist_NoopPayloadDoesNotOverwriteGoodEntry(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	getCache := func() *shadow.Cache { return cache }
	noop := func(context.Context, []byte, int64) ([]byte, error) { return nil, nil }

	// Seed a real value.
	good := cachePersist("hooks", noop, getCache, discardCacheLogger())
	if _, err := good(context.Background(), []byte(`{"hookConfigs":[{"id":"h"}]}`), 5); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	// No-op ticks ({} / null / empty) must NOT overwrite the good entry —
	// otherwise the next offline boot would replay a no-op blob and start
	// with empty policy.
	for _, payload := range [][]byte{[]byte(`{}`), []byte(`null`), {}, nil} {
		w := cachePersist("hooks", noop, getCache, discardCacheLogger())
		if _, err := w(context.Background(), payload, 6); err != nil {
			t.Fatalf("apply %q: %v", payload, err)
		}
	}

	cc, err := cache.Load("hooks")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cc == nil || string(cc.State) != `{"hookConfigs":[{"id":"h"}]}` || cc.Version != 5 {
		t.Fatalf("no-op payloads must not overwrite the last real entry; got %+v", cc)
	}
}

func TestCachePersist_ExplicitEmptyArrayIsCached(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	// An explicit empty array is an authoritative clear, not a no-op — it
	// MUST be cached so an offline boot restores the cleared state.
	w := cachePersist("hooks",
		func(context.Context, []byte, int64) ([]byte, error) { return nil, nil },
		func() *shadow.Cache { return cache }, discardCacheLogger())
	if _, err := w(context.Background(), []byte(`{"hookConfigs":[]}`), 9); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cc, _ := cache.Load("hooks")
	if cc == nil || string(cc.State) != `{"hookConfigs":[]}` || cc.Version != 9 {
		t.Fatalf("explicit empty array must be cached; got %+v", cc)
	}
}

func TestRestoreCachedConfig_AppliesEachKey(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	if err := cache.Save("hooks", json.RawMessage(`{"hookConfigs":[]}`), 3); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save("interception_domains", json.RawMessage(`{"interceptionDomains":[]}`), 4); err != nil {
		t.Fatal(err)
	}

	applied := map[string]string{}
	mk := func(key string) rawApply {
		return func(_ context.Context, raw []byte, _ int64) ([]byte, error) {
			applied[key] = string(raw)
			return nil, nil
		}
	}
	restoreCachedConfig(context.Background(), cache, map[string]rawApply{
		"hooks":                mk("hooks"),
		"interception_domains": mk("interception_domains"),
	}, discardCacheLogger())

	if applied["hooks"] != `{"hookConfigs":[]}` {
		t.Errorf("hooks not restored: %q", applied["hooks"])
	}
	if applied["interception_domains"] != `{"interceptionDomains":[]}` {
		t.Errorf("interception_domains not restored: %q", applied["interception_domains"])
	}
}

func TestRestoreCachedConfig_NilAndEmptyAreNoOps(t *testing.T) {
	// nil cache must not panic.
	restoreCachedConfig(context.Background(), nil, map[string]rawApply{}, discardCacheLogger())

	// empty cache must invoke no applier.
	cache, _ := newOfflineCacheForTest(t)
	called := false
	restoreCachedConfig(context.Background(), cache, map[string]rawApply{
		"hooks": func(context.Context, []byte, int64) ([]byte, error) { called = true; return nil, nil },
	}, discardCacheLogger())
	if called {
		t.Error("an empty cache must not invoke any applier")
	}
}

func TestRestoreCachedConfig_UnknownKeySkippedAndApplyErrorContinues(t *testing.T) {
	cache, _ := newOfflineCacheForTest(t)
	_ = cache.Save("hooks", json.RawMessage(`{"a":1}`), 1)
	_ = cache.Save("ghost_key", json.RawMessage(`{"b":2}`), 1) // no registered applier
	_ = cache.Save("payload_capture", json.RawMessage(`{"c":3}`), 1)

	applied := map[string]bool{}
	restoreCachedConfig(context.Background(), cache, map[string]rawApply{
		// errors — must not abort the whole restore
		"hooks": func(context.Context, []byte, int64) ([]byte, error) {
			applied["hooks"] = true
			return nil, errors.New("boom")
		},
		"payload_capture": func(context.Context, []byte, int64) ([]byte, error) {
			applied["payload_capture"] = true
			return nil, nil
		},
	}, discardCacheLogger())

	if !applied["hooks"] {
		t.Error("hooks applier should have been invoked even though it errors")
	}
	if !applied["payload_capture"] {
		t.Error("payload_capture should still apply after a prior applier error")
	}
}

func TestRestoreCachedConfig_StaleEntryStillAppliedWithWarning(t *testing.T) {
	cache, db := newOfflineCacheForTest(t)
	if err := cache.Save("hooks", json.RawMessage(`{"a":1}`), 1); err != nil {
		t.Fatal(err)
	}
	// Backdate updated_at well past the grace period.
	old := time.Now().UTC().Add(-(configCacheStaleAfter + 48*time.Hour)).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE config_cache SET updated_at = ? WHERE key = 'hooks'`, old); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	applied := false
	restoreCachedConfig(context.Background(), cache, map[string]rawApply{
		"hooks": func(context.Context, []byte, int64) ([]byte, error) { applied = true; return nil, nil },
	}, logger)

	if !applied {
		t.Error("a stale entry must STILL be applied — staleness fails open, never closed")
	}
	if !strings.Contains(buf.String(), "stale") {
		t.Errorf("expected a stale-config warning; log was %q", buf.String())
	}
}

func TestOpenAndRestoreConfigCache_PublishesCacheAndReplays(t *testing.T) {
	// Pre-populate a cache on the DB, then run the boot-time open+restore:
	// the atomic pointer must be published (so the loader's persist wrappers
	// start mirroring) and the cached entry replayed through its applier.
	seed, db := newOfflineCacheForTest(t)
	if err := seed.Save("hooks", json.RawMessage(`{"a":1}`), 3); err != nil {
		t.Fatal(err)
	}

	var ptr atomic.Pointer[shadow.Cache]
	applied := false
	openAndRestoreConfigCache(context.Background(), db, &ptr, map[string]rawApply{
		"hooks": func(_ context.Context, raw []byte, ver int64) ([]byte, error) {
			if string(raw) != `{"a":1}` || ver != 3 {
				t.Errorf("applier got raw=%q ver=%d, want the cached entry", raw, ver)
			}
			applied = true
			return nil, nil
		},
	}, discardCacheLogger())

	if ptr.Load() == nil {
		t.Error("config cache must be published for the persist wrappers")
	}
	if !applied {
		t.Error("cached entry must be replayed at boot")
	}
}

func TestOpenAndRestoreConfigCache_OpenFailureDisablesRestore(t *testing.T) {
	// A closed DB means the cache cannot open: restore is disabled (pointer
	// stays nil) but boot continues — offline restore is best-effort.
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var ptr atomic.Pointer[shadow.Cache]
	openAndRestoreConfigCache(context.Background(), db, &ptr, map[string]rawApply{
		"hooks": func(context.Context, []byte, int64) ([]byte, error) {
			t.Error("no applier may run when the cache failed to open")
			return nil, nil
		},
	}, discardCacheLogger())

	if ptr.Load() != nil {
		t.Error("failed open must leave the cache pointer nil")
	}
}
