// Hand-maintained Go mirror of the corresponding schema.prisma model. Keep in lockstep with schema changes — see docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md §5.

package interception

import (
	"encoding/json"
	"time"
)

// InterceptionDomainTableName is the PostgreSQL table name for this model.
const InterceptionDomainTableName = "interception_domain"

// InterceptionDomain -- generated from schema.prisma model.
type InterceptionDomain struct {
	Id                      string            `db:"id"`
	Name                    string            `db:"name"`
	Description             *string           `db:"description"`
	HostPattern             string            `db:"host_pattern"`
	HostMatchType           HostMatchType     `db:"host_match_type"`
	AdapterId               string            `db:"adapter_id"`
	AdapterConfig           json.RawMessage   `db:"adapter_config"`
	Enabled                 bool              `db:"enabled"`
	Priority                int32             `db:"priority"`
	DefaultPathAction       DefaultPathAction `db:"default_path_action"`
	OnAdapterError          FailureAction     `db:"on_adapter_error"`
	NetworkZone             NetworkZone       `db:"network_zone"`
	Source                  string            `db:"source"`
	StreamingMode           *string           `db:"streaming_mode"`
	StreamingChunkBytes     *int32            `db:"streaming_chunk_bytes"`
	StreamingHookTimeoutMs  *int32            `db:"streaming_hook_timeout_ms"`
	StreamingMaxBufferBytes *int32            `db:"streaming_max_buffer_bytes"`
	StreamingFailBehavior   *string           `db:"streaming_fail_behavior"`
	CaptureRequestBody      *bool             `db:"capture_request_body"`
	CaptureResponseBody     *bool             `db:"capture_response_body"`
	RawBodySpillEnabled     *bool             `db:"raw_body_spill_enabled"`
	CreatedAt               time.Time         `db:"created_at"`
	UpdatedAt               time.Time         `db:"updated_at"`
	CreatedBy               *string           `db:"created_by"`
}
