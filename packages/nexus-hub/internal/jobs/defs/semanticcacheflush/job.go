// Package semanticcacheflush implements the Hub-side blue/green Valkey vector
// index swap triggered by an embedding model change.
//
// # Lifecycle
//
// When an admin saves a new embedding model on the Cache Embedding Settings UI,
// the Control Plane writes the row and bumps redis_index_name from vN to v(N+1).
// The AI Gateway's IndexLifecycle observer calls FT.CREATE on the new index so
// new writes land there immediately. This job
// then runs (on a 5s tick, catching up with no-op runs when fingerprints
// match) and:
//
//  1. Loads the singleton semantic_cache_config row.
//  2. Compares the row's EmbeddingFingerprint against the last-reindexed
//     fingerprint persisted in system_metadata.
//  3. When fingerprints differ (and the current fingerprint is non-empty):
//     a. Calls FT.CREATE on the new index name (idempotent — AI Gateway may
//     have already created it).
//     b. Persists the new fingerprint as last-reindexed in system_metadata
//     BEFORE dropping the old index, so a crash between b and c does not
//     repeat the create on the next tick.
//     c. Calls FT.DROPINDEX on the old index name (idempotent — OK if missing).
//     d. Stamps an AdminAuditLog row: action=semantic-cache.reindex.
//  4. Returns nil on no-op or success; returns an error on failure so the
//     scheduler re-runs on the next tick.
//
// # Idempotency
//
// Because FT.CREATE and FT.DROPINDEX are both idempotent (silently succeed on
// existing / missing indexes respectively), and the fingerprint is persisted to
// system_metadata before DropIndex runs, a crash at any point leaves the system
// in a correct state for the next tick.
//
// # Redis client requirement
//
// The job accepts a redis.UniversalClient so it works with both standalone
// Valkey and Valkey Cluster (future). The Hub's existing redis.UniversalClient
// (from wiring/redis.go) is injected at startup. When the client is nil (Redis
// unavailable at boot), the job no-ops and returns nil — treating Redis as an
// optional dependency consistent with the Hub's existing Redis policy.
package semanticcacheflush

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

const (
	// semanticCacheFlushJobID is the stable slug used as the PK in the `job`
	// table and referenced by the CI catalogue check.
	semanticCacheFlushJobID          = "semantic-cache-reindex"
	semanticCacheFlushJobName        = "Semantic Cache Reindex"
	semanticCacheFlushJobDescription = "Blue/green Valkey vector index swap when the embedding model fingerprint changes. Creates the new FT index, drops the old one, and stamps an audit row."

	// lastReindexedKey is the system_metadata key used to persist the
	// last-successfully-reindexed fingerprint across Hub restarts.
	lastReindexedKey = "semantic_cache.last_reindexed_fingerprint"

	// indexAlreadyExistsMsg is the valkey-search error prefix for FT.CREATE on
	// an existing index. Mirror of the constant in the ai-gateway client so the
	// Hub job is independent without importing that package.
	indexAlreadyExistsMsg = "Index already exists"

	// indexNotFoundMsg is the valkey-search error prefix for FT.DROPINDEX on a
	// non-existent index.
	indexNotFoundMsg = "Unknown index name"
)

// pool is the narrow pgx surface this job needs (Exec + QueryRow).
// `*pgxpool.Pool` satisfies it in production; `pgxmock.PgxPoolIface`
// satisfies it in tests.
type pool interface {
	defs.PgxPool
}

// SemanticCacheReindexJob performs the blue/green index swap whenever the
// embedding model fingerprint changes. Designed to run every 5 seconds; it
// no-ops in O(1) when fingerprints already match.
type SemanticCacheReindexJob struct {
	pool     pool
	rdb      redis.UniversalClient
	store    *configstore.SemanticCacheStore
	interval time.Duration
	logger   *slog.Logger
}

// New constructs the job. interval defaults to 5s. pool must not be nil;
// rdb may be nil (job no-ops when Redis is unavailable).
func New(
	pgPool *pgxpool.Pool,
	rdb redis.UniversalClient,
	interval time.Duration,
	logger *slog.Logger,
) *SemanticCacheReindexJob {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &SemanticCacheReindexJob{
		pool:     pgPool,
		rdb:      rdb,
		store:    configstore.NewSemanticCacheStore(pgPool),
		interval: interval,
		logger:   logger.With("job", semanticCacheFlushJobID),
	}
}

// newWithPool is the test-only constructor accepting the pool interface directly
// so pgxmock can be injected.
func newWithPool(
	p pool,
	rdb redis.UniversalClient,
	interval time.Duration,
	logger *slog.Logger,
) *SemanticCacheReindexJob {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &SemanticCacheReindexJob{
		pool:     p,
		rdb:      rdb,
		store:    configstore.NewSemanticCacheStoreWithPgxPool(p),
		interval: interval,
		logger:   logger.With("job", semanticCacheFlushJobID),
	}
}

