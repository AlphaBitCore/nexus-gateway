package configstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SemanticCacheConfigRow mirrors the semantic_cache_config table row with
// idiomatic Go field names + JSON tags for API transport.
//
// The row is a fleet-wide singleton (id = "singleton"). Nexus is single-tenant
// by design: there is no per-org or per-tenant variation of this config.
//
// Infrastructure fields only: embedding selection, fingerprint, index name,
// fleet kill switch, time-sensitive rule overrides. Per-route policy lives
// on routing_rule.response_cache_policy.semantic — not here.
type SemanticCacheConfigRow struct {
	ID                   string  `json:"id"`
	EmbeddingProviderID  *string `json:"embeddingProviderId"`
	EmbeddingModelID     *string `json:"embeddingModelId"`
	EmbeddingDimension   *int    `json:"embeddingDimension"`
	EmbeddingFingerprint string  `json:"embeddingFingerprint"`
	// RedisIndexName is the versioned Redis Vector index name used in
	// FT.CREATE / FT.SEARCH. Default: "nexus:semantic-cache:v1".
	// Bumped from v1 → v2 → v3 on (provider, model, dimension) changes
	// to achieve atomic blue/green index swap without dropping live traffic.
	RedisIndexName string `json:"redisIndexName"`
	Enabled        bool   `json:"enabled"`
	// Threshold is the fleet-wide cosine similarity gate for L2 hits.
	// Range [0.0, 1.0]; default 0.96. Fleet-level only; per-route
	// policy is not supported.
	Threshold float64 `json:"threshold"`
	// VaryBy controls L2 cache isolation scope. Enum: none | user | vk | org.
	// Default "vk" matches single-tenant deployments.
	VaryBy string `json:"varyBy"`
	// EmbedStrategy shapes the embedding input. Enum: last_user |
	// system_plus_last_user | recent_turns | head_plus_tail | full_truncated.
	// Default "system_plus_last_user".
	EmbedStrategy string `json:"embedStrategy"`
	// AllowCrossModel lets an L2 lookup return an entry cached against a
	// different upstream model than the current one. Default false.
	AllowCrossModel bool      `json:"allowCrossModel"`
	UpdatedAt       time.Time `json:"updatedAt"`
	UpdatedBy       *string   `json:"updatedBy"`
	// TimeSensitiveOverrides is the DB-persisted blob of admin-edited freshness
	// rules. The Handler merges seed rules with this blob on GET (DB wins per
	// rule ID). Stored as JSONB; default empty blob means no overrides.
	TimeSensitiveOverrides TimeSensitiveOverridesBlob `json:"timeSensitiveOverrides"`
	// Joined columns (provider.baseUrl, model.providerModelId,
	// model.inputPricePerMillion) — populated by Get/Save via LEFT JOIN.
	// The gateway needs these in the Hub-pushed snapshot so its L2
	// Reader/Writer can call the embedding upstream + compute embedding
	// cost without an in-memory provider/model lookup on every request.
	// json tags match the gateway's semanticCacheConfigBlob receiver
	// fields in packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go.
	EmbeddingProviderBaseURL      string  `json:"embeddingProviderBaseUrl,omitempty"`
	EmbeddingProviderModelID      string  `json:"embeddingProviderModelId,omitempty"`
	EmbeddingInputPricePerMillion float64 `json:"embeddingInputPricePerMillion,omitempty"`
	// EmbeddingMaxInputTokens is the embedding model's context window
	// (capabilityJson.embeddings.max_input_tokens), joined at Get/Save time so
	// the gateway can truncate the embed input to the model's real limit
	// instead of a hardcoded fallback. 0 when the model declares no limit.
	EmbeddingMaxInputTokens int `json:"embeddingMaxInputTokens,omitempty"`
}

// WireState is the canonical Hub-shadow State for the semantic_cache.config
// key. It is the full row MINUS the two bookkeeping columns (UpdatedAt,
// UpdatedBy): UpdatedAt is stamped from the Go process wall clock by Save but
// from the DB NOW() when read back by Get, so the two never byte-match — and
// without dropping it the configreconcile content-diff watch
// would log a spurious drift + heal on every admin save. The AI Gateway
// receiver does not consume either field. Both the admin push (PutConfig) and
// the reconcile SourceLoader call this so the diff is apples-to-apples.
func (r *SemanticCacheConfigRow) WireState() *SemanticCacheConfigRow {
	cp := *r
	cp.UpdatedAt = time.Time{}
	cp.UpdatedBy = nil
	return &cp
}

