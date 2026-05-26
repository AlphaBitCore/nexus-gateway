package shadow

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// newCacheDB opens a fresh on-disk SQLite database (the SQLCipher driver
// without a PRAGMA key runs as plain SQLite, mirroring the convention
// already used by diag/local_buffer_test.go). The DB is closed on
// t.Cleanup so the file disappears with the temp dir, satisfying the
// "tests only touch their own data" binding.
func newCacheDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestNewCache_CreatesTable(t *testing.T) {
	db := newCacheDB(t)
	c, err := NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if c == nil || c.db == nil {
		t.Fatal("NewCache returned nil cache or nil db")
	}
	// Verify the table exists by issuing the same CREATE again — should
	// be a no-op when IF NOT EXISTS is honoured.
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='config_cache'`).Scan(&name)
	if err != nil {
		t.Fatalf("config_cache table not created: %v", err)
	}
	if name != "config_cache" {
		t.Fatalf("table name: got %q, want %q", name, "config_cache")
	}
}

func TestNewCache_CreateTableError(t *testing.T) {
	// Closing the DB before calling NewCache makes the CREATE TABLE
	// statement fail. NewCache must wrap the error and return nil cache.
	db := newCacheDB(t)
	_ = db.Close()
	c, err := NewCache(db)
	if err == nil {
		t.Fatal("expected NewCache to error on closed DB")
	}
	if c != nil {
		t.Fatal("cache must be nil when CREATE TABLE fails")
	}
}

func TestCache_SaveLoadRoundTrip(t *testing.T) {
	c, err := NewCache(newCacheDB(t))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	state := json.RawMessage(`{"hello":"world"}`)
	if err := c.Save("exemptions", state, 7); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := c.Load("exemptions")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil for an existing key")
	}
	if got.Key != "exemptions" {
		t.Errorf("Key: got %q, want %q", got.Key, "exemptions")
	}
	if got.Version != 7 {
		t.Errorf("Version: got %d, want 7", got.Version)
	}
	if string(got.State) != string(state) {
		t.Errorf("State: got %s, want %s", got.State, state)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be populated")
	}
}

func TestCache_Save_UpsertReplacesExistingRow(t *testing.T) {
	// ON CONFLICT (key) DO UPDATE — the second Save for the same key
	// replaces state + version + updated_at.
	c, err := NewCache(newCacheDB(t))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if err := c.Save("policy_rules", json.RawMessage(`{"v":1}`), 1); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if err := c.Save("policy_rules", json.RawMessage(`{"v":2}`), 2); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	got, err := c.Load("policy_rules")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 2 {
		t.Errorf("Version: got %d, want 2 (upsert)", got.Version)
	}
	if string(got.State) != `{"v":2}` {
		t.Errorf("State: got %s, want %s", got.State, `{"v":2}`)
	}
}

func TestCache_Save_DBError(t *testing.T) {
	db := newCacheDB(t)
	c, err := NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	_ = db.Close()
	if err := c.Save("k", json.RawMessage(`{}`), 1); err == nil {
		t.Fatal("Save on closed DB must error")
	}
}

func TestCache_Load_MissingKeyReturnsNilNoError(t *testing.T) {
	// Per docstring: "Returns nil if not found." A miss is not an error.
	c, err := NewCache(newCacheDB(t))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	got, err := c.Load("nonexistent")
	if err != nil {
		t.Fatalf("Load on miss must not error; got %v", err)
	}
	if got != nil {
		t.Fatalf("Load on miss must return nil; got %+v", got)
	}
}

func TestCache_Load_DBError(t *testing.T) {
	db := newCacheDB(t)
	c, err := NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	_ = db.Close()
	got, err := c.Load("anything")
	if err == nil {
		t.Fatal("Load on closed DB must error")
	}
	if got != nil {
		t.Fatal("on error, Load must return nil")
	}
}

func TestCache_LoadAll_EmptyTable(t *testing.T) {
	c, err := NewCache(newCacheDB(t))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	all, err := c.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("empty table must return empty slice; got %d rows", len(all))
	}
}

func TestCache_LoadAll_MultipleRows(t *testing.T) {
	c, err := NewCache(newCacheDB(t))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	for _, p := range []struct {
		k string
		v int64
	}{{"a", 1}, {"b", 2}, {"c", 3}} {
		if err := c.Save(p.k, json.RawMessage(`{}`), p.v); err != nil {
			t.Fatalf("Save %s: %v", p.k, err)
		}
	}
	all, err := c.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("LoadAll: got %d rows, want 3", len(all))
	}
	seen := map[string]int64{}
	for _, row := range all {
		seen[row.Key] = row.Version
	}
	for _, want := range []struct {
		k string
		v int64
	}{{"a", 1}, {"b", 2}, {"c", 3}} {
		if seen[want.k] != want.v {
			t.Errorf("row %q: got version %d, want %d", want.k, seen[want.k], want.v)
		}
	}
}

func TestCache_LoadAll_DBError(t *testing.T) {
	db := newCacheDB(t)
	c, err := NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	_ = db.Close()
	all, err := c.LoadAll()
	if err == nil {
		t.Fatal("LoadAll on closed DB must error")
	}
	if all != nil {
		t.Fatal("on error, LoadAll must return nil")
	}
}

func TestCache_LoadAll_ScanError(t *testing.T) {
	// Force a Scan-time error by writing a row whose `version` column
	// holds a non-integer value (TEXT). The original schema declares
	// INTEGER but SQLite is dynamically typed — direct INSERT with the
	// wrong affinity is accepted, then Scan into int64 fails. This
	// exercises the rows.Scan err branch in LoadAll.
	db := newCacheDB(t)
	c, err := NewCache(db)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config_cache(key, state, version, updated_at)
		VALUES ('bad', X'7B7D', 'not-an-int', '2026-05-17T00:00:00Z')`); err != nil {
		t.Fatalf("seed bad row: %v", err)
	}
	all, err := c.LoadAll()
	if err == nil {
		t.Fatalf("LoadAll must error when a row scan fails; got %+v", all)
	}
}
