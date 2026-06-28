package trafficstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// NormalizeInput carries the raw captured bodies + the routing metadata the
// normalize registry needs to recompute the canonical payload at VIEW TIME
// (instead of reading the stored traffic_event_normalized sidecar). Bodies are
// already decoded from their stored column form (text / base64 / zstd) and are
// redaction-safe — the storage-governance pass redacts every persisted copy, so
// recomputing from these bytes never exposes redacted content. Empty bodies mean
// "nothing captured for that direction" (capture off or spilled out-of-band).
type NormalizeInput struct {
	AdapterType         string
	Model               string
	Path                string
	RequestBody         []byte
	ResponseBody        []byte
	RequestContentType  string
	ResponseContentType string
	Found               bool // false when the traffic_event row does not exist
}

// GetTrafficEventForNormalize fetches the raw captured request/response bodies
// (decoded from their column encoding) plus the adapter type, model, path, and
// content types for a traffic event, so the caller can recompute the normalized
// payload on the fly. Returns Found=false when the row does not exist.
func (store *Store) GetTrafficEventForNormalize(ctx context.Context, id string) (*NormalizeInput, error) {
	// The captured request + response bodies are stored in the INGRESS
	// (client-facing) wire frame, so they MUST be decoded with the ingress
	// format + ingress path — NOT the upstream provider's adapter_type +
	// target_path (which describe the bytes sent upstream, a body never
	// stored). Using the provider adapter mis-selects the codec for every
	// cross-protocol route (client speaks Anthropic/Gemini, routed elsewhere),
	// falling through to the generic-http catch-all (raw JSON, wrong tier).
	// ingress_format is producer-authoritative (ai-gateway stamps it);
	// compliance-proxy / agent rows leave it '' → the registry's path-only
	// fallback + content sniffers resolve them exactly as before.
	// See traffic-capture-storage-normalize-design.md.
	const q = `
		SELECT COALESCE(a.ingress_format, ''),
		       COALESCE(a.model_name, a.routed_model_name, ''),
		       COALESCE(a.path, ''),
		       p.inline_request_body,  COALESCE(p.inline_request_encoding, ''),
		       p.inline_response_body, COALESCE(p.inline_response_encoding, ''),
		       COALESCE(p.request_content_type, ''), COALESCE(p.response_content_type, '')
		FROM   traffic_event a
		LEFT JOIN traffic_event_payload p ON p.traffic_event_id = a.id
		WHERE  a.id = $1
	`
	var (
		out                  NormalizeInput
		reqCol, respCol      []byte
		reqEncoding, respEnc string
	)
	err := store.pool.QueryRow(ctx, q, id).Scan(
		&out.AdapterType, &out.Model, &out.Path,
		&reqCol, &reqEncoding, &respCol, &respEnc,
		&out.RequestContentType, &out.ResponseContentType,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return &NormalizeInput{Found: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get traffic event for normalize: %w", err)
	}
	out.Found = true
	out.RequestBody = sharedaudit.DecodeBodyForColumn(reqCol, reqEncoding)
	out.ResponseBody = sharedaudit.DecodeBodyForColumn(respCol, respEnc)
	return &out, nil
}

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
