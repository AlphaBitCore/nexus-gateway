package configstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AIGuardConfig mirrors the ai_guard_config table row with idiomatic Go
// field names + JSON tags for API transport.
type AIGuardConfig struct {
	ID                 string         `json:"id"`
	BackendMode        string         `json:"backendMode"`
	ProviderID         *string        `json:"providerId,omitempty"`
	ModelID            *string        `json:"modelId,omitempty"`
	ExternalURL        *string        `json:"externalUrl,omitempty"`
	CustomHeaders      map[string]any `json:"customHeaders,omitempty"`
	PromptTemplate     string         `json:"promptTemplate"`
	TimeoutMs          int            `json:"timeoutMs"`
	CacheTTLSeconds    int            `json:"cacheTtlSeconds"`
	BackendFingerprint string         `json:"backendFingerprint"`
	// InputStrategy controls which portion of the conversation is
	// sent to the classifier. One of: last_user, system_plus_last_user,
	// recent_turns, head_plus_tail, full_truncated. Defaults to
	// "system_plus_last_user".
	InputStrategy string `json:"inputStrategy"`
	// ModelContextLimit is the judge model's context window in tokens.
	// 0 = unknown / not configured; classify falls back to 8192 when zero.
	ModelContextLimit int `json:"modelContextLimit"`
}

// PgxPool is the minimum pgx pool surface AIGuardStore needs. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests, letting the SQL paths be
// unit-tested without a live Postgres. Mirrors the PgxPool seam in
// packages/control-plane/internal/store and packages/nexus-hub/internal/store.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// AIGuardStore reads and writes the singleton ai_guard_config row.
type AIGuardStore struct {
	pool PgxPool
}

// NewAIGuardStore returns a store backed by the provided pool.
func NewAIGuardStore(pool *pgxpool.Pool) *AIGuardStore {
	return &AIGuardStore{pool: pool}
}

// NewAIGuardStoreWithPgxPool is the test-only constructor. Production
// callers go through NewAIGuardStore; tests pass a pgxmock pool here so
// the SQL paths can be exercised without a live Postgres connection.
func NewAIGuardStoreWithPgxPool(pool PgxPool) *AIGuardStore {
	return &AIGuardStore{pool: pool}
}

// Load returns the current singleton row. If the row does not exist yet,
// returns the schema defaults (defensive for fresh DBs where the migration
// seed may have been skipped).
//
// The SQL Scan is the only DB-bound part; the post-scan decision tree
// (ErrNoRows → defaults, generic err → wrap, headers → JSON parse)
// lives in finalizeAIGuardLoad so it can be unit-tested without a live
// Postgres connection.
func (s *AIGuardStore) Load(ctx context.Context) (*AIGuardConfig, error) {
	const q = `
		SELECT id, backend_mode, provider_id, model_id, external_url,
		       custom_headers, prompt_template,
		       timeout_ms, cache_ttl_seconds, backend_fingerprint,
		       input_strategy, model_context_limit
		FROM ai_guard_config WHERE id = 'singleton'`
	cfg := &AIGuardConfig{}
	var headersJSON []byte
	err := s.pool.QueryRow(ctx, q).Scan(
		&cfg.ID, &cfg.BackendMode, &cfg.ProviderID, &cfg.ModelID, &cfg.ExternalURL,
		&headersJSON, &cfg.PromptTemplate,
		&cfg.TimeoutMs, &cfg.CacheTTLSeconds, &cfg.BackendFingerprint,
		&cfg.InputStrategy, &cfg.ModelContextLimit,
	)
	return finalizeAIGuardLoad(cfg, headersJSON, err)
}

// defaultAIGuardConfig is the conservative singleton fallback returned
// when no row exists yet (fresh deploys where the migration seed was
// skipped). The BackendMode + timeouts mirror the schema DEFAULTs so
// behaviour is identical to a freshly-seeded row.
func defaultAIGuardConfig() *AIGuardConfig {
	return &AIGuardConfig{
		ID: "singleton", BackendMode: "configured_provider",
		TimeoutMs: 5000, CacheTTLSeconds: 600,
		InputStrategy: "system_plus_last_user", ModelContextLimit: 0,
	}
}

// finalizeAIGuardLoad applies the three-way decision over a Scan
// outcome: ErrNoRows → defaults, generic err → wrapped err, success
// → JSON-decode headers (if any) and return. Split out so unit tests
// can drive every branch without a real *pgxpool.Pool.
func finalizeAIGuardLoad(cfg *AIGuardConfig, headersJSON []byte, scanErr error) (*AIGuardConfig, error) {
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return defaultAIGuardConfig(), nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("configstore: load ai_guard_config: %w", scanErr)
	}
	if len(headersJSON) > 0 {
		if err := json.Unmarshal(headersJSON, &cfg.CustomHeaders); err != nil {
			return nil, fmt.Errorf("configstore: parse custom_headers: %w", err)
		}
	}
	return cfg, nil
}

// Save upserts the singleton row. Callers must recompute BackendFingerprint
// before calling — the store does not derive it.
//
// The JSON-marshal of CustomHeaders lives in marshalAIGuardHeaders so
// unit tests can drive both the nil-map happy path and the
// unmarshalable-payload error path without a live DB.
func (s *AIGuardStore) Save(ctx context.Context, cfg *AIGuardConfig) error {
	if cfg.ID == "" {
		cfg.ID = "singleton"
	}
	headersJSON, err := marshalAIGuardHeaders(cfg.CustomHeaders)
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO ai_guard_config (
			id, backend_mode, provider_id, model_id, external_url,
			custom_headers, prompt_template,
			timeout_ms, cache_ttl_seconds, backend_fingerprint,
			input_strategy, model_context_limit, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, NOW())
		ON CONFLICT (id) DO UPDATE SET
			backend_mode = EXCLUDED.backend_mode,
			provider_id = EXCLUDED.provider_id,
			model_id = EXCLUDED.model_id,
			external_url = EXCLUDED.external_url,
			custom_headers = EXCLUDED.custom_headers,
			prompt_template = EXCLUDED.prompt_template,
			timeout_ms = EXCLUDED.timeout_ms,
			cache_ttl_seconds = EXCLUDED.cache_ttl_seconds,
			backend_fingerprint = EXCLUDED.backend_fingerprint,
			input_strategy = EXCLUDED.input_strategy,
			model_context_limit = EXCLUDED.model_context_limit,
			updated_at = NOW()`
	_, err = s.pool.Exec(ctx, q,
		cfg.ID, cfg.BackendMode, cfg.ProviderID, cfg.ModelID, cfg.ExternalURL,
		headersJSON, cfg.PromptTemplate,
		cfg.TimeoutMs, cfg.CacheTTLSeconds, cfg.BackendFingerprint,
		cfg.InputStrategy, cfg.ModelContextLimit,
	)
	if err != nil {
		return fmt.Errorf("configstore: save ai_guard_config: %w", err)
	}
	return nil
}

// marshalAIGuardHeaders encodes the custom-headers map for the
// jsonb column. A nil map encodes as a NULL-shaped nil byte slice
// (NOT the JSON literal `null`) so the Postgres jsonb column
// distinguishes "operator omitted headers" from `{}`. An
// unmarshalable map (channels, functions) surfaces a wrapped err so
// the admin UI knows to refuse the save before the SQL fires.
func marshalAIGuardHeaders(headers map[string]any) ([]byte, error) {
	if headers == nil {
		return nil, nil
	}
	b, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("configstore: marshal custom_headers: %w", err)
	}
	return b, nil
}