func (j *SemanticCacheReindexJob) ID() string              { return semanticCacheFlushJobID }
func (j *SemanticCacheReindexJob) Name() string            { return semanticCacheFlushJobName }
func (j *SemanticCacheReindexJob) Description() string     { return semanticCacheFlushJobDescription }
func (j *SemanticCacheReindexJob) Interval() time.Duration { return j.interval }

// Run executes one reindex check cycle. It is idempotent and safe to call
// concurrently (the scheduler wraps it with SkipIfStillRunning).
//
// Returns nil on no-op (fingerprints match, Redis unavailable, or empty
// fingerprint). Returns an error on failures so the scheduler re-runs on
// the next tick.
//
// Nexus is single-tenant — there is only one semantic_cache_config row
// (id='singleton') and one corresponding Redis Vector index to manage.
func (j *SemanticCacheReindexJob) Run(ctx context.Context) error {
	if j.rdb == nil {
		j.logger.Debug("semantic-cache-reindex: redis client nil, skipping")
		return nil
	}

	// 1. Load the singleton config row.
	row, err := j.store.Get(ctx)
	if err != nil {
		return fmt.Errorf("semantic-cache-reindex: load config: %w", err)
	}

	// 2. Skip if no valid fingerprint (admin hasn't configured an embedding
	//    model yet, or all three fields are nil).
	if row.EmbeddingFingerprint == "" {
		j.logger.Debug("semantic-cache-reindex: empty fingerprint, skipping")
		return nil
	}

	// 3. Load the last-reindexed fingerprint from system_metadata.
	lastFP, err := j.loadLastReindexedForKey(ctx, lastReindexedKey)
	if err != nil {
		return fmt.Errorf("semantic-cache-reindex: load last-reindexed fingerprint: %w", err)
	}

	// 4. No-op when fingerprints match.
	if lastFP == row.EmbeddingFingerprint {
		j.logger.Debug("semantic-cache-reindex: fingerprint unchanged, no-op",
			"fingerprint", row.EmbeddingFingerprint,
			"indexName", row.RedisIndexName,
		)
		return nil
	}

	j.logger.Info("semantic-cache-reindex: fingerprint changed, running index swap",
		"oldFingerprint", lastFP,
		"newFingerprint", row.EmbeddingFingerprint,
		"newIndexName", row.RedisIndexName,
	)

	// 5. Derive the old index name from the previously persisted state.
	oldIndexName, err := j.loadLastOldIndexNameForKey(ctx, lastReindexedKey)
	if err != nil {
		// Non-fatal: we'll just skip the DropIndex this cycle. Old index leaks
		// in this edge case but the next successful reindex will clean it up.
		j.logger.Warn("semantic-cache-reindex: could not load old index name; DropIndex skipped",
			"error", err)
		oldIndexName = ""
	}

	dim := 0
	if row.EmbeddingDimension != nil {
		dim = *row.EmbeddingDimension
	}
	if dim <= 0 {
		return fmt.Errorf("semantic-cache-reindex: embedding_dimension is %d (non-positive); cannot create index", dim)
	}

	// 6a. FT.CREATE new index (idempotent).
	if err := j.ensureIndex(ctx, row.RedisIndexName, dim); err != nil {
		return fmt.Errorf("semantic-cache-reindex: FT.CREATE %q: %w", row.RedisIndexName, err)
	}

	// 6b. Persist the new fingerprint + index names BEFORE dropping the old
	//     index. A crash here means the next tick re-runs step 6a (idempotent)
	//     then skips to 6c with the correct old name.
	if err := j.persistReindexStateForKey(ctx, lastReindexedKey, row.EmbeddingFingerprint, oldIndexName, row.RedisIndexName); err != nil {
		return fmt.Errorf("semantic-cache-reindex: persist state: %w", err)
	}

	// 6c. FT.DROPINDEX old index (idempotent — returns nil on missing).
	if oldIndexName != "" && oldIndexName != row.RedisIndexName {
		if err := j.dropIndex(ctx, oldIndexName); err != nil {
			// Log but do not return error: the new index is live and the
			// fingerprint is persisted. The old index is orphaned but the
			// system is consistent. Operator can drop it manually.
			j.logger.Warn("semantic-cache-reindex: FT.DROPINDEX failed; old index may be orphaned",
				"oldIndexName", oldIndexName,
				"error", err,
			)
		}
	}

	// 6d. Stamp an AdminAuditLog row.
	j.writeAuditRow(ctx, lastFP, row.EmbeddingFingerprint, oldIndexName, row.RedisIndexName, dim)

	j.logger.Info("semantic-cache-reindex: index swap complete",
		"oldIndex", oldIndexName,
		"newIndex", row.RedisIndexName,
		"fingerprint", row.EmbeddingFingerprint,
		"dim", dim,
	)
	return nil
}

