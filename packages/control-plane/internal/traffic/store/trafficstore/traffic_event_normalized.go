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
	// Request/ResponseSpillRef carry the JSONB spill_ref for a direction whose
	// body was stored out-of-band (large payload). They are nil when the body
	// is inline or absent. The view-time handler fetches the RAW spilled bytes
	// via the spill store and recomputes from them — so a spilled row normalizes
	// exactly like a captured-inline row, not just falls back to the sidecar.
	RequestSpillRef  json.RawMessage
	ResponseSpillRef json.RawMessage
	// EndpointType is the request modality (traffic_event.endpoint_type) — the
	// canonical discriminator the artifact endpoint switches on to decide how to
	// extract a previewable artifact from the captured body.
	EndpointType string
	// ArtifactRefs is traffic_event.artifact_refs: the fingerprints of binary
	// payloads the gateway hashed but did not keep. When a request body was
	// never stored, this is the only record that the request carried one.
	ArtifactRefs string
	// Request/ResponseTruncated say the stored body for that direction is only a
	// PREFIX (it reached payload_capture.maxInlineBodyBytes with no spill backend
	// configured). The view-time recompute still runs — a prefix normalizes to
	// whatever it contains — but the result describes an incomplete payload, and
	// silently presenting a partial conversation as a whole one is exactly the
	// failure the flag exists to prevent.
	RequestTruncated  bool
	ResponseTruncated bool
	Found             bool // false when the traffic_event row does not exist
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
	// The spill refs (request_spill_ref / response_spill_ref) ride alongside the
	// inline bodies so the view-time handler can fetch the RAW spilled bytes for a
	// direction whose body was stored out-of-band and recompute from them, instead
	// of only falling back to the stored sidecar.
	const q = `
		SELECT COALESCE(a.ingress_format, ''),
		       COALESCE(a.model_name, a.routed_model_name, ''),
		       COALESCE(a.path, ''),
		       p.inline_request_body,  COALESCE(p.inline_request_encoding, ''),
		       p.inline_response_body, COALESCE(p.inline_response_encoding, ''),
		       COALESCE(p.request_content_type, ''), COALESCE(p.response_content_type, ''),
		       p.request_spill_ref, p.response_spill_ref,
		       COALESCE(a.endpoint_type, ''),
		       COALESCE(a.artifact_refs, ''),
		       COALESCE(p.request_truncated, false), COALESCE(p.response_truncated, false)
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
		&out.RequestSpillRef, &out.ResponseSpillRef,
		&out.EndpointType,
		&out.ArtifactRefs,
		&out.RequestTruncated, &out.ResponseTruncated,
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
	// RequestTruncated / ResponseTruncated are NOT columns of
	// traffic_event_normalized — they are view-time provenance, carried from
	// traffic_event_payload so the drawer can say that this projection was
	// computed from a body that is only a prefix. A partial conversation
	// rendered as a complete one is indistinguishable from a model that stopped
	// early, which is the whole reason the flag is plumbed this far.
	RequestTruncated  bool `json:"requestTruncated"`
	ResponseTruncated bool `json:"responseTruncated"`
}