// ErrUnsupportedEmbeddingDimension is returned by Save when the requested
// embedding dimension is not one the chosen model can produce (per its
// capabilityJson.embeddings.supported_dimensions). Surfaced as 400 by the
// admin handler so an operator cannot persist a config the gateway would
// 400 on every embed call (the failure that wedged prod: a 3072 dim on
// text-embedding-3-small, which only supports [512,1024,1536]).
var ErrUnsupportedEmbeddingDimension = errors.New("configstore: embedding dimension not supported by model")

// ErrEmbeddingDimensionRequired is returned by Save when a model is set, no
// dimension was supplied, and the model declares no default_dimension to
// derive one from — so there is nothing to build the index with.
var ErrEmbeddingDimensionRequired = errors.New("configstore: embedding dimension required (model has no default_dimension)")

// embeddingCapability is the parsed embeddings block of a Model's
// capabilityJson. Zero values mean "model declares no constraint", which the
// caller treats as "skip the check" rather than "reject".
type embeddingCapability struct {
	MaxInputTokens      int   `json:"max_input_tokens"`
	DefaultDimension    int   `json:"default_dimension"`
	SupportedDimensions []int `json:"supported_dimensions"`
}

// parseEmbeddingCapability extracts the embeddings capability block from a
// Model.capabilityJson payload. Returns a zero-valued struct on empty or
// unparseable input — capability constraints are advisory, never a hard
// dependency for saving.
func parseEmbeddingCapability(raw []byte) embeddingCapability {
	if len(raw) == 0 {
		return embeddingCapability{}
	}
	var wrap struct {
		Embeddings embeddingCapability `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return embeddingCapability{}
	}
	return wrap.Embeddings
}

// resolveEmbeddingDimension applies the capability to the requested dimension:
//   - nil dimension → derive the model's default_dimension (error if none).
//   - supplied dimension → must be in supported_dimensions when that list is
//     non-empty (else ErrUnsupportedEmbeddingDimension).
//
// A model that declares neither supported_dimensions nor default_dimension is
// treated permissively: any supplied dimension passes (legacy models without
// capability metadata still work). Returns the resolved dimension.
func resolveEmbeddingDimension(requested *int, capb embeddingCapability) (*int, error) {
	if requested == nil {
		if capb.DefaultDimension > 0 {
			d := capb.DefaultDimension
			return &d, nil
		}
		return nil, ErrEmbeddingDimensionRequired
	}
	if len(capb.SupportedDimensions) > 0 {
		ok := false
		for _, d := range capb.SupportedDimensions {
			if d == *requested {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: %d (supported: %v)", ErrUnsupportedEmbeddingDimension, *requested, capb.SupportedDimensions)
		}
	}
	return requested, nil
}

// TimeSensitiveOverridesBlob is the JSONB payload stored in
// semantic_cache_config.time_sensitive_overrides.
//
// This blob IS the canonical rule list. seed.ts populates the default 11
// rules on a fresh DB (tools/db-migrate/seed/fixtures/semantic_cache_config.json,
// the single row's time_sensitive_overrides field);
// admin CRUD operates on the same blob from there on. There are no Go-side
// defaults or fallbacks — if seed didn't run the list is empty and the
// freshness gate is off, which is the correct behaviour.
type TimeSensitiveOverridesBlob struct {
	Rules []TimeSensitiveOverrideRule `json:"rules"`
}

// TimeSensitiveOverrideRule is one admin-edited rule entry inside the overrides
// blob. The json tags must stay in sync with the handler's TimeSensitivePattern
// struct (which is intentionally not imported here to avoid reverse deps).
type TimeSensitiveOverrideRule struct {
	ID                  string   `json:"id"`
	Keywords            []string `json:"keywords"`
	RequireQuestionMark bool     `json:"requireQuestionMark"`
	RequireEntity       bool     `json:"requireEntity"`
	Languages           []string `json:"languages"`
	Enabled             bool     `json:"enabled"`
}

// SaveInput is the caller-supplied mutation spec for SemanticCacheStore.Save.
// EmbeddingFingerprint is NOT a field: Save recomputes it server-side so
// callers cannot accidentally persist a stale value.
type SaveInput struct {
	EmbeddingProviderID *string
	EmbeddingModelID    *string
	EmbeddingDimension  *int
	Enabled             bool
	// Threshold + VaryBy + EmbedStrategy + AllowCrossModel are fleet-wide
	// L2 tuning knobs surfaced after the per-route policy retirement. Save
	// normalizes each value into its allowed range / enum (out-of-range
	// values fall back to schema defaults) rather than rejecting the save.
	Threshold       float64
	VaryBy          string
	EmbedStrategy   string
	AllowCrossModel bool
	UpdatedBy       string
}

// SemanticCacheStore reads and writes the singleton semantic_cache_config row.
// Uses the same PgxPool interface as AIGuardStore (defined in aiguard.go) so
// both stores can be unit-tested via pgxmock without a live Postgres pool.
type SemanticCacheStore struct {
	pool PgxPool
}

// NewSemanticCacheStore returns a store backed by the provided production pool.
func NewSemanticCacheStore(pool *pgxpool.Pool) *SemanticCacheStore {
	return &SemanticCacheStore{pool: pool}
}

// NewSemanticCacheStoreWithPgxPool is the test-only constructor. Production
// callers go through NewSemanticCacheStore; tests pass a pgxmock pool here
// so the SQL paths can be exercised without a live Postgres connection.
func NewSemanticCacheStoreWithPgxPool(pool PgxPool) *SemanticCacheStore {
	return &SemanticCacheStore{pool: pool}
}

// Get returns the singleton config row. If no row exists (fresh DB where
// migration seed was skipped), returns schema defaults.
//
// The SQL Scan is the only DB-bound part; finalizeSemanticCacheGet handles
// the ErrNoRows fallback so it can be unit-tested without a live Postgres
// connection.
func (s *SemanticCacheStore) Get(ctx context.Context) (*SemanticCacheConfigRow, error) {
	const q = `
		SELECT sc.id, sc.embedding_provider_id, sc.embedding_model_id, sc.embedding_dimension,
		       sc.embedding_fingerprint, sc.redis_index_name, sc.enabled,
		       sc.threshold, sc.vary_by, sc.embed_strategy, sc.allow_cross_model,
		       sc.updated_at, sc.updated_by, sc.time_sensitive_overrides,
		       COALESCE(p."baseUrl", '') AS provider_base_url,
		       COALESCE(m."providerModelId", '') AS provider_model_id,
		       COALESCE(m."inputPricePerMillion"::float8, 0) AS provider_input_price_per_m,
		       COALESCE(m."capabilityJson"::text, '') AS model_capability_json
		FROM semantic_cache_config sc
		LEFT JOIN "Provider" p ON p.id = sc.embedding_provider_id
		LEFT JOIN "Model"    m ON m.id = sc.embedding_model_id
		WHERE sc.id = 'singleton'`
	row := &SemanticCacheConfigRow{}
	var rawOverrides []byte
	var capabilityJSON string
	scanErr := s.pool.QueryRow(ctx, q).Scan(
		&row.ID, &row.EmbeddingProviderID, &row.EmbeddingModelID, &row.EmbeddingDimension,
		&row.EmbeddingFingerprint, &row.RedisIndexName, &row.Enabled,
		&row.Threshold, &row.VaryBy, &row.EmbedStrategy, &row.AllowCrossModel,
		&row.UpdatedAt, &row.UpdatedBy,
		&rawOverrides,
		&row.EmbeddingProviderBaseURL, &row.EmbeddingProviderModelID,
		&row.EmbeddingInputPricePerMillion,
		&capabilityJSON,
	)
	if scanErr == nil {
		row.EmbeddingMaxInputTokens = parseEmbeddingCapability([]byte(capabilityJSON)).MaxInputTokens
	}
	if err := parseSemanticCacheScan(row, rawOverrides, scanErr); err != nil {
		return nil, fmt.Errorf("configstore: load semantic_cache_config: %w", err)
	}
	return finalizeSemanticCacheGet(row, scanErr)
}

// parseSemanticCacheScan populates TimeSensitiveOverrides from rawOverrides.
// Called after every Scan to decouple JSONB parsing from the Scan error path.
//
// Returns nil unconditionally when scanErr is non-nil: rawOverrides is
// undefined on a failed Scan and the caller (Get) deliberately forwards
// scanErr to finalizeSemanticCacheGet for ErrNoRows-vs-real-error
// classification. The returned error reflects JSONB-parse failures only.
func parseSemanticCacheScan(row *SemanticCacheConfigRow, rawOverrides []byte, scanErr error) error {
	if scanErr != nil {
		//nolint:nilerr // scanErr is intentionally forwarded by the caller; this function reports JSONB-parse errors only.
		return nil
	}
	if len(rawOverrides) > 0 {
		if err := json.Unmarshal(rawOverrides, &row.TimeSensitiveOverrides); err != nil {
			// Corrupt JSONB — treat as empty blob; do not surface as error since
			// the row is otherwise usable and the GET merges with seed rules.
			row.TimeSensitiveOverrides = TimeSensitiveOverridesBlob{}
		}
	}
	return nil
}

// GetOverrides returns only the TimeSensitiveOverridesBlob from the singleton
// row. Returns an empty blob when no row or overrides exist.
func (s *SemanticCacheStore) GetOverrides(ctx context.Context) (TimeSensitiveOverridesBlob, error) {
	row, err := s.Get(ctx)
	if err != nil {
		return TimeSensitiveOverridesBlob{}, err
	}
	return row.TimeSensitiveOverrides, nil
}

// SaveOverrides persists the time_sensitive_overrides JSONB column on the
// singleton row. It upserts the row (inserting with defaults if not present)
// then updates only the overrides column.
func (s *SemanticCacheStore) SaveOverrides(ctx context.Context, blob TimeSensitiveOverridesBlob) error {
	raw, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("configstore: marshal time_sensitive_overrides: %w", err)
	}
	const q = `
		INSERT INTO semantic_cache_config (id, time_sensitive_overrides)
		VALUES ('singleton', $1)
		ON CONFLICT (id) DO UPDATE SET
			time_sensitive_overrides = EXCLUDED.time_sensitive_overrides,
			updated_at               = NOW()`
	_, err = s.pool.Exec(ctx, q, raw)
	if err != nil {
		return fmt.Errorf("configstore: save time_sensitive_overrides: %w", err)
	}
	return nil
}

// defaultSemanticCacheRow is the conservative singleton fallback returned
// when no row exists yet. Values mirror the schema DEFAULTs so behavior
// is identical to a freshly-seeded row.
func defaultSemanticCacheRow() *SemanticCacheConfigRow {
	return &SemanticCacheConfigRow{
		ID:                     "singleton",
		EmbeddingFingerprint:   "",
		RedisIndexName:         "nexus:semantic-cache:v1",
		Enabled:                false,
		Threshold:              0.96,
		VaryBy:                 "vk",
		EmbedStrategy:          "system_plus_last_user",
		AllowCrossModel:        false,
		TimeSensitiveOverrides: TimeSensitiveOverridesBlob{Rules: []TimeSensitiveOverrideRule{}},
	}
}

// normalizeFleetTuning normalizes the four L2 tuning inputs back to schema
// defaults when out-of-range / unknown values arrive. Threshold clamps to
// (0, 1] (else 0.96); VaryBy and EmbedStrategy each validate against their
// enum (unknown → "vk" / "system_plus_last_user"); AllowCrossModel passes
// through (bool has no invalid value).
func normalizeFleetTuning(threshold float64, varyBy, embedStrategy string) (float64, string, string) {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.96
	}
	switch varyBy {
	case "none", "user", "vk", "org":
		// allowed
	default:
		varyBy = "vk"
	}
	switch embedStrategy {
	case "last_user", "system_plus_last_user", "recent_turns", "head_plus_tail", "full_truncated":
		// allowed
	default:
		embedStrategy = "system_plus_last_user"
	}
	return threshold, varyBy, embedStrategy
}

// finalizeSemanticCacheGet applies the three-way decision over a Scan outcome:
//   - ErrNoRows → schema defaults (fresh DB before seed)
//   - generic error → wrapped error
//   - success → return the row as-is
//
// Split out for unit testability without a real *pgxpool.Pool.
func finalizeSemanticCacheGet(row *SemanticCacheConfigRow, scanErr error) (*SemanticCacheConfigRow, error) {
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return defaultSemanticCacheRow(), nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("configstore: load semantic_cache_config: %w", scanErr)
	}
	return row, nil
}

// Save upserts the singleton config row with the values from in. It always:
//
//  1. Recomputes EmbeddingFingerprint = sha256(providerID:modelID:dim) so
//     callers cannot persist a stale fingerprint value.
//  2. Bumps RedisIndexName from vN to v(N+1) when the fingerprint changes
//     AND the new fingerprint is non-empty (i.e., a valid (provider, model,
//     dim) triplet is configured). The version bump enables atomic blue/green
//     index swaps: the Hub-side flush job drops the old index and creates a
//     fresh one under the new name.
//  3. Keeps RedisIndexName stable when only the Enabled flag changes —
//     a fleet-wide kill-switch flip must not trigger an unnecessary reindex.
//
// Returns the post-save row (with the possibly-bumped index name).
func (s *SemanticCacheStore) Save(ctx context.Context, in SaveInput) (*SemanticCacheConfigRow, error) {
	// Load the current row so we can compare fingerprints and carry the
	// current RedisIndexName forward (bump only when fingerprint differs).
	current, err := s.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("configstore: save semantic_cache_config: load current: %w", err)
	}

	// Resolve the joined provider/model fields up front so they are available
	// for both dimension validation (capability) and the returned snapshot. A
	// join miss leaves the values zero — the gateway logs a warning and skips
	// L2 rather than failing the save.
	var baseURL, providerModelID string
	var inputPricePerMillion float64
	var maxInputTokens int
	var embCap embeddingCapability
	if in.EmbeddingProviderID != nil && in.EmbeddingModelID != nil {
		const joinQ = `
			SELECT COALESCE(p."baseUrl", ''),
			       COALESCE(m."providerModelId", ''),
			       COALESCE(m."inputPricePerMillion"::float8, 0),
			       COALESCE(m."capabilityJson"::text, '')
			FROM "Provider" p, "Model" m
			WHERE p.id = $1 AND m.id = $2`
		var capabilityJSON string
		_ = s.pool.QueryRow(ctx, joinQ, *in.EmbeddingProviderID, *in.EmbeddingModelID).
			Scan(&baseURL, &providerModelID, &inputPricePerMillion, &capabilityJSON)
		embCap = parseEmbeddingCapability([]byte(capabilityJSON))
		maxInputTokens = embCap.MaxInputTokens
	}

	// Validate / derive the embedding dimension against the model's capability
	// before it is baked into the fingerprint and index.
	dim := in.EmbeddingDimension
	if in.EmbeddingProviderID != nil && *in.EmbeddingProviderID != "" &&
		in.EmbeddingModelID != nil && *in.EmbeddingModelID != "" {
		resolved, derr := resolveEmbeddingDimension(in.EmbeddingDimension, embCap)
		if derr != nil {
			return nil, derr
		}
		dim = resolved
	}

	newFP := computeSemanticFingerprint(in.EmbeddingProviderID, in.EmbeddingModelID, dim)
	indexName := current.RedisIndexName
	if indexName == "" {
		indexName = defaultIndexName()
	}

	// Bump index name only when the fingerprint actually changes AND the new
	// config is non-empty (non-empty fingerprint signals a valid triplet).
	// Enabled-only edits leave the index name unchanged so a fleet kill-switch
	// toggle does not trigger an unnecessary FT.DROPINDEX + FT.CREATE.
	if newFP != "" && newFP != current.EmbeddingFingerprint {
		indexName = bumpIndexVersion(indexName)
	}

	updatedBy := in.UpdatedBy
	var updatedByPtr *string
	if updatedBy != "" {
		updatedByPtr = &updatedBy
	}

	threshold, varyBy, embedStrategy := normalizeFleetTuning(in.Threshold, in.VaryBy, in.EmbedStrategy)

	const q = `
		INSERT INTO semantic_cache_config (
			id, embedding_provider_id, embedding_model_id, embedding_dimension,
			embedding_fingerprint, redis_index_name, enabled,
			threshold, vary_by, embed_strategy, allow_cross_model,
			updated_at, updated_by
		) VALUES ('singleton', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), $11)
		ON CONFLICT (id) DO UPDATE SET
			embedding_provider_id = EXCLUDED.embedding_provider_id,
			embedding_model_id    = EXCLUDED.embedding_model_id,
			embedding_dimension   = EXCLUDED.embedding_dimension,
			embedding_fingerprint = EXCLUDED.embedding_fingerprint,
			redis_index_name      = EXCLUDED.redis_index_name,
			enabled               = EXCLUDED.enabled,
			threshold             = EXCLUDED.threshold,
			vary_by               = EXCLUDED.vary_by,
			embed_strategy        = EXCLUDED.embed_strategy,
			allow_cross_model     = EXCLUDED.allow_cross_model,
			updated_at            = NOW(),
			updated_by            = EXCLUDED.updated_by`
	_, err = s.pool.Exec(ctx, q,
		in.EmbeddingProviderID, in.EmbeddingModelID, dim,
		newFP, indexName, in.Enabled,
		threshold, varyBy, embedStrategy, in.AllowCrossModel,
		updatedByPtr,
	)
	if err != nil {
		return nil, fmt.Errorf("configstore: save semantic_cache_config: %w", err)
	}

	// The provider/model fields (baseURL, providerModelID, price, capability)
	// were resolved up front; the Hub-pushed snapshot carries them so the
	// gateway L2 path reads them straight from ConfigSnapshot.
	saved := &SemanticCacheConfigRow{
		ID:                            "singleton",
		EmbeddingProviderID:           in.EmbeddingProviderID,
		EmbeddingModelID:              in.EmbeddingModelID,
		EmbeddingDimension:            dim,
		EmbeddingFingerprint:          newFP,
		RedisIndexName:                indexName,
		Enabled:                       in.Enabled,
		Threshold:                     threshold,
		VaryBy:                        varyBy,
		EmbedStrategy:                 embedStrategy,
		AllowCrossModel:               in.AllowCrossModel,
		UpdatedAt:                     time.Now().UTC(),
		UpdatedBy:                     updatedByPtr,
		TimeSensitiveOverrides:        current.TimeSensitiveOverrides,
		EmbeddingProviderBaseURL:      baseURL,
		EmbeddingProviderModelID:      providerModelID,
		EmbeddingInputPricePerMillion: inputPricePerMillion,
		EmbeddingMaxInputTokens:       maxInputTokens,
	}
	return saved, nil
}

// defaultIndexName returns the base Redis Vector index name for the singleton
// fleet-wide row. Kept as a function (rather than a const) so future changes
// to the naming scheme have a single point of edit.
func defaultIndexName() string {
	return "nexus:semantic-cache:v1"
}

// computeSemanticFingerprint returns sha256(providerID:modelID:dim) as a hex
// string. Returns "" when any of the three components is nil/zero, signalling
// "no valid embedding config" so the caller knows not to bump the index name.
func computeSemanticFingerprint(providerID, modelID *string, dim *int) string {
	if providerID == nil || modelID == nil || dim == nil || *dim == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(*providerID))
	h.Write([]byte{':'})
	h.Write([]byte(*modelID))
	h.Write([]byte{':'})
	h.Write([]byte(strconv.Itoa(*dim)))
	return hex.EncodeToString(h.Sum(nil))
}

// indexVersionRe matches a trailing ":vN" suffix (N is one or more digits).
var indexVersionRe = regexp.MustCompile(`:v(\d+)$`)

// bumpIndexVersion increments the trailing ":vN" version suffix by one.
//
// Examples:
//
//	"nexus:semantic-cache:v1"            → "nexus:semantic-cache:v2"
//	"nexus:semantic-cache:v9"            → "nexus:semantic-cache:v10"
//	"nexus:semantic-cache"               → "nexus:semantic-cache:v2"
func bumpIndexVersion(name string) string {
	m := indexVersionRe.FindStringSubmatchIndex(name)
	if m == nil {
		// No trailing :vN — append :v2 (treat as "was v1 before we started").
		return name + ":v2"
	}
	// m[2]:m[3] is the capture group for the digits after ":v".
	n, _ := strconv.Atoi(name[m[2]:m[3]])
	prefix := name[:m[0]] // everything before ":vN"
	return fmt.Sprintf("%s:v%d", prefix, n+1)
}
