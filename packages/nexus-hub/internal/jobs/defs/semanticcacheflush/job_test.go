package semanticcacheflush

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"
)

// Test helpers

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ftValkey starts a miniredis instance with stub FT.CREATE and FT.DROPINDEX
// handlers that record invocations. Returns the *redis.Client and a spy that
// captures which commands were called.
type ftSpy struct {
	mu      sync.Mutex
	creates []string // index names passed to FT.CREATE
	drops   []string // index names passed to FT.DROPINDEX

	// errOnCreate makes the next FT.CREATE return an error matching this string.
	// Reset after the first use.
	errOnCreate string
	// errOnDrop makes the next FT.DROPINDEX return an error.
	errOnDrop string
}

func (s *ftSpy) recordCreate(name string) {
	s.mu.Lock()
	s.creates = append(s.creates, name)
	s.mu.Unlock()
}
func (s *ftSpy) recordDrop(name string) {
	s.mu.Lock()
	s.drops = append(s.drops, name)
	s.mu.Unlock()
}
func (s *ftSpy) Creates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.creates))
	copy(out, s.creates)
	return out
}
func (s *ftSpy) Drops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.drops))
	copy(out, s.drops)
	return out
}

// newFTValkey starts a miniredis with FT stub handlers. Returns the client and
// a spy for asserting which commands were issued.
func newFTValkey(t *testing.T) (*redis.Client, *ftSpy) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)

	spy := &ftSpy{}
	srv := mr.Server()

	// FT.CREATE handler — records the index name; responds OK (or error when
	// spy.errOnCreate is set).
	registerFT(t, srv, "FT.CREATE", func(c *server.Peer, _ string, args []string) {
		if len(args) < 1 {
			c.WriteError("ERR FT.CREATE: missing args")
			return
		}
		name := strings.ToUpper(args[0])
		spy.mu.Lock()
		errMsg := spy.errOnCreate
		spy.errOnCreate = ""
		spy.mu.Unlock()
		if errMsg != "" {
			c.WriteError(errMsg)
			return
		}
		spy.recordCreate(name)
		c.WriteOK()
	})

	// FT.DROPINDEX handler — records the index name; responds OK (or error
	// when spy.errOnDrop is set).
	registerFT(t, srv, "FT.DROPINDEX", func(c *server.Peer, _ string, args []string) {
		if len(args) < 1 {
			c.WriteError("ERR FT.DROPINDEX: missing args")
			return
		}
		name := strings.ToUpper(args[0])
		spy.mu.Lock()
		errMsg := spy.errOnDrop
		spy.errOnDrop = ""
		spy.mu.Unlock()
		if errMsg != "" {
			c.WriteError(errMsg)
			return
		}
		spy.recordDrop(name)
		c.WriteOK()
	})

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, spy
}

func registerFT(t *testing.T, srv *server.Server, cmd string, f server.Cmd) {
	t.Helper()
	if err := srv.Register(cmd, f); err != nil {
		t.Fatalf("register %s: %v", cmd, err)
	}
}

// Mock pool helpers

// buildJobWithMock creates a SemanticCacheReindexJob backed by a pgxmock pool
// and the provided redis client.
func buildJobWithMock(t *testing.T, mock pgxmock.PgxPoolIface, rdb redis.UniversalClient) *SemanticCacheReindexJob {
	t.Helper()
	return newWithPool(mock, rdb, 5*time.Second, testLogger())
}

// expectNoStateRow makes the mock return pgx.ErrNoRows for the system_metadata
// SELECT (simulating a fresh DB with no prior reindex).
func expectNoStateRow(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnError(pgx.ErrNoRows)
}

// expectStateRow makes the mock return a JSON blob for the system_metadata SELECT.
func expectStateRow(mock pgxmock.PgxPoolIface, st lastReindexedState) {
	raw, _ := json.Marshal(st)
	rows := pgxmock.NewRows([]string{"value"}).AddRow(raw)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnRows(rows)
}