// system_metadata helpers

// lastReindexedState is the JSON blob stored under a system_metadata key in
// system_metadata. It records the fingerprint that was successfully indexed
// AND the old/new index names at that time so DropIndex knows what to drop.
type lastReindexedState struct {
	Fingerprint  string `json:"fingerprint"`
	NewIndexName string `json:"newIndexName"`
	OldIndexName string `json:"oldIndexName"`
}

// loadLastReindexedForKey returns the fingerprint stored in system_metadata
// under the given key. Returns "" when no row exists yet (first run).
func (j *SemanticCacheReindexJob) loadLastReindexedForKey(ctx context.Context, key string) (string, error) {
	st, err := j.loadReindexStateForKey(ctx, key)
	if err != nil {
		return "", err
	}
	return st.Fingerprint, nil
}

// loadLastOldIndexNameForKey returns the old index name stored in system_metadata
// under the given key at the previous reindex. Returns "" on first run.
func (j *SemanticCacheReindexJob) loadLastOldIndexNameForKey(ctx context.Context, key string) (string, error) {
	st, err := j.loadReindexStateForKey(ctx, key)
	if err != nil {
		return "", err
	}
	// The "old" index from the perspective of this new run is the "new" index
	// from the last run (the one that was created last time we reindexed).
	return st.NewIndexName, nil
}

