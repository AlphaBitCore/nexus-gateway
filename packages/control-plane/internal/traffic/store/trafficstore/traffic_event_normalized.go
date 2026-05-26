package trafficstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrafficEventNormalized mirrors the traffic_event_normalized table.
// JSON tags match the OpenAPI schema in docs/users/api/openapi/ai-gateway/e46-s2-aigw-openai.yaml.
type TrafficEventNormalized struct {
	TrafficEventID         string          `json:"trafficEventId"`
	RequestNormalized      json.RawMessage `json:"requestNormalized,omitempty"`
	ResponseNormalized     json.RawMessage `json:"responseNormalized,omitempty"`
	RequestStatus          *string         `json:"requestStatus,omitempty"`
	ResponseStatus         *string         `json:"responseStatus,omitempty"`
	RequestErrorReason     *string         `json:"requestErrorReason,omitempty"`
	ResponseErrorReason    *string         `json:"responseErrorReason,omitempty"`
	RequestRedactionSpans  json.RawMessage `json:"requestRedactionSpans,omitempty"`
	ResponseRedactionSpans json.RawMessage `json:"responseRedactionSpans,omitempty"`
	NormalizeVersion       string          `json:"normalizeVersion"`
	CreatedAt              time.Time       `json:"createdAt"`
}

// GetTrafficEventNormalized returns the normalized payload sidecar row
// for the given traffic event id, or nil when no normalize row exists.
//
// The parent traffic_event existence is NOT verified here; callers (the
// admin handler) treat (nil, nil) as 404 regardless of which row is
// missing — there is no business reason to distinguish "no traffic event"
// from "traffic event exists but was not normalized".
func (store *Store) GetTrafficEventNormalized(ctx context.Context, id string) (*TrafficEventNormalized, error) {
	const q = `
		SELECT traffic_event_id,
		       request_normalized, response_normalized,
		       request_status, response_status,
		       request_error_reason, response_error_reason,
		       request_redaction_spans, response_redaction_spans,
		       normalize_version, created_at
		FROM traffic_event_normalized
		WHERE traffic_event_id = $1
	`
	var out TrafficEventNormalized
	err := store.pool.QueryRow(ctx, q, id).Scan(
		&out.TrafficEventID,
		&out.RequestNormalized, &out.ResponseNormalized,
		&out.RequestStatus, &out.ResponseStatus,
		&out.RequestErrorReason, &out.ResponseErrorReason,
		&out.RequestRedactionSpans, &out.ResponseRedactionSpans,
		&out.NormalizeVersion, &out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get traffic event normalized: %w", err)
	}
	return &out, nil
}