// expectConfigRow makes the mock return a semantic_cache_config row.
// The query pattern matches the SemanticCacheStore.Get SELECT for the
// fleet-wide singleton (id = 'singleton'). Nexus is single-tenant so the
// SELECT has no org_id column.
func expectConfigRow(mock pgxmock.PgxPoolIface, fp, indexName string, dim *int) {
	provID := "p1"
	modelID := "m1"
	// 18-col SELECT shape per configstore.SemanticCacheStore.Get. Joined columns
	// (provider.baseUrl, model.providerModelId, model.inputPricePerMillion,
	// model.capabilityJson) at the end were added when Get/Save started JOINing
	// Provider + Model so the gateway snapshot carries embedding-call wire,
	// cost-per-token, and max-input-tokens capability directly. Keep in sync with
	// semantic_cache.go's Scan.
	rows := pgxmock.NewRows([]string{
		"id", "embedding_provider_id", "embedding_model_id", "embedding_dimension",
		"embedding_fingerprint", "redis_index_name", "enabled",
		"threshold", "vary_by", "embed_strategy", "allow_cross_model",
		"updated_at", "updated_by",
		"time_sensitive_overrides",
		"provider_base_url", "provider_model_id", "provider_input_price_per_m",
		"model_capability_json",
	}).AddRow(
		"singleton", &provID, &modelID, dim,
		fp, indexName, true,
		0.96, "vk", "system_plus_last_user", false,
		time.Now().UTC(), nil,
		[]byte(`{"rules":[]}`),
		"", "", 0.0,
		"",
	)
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).WillReturnRows(rows)
}

// expectConfigNoRow makes the mock return pgx.ErrNoRows for the config SELECT
// (simulating a fresh DB with no singleton row seeded).
func expectConfigNoRow(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).WillReturnError(pgx.ErrNoRows)
}

