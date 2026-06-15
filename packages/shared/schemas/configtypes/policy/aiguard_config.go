// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package policy

import (
	"encoding/json"
	"time"
)

// AIGuardConfigTableName is the PostgreSQL table name for this model.
const AIGuardConfigTableName = "ai_guard_config"

// AIGuardConfig -- generated from schema.prisma model.
type AIGuardConfig struct {
	Id                  string          `db:"id"`
	Backend_mode        string          `db:"backend_mode"`
	Provider_id         *string         `db:"provider_id"`
	Model_id            *string         `db:"model_id"`
	External_url        *string         `db:"external_url"`
	Custom_headers      json.RawMessage `db:"custom_headers"`
	Prompt_template     string          `db:"prompt_template"`
	Timeout_ms          int32           `db:"timeout_ms"`
	Cache_ttl_seconds   int32           `db:"cache_ttl_seconds"`
	Backend_fingerprint string          `db:"backend_fingerprint"`
	// Input_strategy is the inputstaging strategy for classify input truncation.
	// One of the five inputstaging.Strategy constants. Default "system_plus_last_user".
	Input_strategy string `db:"input_strategy"`
	// Model_context_limit is the judge model context window in tokens. 0 = unknown, pipeline falls back to 8192.
	Model_context_limit int32     `db:"model_context_limit"`
	Updated_at          time.Time `db:"updated_at"`
}