// loadReindexStateForKey reads the lastReindexedState blob from system_metadata
// under the given key. Returns a zero-value struct when no row exists.
func (j *SemanticCacheReindexJob) loadReindexStateForKey(ctx context.Context, key string) (lastReindexedState, error) {
	var raw []byte
	err := j.pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`,
		key,
	).Scan(&raw)
	if err != nil {
		// pgx ErrNoRows → no prior state; return zero-value struct.
		if strings.Contains(err.Error(), "no rows") {
			return lastReindexedState{}, nil
		}
		return lastReindexedState{}, fmt.Errorf("query system_metadata: %w", err)
	}

	var st lastReindexedState
	if err := json.Unmarshal(raw, &st); err != nil {
		// Corrupt value — treat as absent (re-run reindex).
		j.logger.Warn("semantic-cache-reindex: corrupt system_metadata value; treating as absent",
			"key", key, "error", err)
		return lastReindexedState{}, nil
	}
	return st, nil
}

// persistReindexStateForKey upserts the state blob into system_metadata under key.
func (j *SemanticCacheReindexJob) persistReindexStateForKey(ctx context.Context, key, fingerprint, oldIndexName, newIndexName string) error {
	st := lastReindexedState{
		Fingerprint:  fingerprint,
		NewIndexName: newIndexName,
		OldIndexName: oldIndexName,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	_, err = j.pool.Exec(ctx, `
		INSERT INTO system_metadata (key, value, updated_at, updated_by)
		VALUES ($1, $2, NOW(), 'hub-job')
		ON CONFLICT (key) DO UPDATE SET
			value      = EXCLUDED.value,
			updated_at = NOW(),
			updated_by = 'hub-job'
	`, key, raw)
	if err != nil {
		return fmt.Errorf("upsert system_metadata: %w", err)
	}
	return nil
}

// Valkey helpers

// ensureIndex runs FT.CREATE <indexName> ON HASH ... and is idempotent on
// an existing index (logs debug, returns nil).
func (j *SemanticCacheReindexJob) ensureIndex(ctx context.Context, indexName string, dim int) error {
	args := []interface{}{
		"FT.CREATE", indexName,
		"ON", "HASH",
		"PREFIX", "1", indexName + ":",
		"SCHEMA",
		"vector", "VECTOR", "HNSW", "12",
		"DIM", fmt.Sprintf("%d", dim),
		"TYPE", "FLOAT32",
		"DISTANCE_METRIC", "COSINE",
		"M", "16",
		"EF_CONSTRUCTION", "200",
		"EF_RUNTIME", "10",
		"upstream_provider", "TAG",
		"upstream_model", "TAG",
		"vk_scope", "TAG",
		"response_kind", "TAG",
		"fingerprint", "TAG",
		"response_body", "TEXT", "NOINDEX",
		"usage", "TEXT", "NOINDEX",
		"cached_at", "NUMERIC",
	}
	err := j.rdb.Do(ctx, args...).Err()
	if err != nil {
		if strings.Contains(err.Error(), indexAlreadyExistsMsg) {
			j.logger.Debug("semantic-cache-reindex: FT.CREATE: index already exists, skipping",
				"indexName", indexName)
			return nil
		}
		return err
	}
	j.logger.Info("semantic-cache-reindex: FT.CREATE: created index",
		"indexName", indexName, "dim", dim)
	return nil
}

// dropIndex runs FT.DROPINDEX and is idempotent on a missing index.
func (j *SemanticCacheReindexJob) dropIndex(ctx context.Context, indexName string) error {
	err := j.rdb.Do(ctx, "FT.DROPINDEX", indexName).Err()
	if err != nil {
		if strings.Contains(err.Error(), indexNotFoundMsg) {
			j.logger.Debug("semantic-cache-reindex: FT.DROPINDEX: index not found, skipping",
				"indexName", indexName)
			return nil
		}
		return err
	}
	j.logger.Info("semantic-cache-reindex: FT.DROPINDEX: dropped index",
		"indexName", indexName)
	return nil
}

// Audit log

// reindexAuditPayload is the JSON payload written to AdminAuditLog.afterState.
type reindexAuditPayload struct {
	OldIndexName   string `json:"oldIndexName"`
	NewIndexName   string `json:"newIndexName"`
	Fingerprint    string `json:"fingerprint"`
	OldFingerprint string `json:"oldFingerprint"`
	Dim            int    `json:"dim"`
}

// writeAuditRow inserts one AdminAuditLog row for the reindex event.
// Errors are logged at WARN only — the reindex has already succeeded at this
// point; losing the audit trail is preferable to surfacing a false error.
//
// The row joins the tamper-evident hash chain like every other AdminAuditLog
// writer (F3: the Hub is the sole chain writer, through one helper, with no
// parallel non-chained path). The insert runs in a short transaction so
// chain.NextHash can hold the advisory lock across the head read and the
// insert. The timestamp is generated in Go and hashed as an int64 ms-epoch,
// then persisted via to_timestamp from the same value, so VerifyChain
// recomputes the row byte-for-byte. actorRole='system' stays as a display
// attribute that distinguishes scheduler-written rows from admin actions; it
// is no longer load-bearing for chain linkage.
func (j *SemanticCacheReindexJob) writeAuditRow(
	ctx context.Context,
	oldFP, newFP, oldIndexName, newIndexName string,
	dim int,
) {
	payload := reindexAuditPayload{
		OldIndexName:   oldIndexName,
		NewIndexName:   newIndexName,
		Fingerprint:    newFP,
		OldFingerprint: oldFP,
		Dim:            dim,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		j.logger.Warn("semantic-cache-reindex: marshal audit payload failed", "error", err)
		return
	}

	tx, err := j.pool.Begin(ctx)
	if err != nil {
		j.logger.Warn("semantic-cache-reindex: begin audit tx failed", "error", err)
		return
	}
	// Rollback is a no-op after a successful Commit; the deferred call covers
	// every early-return path below without leaking the transaction.
	defer func() { _ = tx.Rollback(ctx) }()

	// Action and actor are compile-time constants here, so the validated
	// chain.NewHashPayload constructor (which the runtime-driven writers use
	// because their actor/action come from request data) would only contribute
	// an unreachable error branch. Build the payload directly instead.
	hp := chain.HashPayload{
		TimestampMs: time.Now().UTC().UnixMilli(),
		Action:      "semantic-cache.reindex",
		ActorID:     "hub-job",
		EntityType:  "semantic_cache_config",
		EntityID:    "singleton",
		AfterState:  json.RawMessage(payloadJSON),
	}

	prevHash, integrityHash, hashInput, err := chain.NextHash(ctx, tx, hp)
	if err != nil {
		j.logger.Warn("semantic-cache-reindex: compute chain hash failed", "error", err)
		return
	}
	// Genesis row stores previousHash NULL; every later row stores the prior
	// integrityHash.
	var prevArg any
	if prevHash != "" {
		prevArg = prevHash
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO "AdminAuditLog" (
			id, timestamp,
			"actorId", "actorLabel", "actorRole",
			action, "entityType", "entityId",
			"afterState",
			"previousHash", "integrityHash", "hashInput"
		) VALUES (
			gen_random_uuid()::text, to_timestamp($1 / 1000.0),
			'hub-job', 'Hub Scheduler', 'system',
			'semantic-cache.reindex', 'semantic_cache_config', 'singleton',
			$2,
			$3, $4, $5
		)
	`, hp.TimestampMs, payloadJSON, prevArg, integrityHash, hashInput); err != nil {
		j.logger.Warn("semantic-cache-reindex: write audit row failed", "error", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		j.logger.Warn("semantic-cache-reindex: commit audit row failed", "error", err)
	}
}