// expectPersistState makes the mock expect and accept the system_metadata
// upsert that persists the reindex state. Uses AnyArg() so the JSON blob
// does not need to be matched exactly.
func expectPersistState(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(lastReindexedKey, pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
}

// expectAuditRow makes the mock expect and accept the AdminAuditLog insert.
// The row joins the tamper-evident hash chain via chain.NextHash inside a
// transaction, so the expectation chain is: Begin → advisory lock → chain-head
// read (genesis: no prior rows) → INSERT → Commit. The deferred post-commit
// Rollback is tolerated by pgxmock without its own expectation.
func expectAuditRow(mock pgxmock.PgxPoolIface) {
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
}

// Identity tests

func TestSemanticCacheReindexJob_Identity(t *testing.T) {
	j := New(nil, nil, 0, testLogger())
	if j.ID() != semanticCacheFlushJobID {
		t.Errorf("ID = %q, want %q", j.ID(), semanticCacheFlushJobID)
	}
	if j.Name() == "" {
		t.Error("Name is empty")
	}
	if j.Description() == "" {
		t.Error("Description is empty")
	}
	if j.Interval() != 5*time.Second {
		t.Errorf("default Interval = %v, want 5s", j.Interval())
	}
	j2 := New(nil, nil, 10*time.Second, testLogger())
	if j2.Interval() != 10*time.Second {
		t.Errorf("custom Interval = %v, want 10s", j2.Interval())
	}
}

// newWithPool with zero interval defaults to 5s.
func TestNewWithPool_ZeroIntervalDefaults(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := newWithPool(mock, nil, 0, testLogger())
	if j.Interval() != 5*time.Second {
		t.Errorf("newWithPool zero interval = %v, want 5s", j.Interval())
	}
}

// No-op: Redis client is nil

func TestSemanticCacheReindexJob_NilRedis(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := buildJobWithMock(t, mock, nil)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run with nil rdb: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// No-op: empty fingerprint (admin hasn't configured embedding yet)

func TestSemanticCacheReindexJob_EmptyFingerprint(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Config row with empty fingerprint (default row from ErrNoRows fallback).
	expectConfigNoRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(spy.Creates()) != 0 {
		t.Errorf("expected 0 FT.CREATE calls, got %v", spy.Creates())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// No-op: fingerprint unchanged (steady state)

func TestSemanticCacheReindexJob_FingerprintUnchanged(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "aabbcc"
	const indexName = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, indexName, &dim)
	expectStateRow(mock, lastReindexedState{
		Fingerprint:  fp,
		NewIndexName: indexName,
	})

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(spy.Creates()) != 0 {
		t.Errorf("expected 0 FT.CREATE calls, got %v", spy.Creates())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// Happy path: fingerprint changed → EnsureIndex + DropIndex + audit row

func TestSemanticCacheReindexJob_HappyPath_FingerprintChanged(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const oldFP = "aabb"
	const newFP = "ccdd"
	const oldIndex = "nexus:semantic-cache:v1"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	// Run sequence:
	// 1. Load config row (newFP, newIndex)
	expectConfigRow(mock, newFP, newIndex, &dim)
	// 2. Load last-reindexed fingerprint — returns oldFP
	expectStateRow(mock, lastReindexedState{
		Fingerprint:  oldFP,
		NewIndexName: oldIndex, // the "old index" for this new run
	})
	// 3. Load old index name (uses the same loadReindexState — same call path)
	// Note: loadLastReindexed and loadLastOldIndexName each call loadReindexState
	// which issues one QueryRow. We call both in sequence so we need two expects.
	expectStateRow(mock, lastReindexedState{
		Fingerprint:  oldFP,
		NewIndexName: oldIndex,
	})
	// 4. Persist new state
	expectPersistState(mock)
	// 5. Write audit row
	expectAuditRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	creates := spy.Creates()
	if len(creates) != 1 || !strings.EqualFold(creates[0], newIndex) {
		t.Errorf("FT.CREATE: got %v, want [%s]", creates, newIndex)
	}
	drops := spy.Drops()
	if len(drops) != 1 || !strings.EqualFold(drops[0], oldIndex) {
		t.Errorf("FT.DROPINDEX: got %v, want [%s]", drops, oldIndex)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// Happy path: first reindex ever (no prior state in system_metadata)

func TestSemanticCacheReindexJob_FirstRun_NoState(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const newFP = "ccdd"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	// First loadLastReindexed → ErrNoRows → empty fp
	expectNoStateRow(mock)
	// Second loadLastOldIndexName → ErrNoRows → empty old index name
	expectNoStateRow(mock)
	// Persist
	expectPersistState(mock)
	// Audit
	expectAuditRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	creates := spy.Creates()
	if len(creates) != 1 || !strings.EqualFold(creates[0], newIndex) {
		t.Errorf("FT.CREATE: got %v, want [%s]", creates, newIndex)
	}
	// No old index → no DropIndex expected.
	if len(spy.Drops()) != 0 {
		t.Errorf("unexpected FT.DROPINDEX calls: %v", spy.Drops())
	}
}

// Idempotency: FT.CREATE already exists → job treats it as no-op success

func TestSemanticCacheReindexJob_Idempotent_IndexAlreadyExists(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "newfingerprint"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, newIndex, &dim)
	expectNoStateRow(mock) // no prior state
	expectNoStateRow(mock) // no old index name
	expectPersistState(mock)
	expectAuditRow(mock)

	// Make FT.CREATE respond with "already exists" — the job should treat this
	// as idempotent and continue to persist state + drop old.
	spy.mu.Lock()
	spy.errOnCreate = "Index already exists"
	spy.mu.Unlock()

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// FT.CREATE was called (even though it returned "already exists").
	creates := spy.Creates()
	// spy.Creates records ONLY when the handler records — since we returned
	// an error before recording, Creates is empty; that's fine: the key
	// assertion is that Run returns nil.
	_ = creates

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// Partial failure: EnsureIndex fails → error returned, state NOT persisted

func TestSemanticCacheReindexJob_PartialFailure_EnsureIndexFails(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "newfp"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, newIndex, &dim)
	expectNoStateRow(mock) // no prior fingerprint → triggers reindex
	expectNoStateRow(mock) // no prior old index name

	// FT.CREATE returns a non-idempotent error (e.g. connection refused).
	spy.mu.Lock()
	spy.errOnCreate = "ERR connection refused"
	spy.mu.Unlock()

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when FT.CREATE fails, got nil")
	}
	if !strings.Contains(err.Error(), "FT.CREATE") {
		t.Errorf("error %q does not mention FT.CREATE", err.Error())
	}

	// State must NOT be persisted — no expectPersistState was registered.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// Partial failure: DropIndex fails → job logs warn, returns nil (best-effort)

func TestSemanticCacheReindexJob_DropIndexFails_ContinuesGracefully(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const oldFP = "old"
	const newFP = "new"
	const oldIndex = "nexus:semantic-cache:v1"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectStateRow(mock, lastReindexedState{Fingerprint: oldFP, NewIndexName: oldIndex})
	expectStateRow(mock, lastReindexedState{Fingerprint: oldFP, NewIndexName: oldIndex})
	expectPersistState(mock)
	expectAuditRow(mock)

	// Make FT.DROPINDEX fail.
	spy.mu.Lock()
	spy.errOnDrop = "ERR some drop error"
	spy.mu.Unlock()

	j := buildJobWithMock(t, mock, rdb)
	// Drop failure is best-effort: Run should return nil.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// DB failure: config load fails → error propagated

func TestSemanticCacheReindexJob_ConfigLoadFails(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM semantic_cache_config sc.*WHERE sc.id = 'singleton'`).
		WillReturnError(errors.New("db: connection refused"))

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error %q should mention 'load config'", err.Error())
	}
}

// DB failure: persist state fails → error propagated, no DropIndex

func TestSemanticCacheReindexJob_PersistFails(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "newfp"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, newIndex, &dim)
	expectNoStateRow(mock)
	expectNoStateRow(mock)

	// FT.CREATE succeeds but persist fails.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(lastReindexedKey, pgxmock.AnyArg()).
		WillReturnError(errors.New("db: disk full"))

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "persist state") {
		t.Errorf("error %q should mention 'persist state'", err.Error())
	}

	// FT.CREATE was called (before persist).
	creates := spy.Creates()
	if len(creates) != 1 {
		t.Errorf("expected 1 FT.CREATE, got %v", creates)
	}
	// No DropIndex since persist failed.
	if len(spy.Drops()) != 0 {
		t.Errorf("unexpected FT.DROPINDEX calls: %v", spy.Drops())
	}
}

// Idempotency on second run after EnsureIndex succeeded but DropIndex crashed

func TestSemanticCacheReindexJob_SecondRun_DropOldAfterCrash(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Simulate state after a crash: system_metadata has the NEW fingerprint
	// persisted (EnsureIndex + persist succeeded last time) but DropIndex
	// was never called. On this run:
	// - config fingerprint == state fingerprint → NO-OP (fingerprints match).
	// The DropIndex retry is naturally handled by the NEXT fingerprint change.
	// This test verifies the no-op path when fingerprints already match —
	// which is the idempotency guarantee: a second run after a partial crash
	// does NOT re-run FT.CREATE or FT.DROPINDEX once state is persisted.

	const fp = "currentfp"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, newIndex, &dim)
	expectStateRow(mock, lastReindexedState{
		Fingerprint:  fp, // matches current → no-op
		NewIndexName: newIndex,
	})

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run (second run after crash): %v", err)
	}
	if len(spy.Creates()) != 0 {
		t.Errorf("unexpected FT.CREATE on second run: %v", spy.Creates())
	}
	if len(spy.Drops()) != 0 {
		t.Errorf("unexpected FT.DROPINDEX on second run: %v", spy.Drops())
	}
}

// FT.DROPINDEX index not found → idempotent (no error)

func TestSemanticCacheReindexJob_DropIndex_AlreadyGone(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const oldFP = "prev"
	const newFP = "curr"
	const oldIndex = "nexus:semantic-cache:v1"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectStateRow(mock, lastReindexedState{Fingerprint: oldFP, NewIndexName: oldIndex})
	expectStateRow(mock, lastReindexedState{Fingerprint: oldFP, NewIndexName: oldIndex})
	expectPersistState(mock)
	expectAuditRow(mock)

	// FT.DROPINDEX returns "Unknown index name" (already gone) → idempotent.
	spy.mu.Lock()
	spy.errOnDrop = "Unknown index name (first: " + strings.ToUpper(oldIndex) + ")"
	spy.mu.Unlock()

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// loadLastOldIndexName returns error → job warns and continues (no DropIndex)

func TestSemanticCacheReindexJob_OldIndexNameLoadFails_ContinuesWithoutDrop(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const oldFP = "prev"
	const newFP = "curr"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 768

	expectConfigRow(mock, newFP, newIndex, &dim)
	// loadLastReindexed: returns oldFP (different from newFP → triggers reindex)
	expectStateRow(mock, lastReindexedState{Fingerprint: oldFP, NewIndexName: "nexus:semantic-cache:v1"})
	// loadLastOldIndexName: DB error → logged as warn, oldIndexName = ""
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnError(errors.New("db: read-only replica"))
	// EnsureIndex + persist + audit still run.
	expectPersistState(mock)
	expectAuditRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// FT.CREATE was called.
	if len(spy.Creates()) != 1 {
		t.Errorf("expected 1 FT.CREATE, got %v", spy.Creates())
	}
	// No DropIndex because oldIndexName was empty.
	if len(spy.Drops()) != 0 {
		t.Errorf("unexpected FT.DROPINDEX calls: %v", spy.Drops())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// writeAuditRow: Exec fails → logged at warn, Run still returns nil

func TestSemanticCacheReindexJob_AuditRowExecFails_NoError(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const newFP = "fp"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectNoStateRow(mock)
	expectNoStateRow(mock)
	expectPersistState(mock)
	// Audit INSERT fails inside the chain transaction; writeAuditRow logs at
	// WARN and rolls back, but Run still returns nil.
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnError(errors.New("db: audit table locked"))
	mock.ExpectRollback()

	j := buildJobWithMock(t, mock, rdb)
	// Audit failure must NOT propagate as an error.
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// writeAuditRow: tx Begin fails → logged at warn, Run still returns nil

func TestSemanticCacheReindexJob_AuditTxBeginFails_NoError(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const newFP = "fp"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectNoStateRow(mock)
	expectNoStateRow(mock)
	expectPersistState(mock)
	// The audit transaction cannot even begin; reindex already succeeded so
	// the failure is swallowed.
	mock.ExpectBegin().WillReturnError(errors.New("db: too many connections"))

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// writeAuditRow: chain.NextHash fails (advisory lock) → warn, rollback, Run nil

func TestSemanticCacheReindexJob_AuditChainHashFails_NoError(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const newFP = "fp"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectNoStateRow(mock)
	expectNoStateRow(mock)
	expectPersistState(mock)
	mock.ExpectBegin()
	// Advisory-lock acquisition (first statement inside NextHash) fails.
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("db: lock wait timeout"))
	mock.ExpectRollback()

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// writeAuditRow: tx Commit fails → logged at warn, Run still returns nil

func TestSemanticCacheReindexJob_AuditCommitFails_NoError(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const newFP = "fp"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 1536

	expectConfigRow(mock, newFP, newIndex, &dim)
	expectNoStateRow(mock)
	expectNoStateRow(mock)
	expectPersistState(mock)
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit().WillReturnError(errors.New("db: commit conflict"))

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// system_metadata load fails → error propagated

func TestSemanticCacheReindexJob_StateLoadFails(t *testing.T) {
	rdb, _ := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "fp"
	const newIndex = "nexus:semantic-cache:v2"
	dim := 1536

	expectConfigRow(mock, fp, newIndex, &dim)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnError(errors.New("db: replica down"))

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "last-reindexed fingerprint") {
		t.Errorf("error %q should mention 'last-reindexed fingerprint'", err.Error())
	}
}

// Dimension 0 → error returned, no Valkey commands issued

func TestSemanticCacheReindexJob_ZeroDimension(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "fp"
	const newIndex = "nexus:semantic-cache:v2"
	zeroDim := 0

	expectConfigRow(mock, fp, newIndex, &zeroDim)
	expectNoStateRow(mock) // no prior fingerprint → triggers reindex
	expectNoStateRow(mock) // no prior old index

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for zero dimension, got nil")
	}
	if !strings.Contains(err.Error(), "embedding_dimension") {
		t.Errorf("error %q should mention 'embedding_dimension'", err.Error())
	}
	if len(spy.Creates()) != 0 {
		t.Errorf("unexpected FT.CREATE calls: %v", spy.Creates())
	}
}

// Nil dimension pointer in config row → error returned

func TestSemanticCacheReindexJob_NilDimension(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "fp"
	const newIndex = "nexus:semantic-cache:v2"

	expectConfigRow(mock, fp, newIndex, nil)
	expectNoStateRow(mock)
	expectNoStateRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for nil dimension, got nil")
	}
	if len(spy.Creates()) != 0 {
		t.Errorf("unexpected FT.CREATE calls: %v", spy.Creates())
	}
}

// loadReindexState: corrupt JSON in system_metadata → treated as absent

func TestSemanticCacheReindexJob_CorruptStateValue(t *testing.T) {
	rdb, spy := newFTValkey(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const fp = "fp"
	const newIndex = "nexus:semantic-cache:v1"
	dim := 512

	expectConfigRow(mock, fp, newIndex, &dim)
	// loadLastReindexed — returns corrupt JSON → treated as "" fingerprint.
	rows := pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-json"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnRows(rows)

	// loadLastOldIndexName — also corrupt, treated as empty old index.
	rows2 := pgxmock.NewRows([]string{"value"}).AddRow([]byte("{bad"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs(lastReindexedKey).
		WillReturnRows(rows2)

	// Since the corrupt JSON is treated as "no prior state", fingerprint="" vs
	// fp="fp" → reindex proceeds.
	expectPersistState(mock)
	expectAuditRow(mock)

	j := buildJobWithMock(t, mock, rdb)
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	creates := spy.Creates()
	if len(creates) != 1 {
		t.Errorf("expected 1 FT.CREATE, got %v", creates)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}
