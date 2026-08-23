package traffic

import (
	"context"
	stdjson "encoding/json"
	"fmt"

	"github.com/goccy/go-json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RegisterTrafficRoutes registers traffic event and admin audit log routes.
func (h *Handler) RegisterTrafficRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/traffic", h.ListTrafficEvents, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	// Error-governance aggregation over traffic_event — same read action as
	// the raw list: the view is a lens over the same rows.
	g.GET("/traffic/errors/groups", h.ListTrafficErrorGroups, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	g.GET("/traffic/:id", h.GetTrafficEvent, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	// Normalized sidecar for a single traffic event. Returns the canonical
	// NormalizedPayload(s) plus normalize status / error reason / redaction spans.
	// Gated by the same read action as /traffic/:id; no separate IAM resource.
	g.GET("/traffic/:id/normalized", h.GetTrafficEventNormalized, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	// Streams the captured multimodal artifact bytes (image/audio) for inline
	// preview. Gated by the same read action as /traffic/:id; no separate IAM
	// resource.
	g.GET("/traffic/:id/artifact", h.GetTrafficEventArtifact, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	g.GET("/traffic/storage", h.TrafficStorage, iamMW(iam.ResourceTrafficLog.Action(iam.VerbRead)))
	// Admin audit log routes (separate concern)
	g.GET("/admin-audit-logs", h.ListAdminAuditLogs, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	g.GET("/admin-audit-logs/export", h.ExportAdminAuditLogs, iamMW(iam.ResourceAuditLog.Action(iam.VerbExport)))
	g.GET("/me/admin-audit-logs", h.ListMyAdminAuditLogs) // iam-exempt: self-service, the caller's own audit log
}

// GetTrafficEventNormalized returns the canonical normalized payload for the
// given traffic event id, COMPUTED ON THE FLY from the captured request /
// response bodies. View-time recompute is the primary source for every row that
// still has a recoverable body — it does not read the stored
// traffic_event_normalized sidecar except as a last resort. Recomputing at view
// time means the normalized form is never persisted: the write path stays thin
// and the drawer always reflects the current normalize version. The captured
// bodies are decoded from their column form (text / base64 / zstd) and are
// already redaction-safe (the storage governance pass redacts every persisted
// copy), so recompute never exposes redacted content.
//
// Resolution is a 3-tier ladder, evaluated per direction for (a)+(b):
//
//	(a) inline body present       → recompute from the inline bytes
//	(b) else a spill ref present  → fetch the RAW spilled bytes and recompute
//	(c) else                      → 404 "normalized payload not found" (unavailable)
//
// A spill fetch that fails (object aged out to retention, integrity mismatch)
// degrades that direction to empty and the row falls through to (c); a missing
// spill object never errors the endpoint.
func (h *Handler) GetTrafficEventNormalized(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	in, err := h.traffic.GetTrafficEventForNormalize(ctx, id)
	if err != nil {
		h.logger.Error("get traffic event for normalize", "trafficEventId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if in == nil || !in.Found {
		return c.JSON(http.StatusNotFound, errJSON("Traffic event not found", "not_found", ""))
	}

	// Tiers (a)+(b): recompute from the captured body. Inline is preferred; a
	// direction whose body spilled out-of-band is recovered by fetching the RAW
	// spilled bytes (fail-graceful — a gone/tampered blob leaves that direction
	// empty and falls through to the sidecar tier).
	if h.normalize != nil {
		h.fillSpilledBodies(ctx, id, in)
		if len(in.RequestBody) > 0 || len(in.ResponseBody) > 0 {
			return c.JSON(http.StatusOK, h.computeNormalized(id, in))
		}
	}

	// Tier (c): nothing inline and nothing recoverable from spill — capture was
	// off for this row, or every spilled body has gone to retention. There is
	// nothing left to recompute from and no stored sidecar to fall back on:
	// traffic_event_normalized is being dropped (e88-multimodal-gateway.md NFR-1
	// puts it on the GDPR Art.17 erasure trajectory), and nothing has written a
	// row to it since 2026-06-26.
	//
	// The cost is bounded and worth stating: a row whose body aged out but which
	// still carries a historical sidecar used to render in the drawer and now
	// answers 404. That is what dropping the table means — the alternative is
	// keeping an un-erasable copy of request content alive for a handful of
	// pre-2026-06 rows.
	return c.JSON(http.StatusNotFound, errJSON("Normalized payload not found", "not_found", ""))
}

// computeNormalized runs the shared normalize chain over the captured bodies and
// assembles the same TrafficEventNormalized shape the stored sidecar would have,
// minus redaction spans (the recompute input is already redaction-safe). A
// text/event-stream response content type marks the response direction as a
// stream so the SSE codecs are selected.
func (h *Handler) computeNormalized(id string, in *trafficstore.NormalizeInput) trafficstore.TrafficEventNormalized {
	out := trafficstore.TrafficEventNormalized{
		TrafficEventID:   id,
		NormalizeVersion: normcore.SchemaVersion,
		CreatedAt:        time.Now().UTC(),
		// Provenance, not content: this projection may have been computed from a
		// stored PREFIX. Carried through so the drawer's normalized tab can say
		// so — the raw tab already does, and the two must not disagree.
		RequestTruncated:  in.RequestTruncated,
		ResponseTruncated: in.ResponseTruncated,
	}
	if len(in.RequestBody) > 0 {
		raw, status, reason := h.normalize("request", in.RequestContentType, in.AdapterType, in.Model, in.Path, false, in.RequestBody)
		out.RequestNormalized = raw
		out.RequestStatus = strPtr(status)
		if reason != "" {
			out.RequestErrorReason = strPtr(reason)
		}
	}
	if len(in.RequestBody) == 0 && in.ArtifactRefs != "" {
		// The request carried a binary the gateway hashed and forwarded
		// without keeping a copy — STT audio, a video input reference. The
		// fingerprint is the only record it existed, so it becomes a media
		// element in the request payload like any other.
		//
		// The discriminator is "we stored no request body yet recorded a
		// fingerprint", not a list of endpoint kinds. TTS and image
		// generation also write artifact_refs, but their refs describe the
		// RESPONSE and their JSON request body IS stored — so this arm does
		// not fire for them, and their artifacts stay in the one place they
		// already are rather than appearing a second time with a different
		// custody state.
		if raw := fingerprintRequestPayload(in.ArtifactRefs); raw != nil {
			out.RequestNormalized = raw
			out.RequestStatus = strPtr("ok")
		}
	}
	if len(in.ResponseBody) > 0 {
		stream := strings.Contains(strings.ToLower(in.ResponseContentType), "text/event-stream")
		raw, status, reason := h.normalize("response", in.ResponseContentType, in.AdapterType, in.Model, in.Path, stream, in.ResponseBody)
		out.ResponseNormalized = raw
		out.ResponseStatus = strPtr(status)
		if reason != "" {
			out.ResponseErrorReason = strPtr(reason)
		}
	}
	return out
}

// TrafficStorage returns the traffic storage configuration (database-backed = queryable).
func (h *Handler) TrafficStorage(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"traffic": map[string]any{"enabled": true, "sink": "database", "queryable": true},
	})
}

// ListTrafficEvents returns a paginated, filtered list of traffic events.
// The `source` query param accepts product domains (vk|proxy|agent); empty
// means "all data-plane traffic". Unknown values yield an empty DB filter,
// which the store interprets as "all data-plane sources".
func (h *Handler) ListTrafficEvents(c echo.Context) error {
	pg := parsePagination(c)
	params := trafficstore.TrafficEventListParams{
		DBSources: parseTrafficDomainParam(c.QueryParam("source")),
		Provider:  c.QueryParam("provider"),
		// entity_id is the subject column (NexusUser.id for AI Gateway,
		// VK owner for compliance proxy). thing_id is the Thing that
		// emitted the row — for the agent path that's the agent device.
		// Route the `deviceId` query param to thing_id so the global
		// traffic search returns rows uploaded by that agent; keep
		// `entityId` and `userId` on entity_id for non-agent traffic.
		EntityID:     firstNonEmpty(c.QueryParam("entityId"), c.QueryParam("userId")),
		ThingID:      firstNonEmpty(c.QueryParam("thingId"), c.QueryParam("deviceId")),
		OrgID:        c.QueryParam("orgId"),
		EntityType:   c.QueryParam("entityType"),
		ProjectID:    c.QueryParam("projectId"),
		VirtualKeyID: c.QueryParam("virtualKeyId"),
		ModelUsed:    c.QueryParam("modelUsed"),
		ModelExact:   c.QueryParam("modelExact"),
		EndpointType: c.QueryParam("endpointType"),
		RequestID:    c.QueryParam("requestId"),
		// Caller-declared correlation tags (X-Nexus-End-User-Id /
		// X-Nexus-Session-Id at ingress). Exact match, indexed.
		EndUserID:             c.QueryParam("endUserId"),
		SessionID:             c.QueryParam("sessionId"),
		HookDecision:          c.QueryParam("hookDecision"),
		ResponseHookDecision:  c.QueryParam("responseHookDecision"),
		StatusRange:           c.QueryParam("statusRange"),
		TargetHost:            c.QueryParam("targetHost"),
		Path:                  c.QueryParam("path"),
		SourceProcess:         c.QueryParam("sourceProcess"),
		BumpStatus:            c.QueryParam("bumpStatus"),
		ComplianceTags:        parseComplianceTagParams(c),
		APIKeyFingerprint:     c.QueryParam("apiKeyFingerprint"),
		UsageExtractionStatus: c.QueryParam("usageExtractionStatus"),
		RoutingRuleID:         c.QueryParam("routingRuleId"),
		ErrorCode:             c.QueryParam("errorCode"),
		Limit:                 pg.Limit,
		Offset:                pg.Offset,
	}

	if v := c.QueryParam("statusCode"); v != "" {
		if code, err := strconv.Atoi(v); err == nil && code >= 100 && code <= 599 {
			params.StatusCode = &code
		}
	}
	if v, err := parseCacheStatusParam(c.QueryParam("cacheStatus")); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid cacheStatus", "invalid_cache_status", err.Error()))
	} else if v != nil {
		params.CacheStatus = v
	}
	// AI-Guard classify calls persist as traffic_event rows tagged with
	// internal_purpose='ai-guard'. Those rows are operational traffic and
	// would distort customer billing/cost analytics if shown by default, so
	// the admin traffic list hides them unless the caller explicitly opts
	// in via `?excludeInternal=false`. Any other value (including empty)
	// keeps the default-on filter.
	params.ExcludeInternal = parseExcludeInternalParam(c.QueryParam("excludeInternal"))
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.StartTime = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.EndTime = &t
		}
	}

	data, total, err := h.traffic.ListTrafficEvents(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list traffic events", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total, "limit": pg.Limit, "offset": pg.Offset})
}

// parseComplianceTagParams reads the repeatable `?tag=<value>` query
// parameter into a deduplicated slice. Empty strings are dropped. Returns
// nil when no tags are supplied so the store skips the tag filter entirely.
func parseComplianceTagParams(c echo.Context) []string {
	raw := c.QueryParams()["tag"]
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseTrafficDomainParam maps the UI `source` query param (vk|proxy|agent)
// to the DB source values written by each data-plane binary. Returns nil
// for empty/invalid input; the store treats nil as "all sources".
func parseTrafficDomainParam(raw string) []string {
	if raw == "" {
		return nil
	}
	d, ok := domain.ParseTrafficDomain(raw)
	if !ok {
		return nil
	}
	return domain.DBSourcesFor(d)
}

// parseCacheStatusParam validates the `cacheStatus` query parameter
// against the unified cache_status enum (HIT | MISS). Empty input
// returns (nil, nil) — no filter applied. Any other value returns
// (nil, error) and the caller MUST return HTTP 400.
//
// The old internal values (HIT_LIVE, DISABLED, SKIP_NO_CACHE, PASSTHROUGH_SKIP)
// are explicitly rejected — drill-down on those gateway-internal states is the
// audit drawer's job, not a filter.
func parseCacheStatusParam(raw string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	switch raw {
	case "HIT", "MISS":
		v := raw
		return &v, nil
	default:
		return nil, fmt.Errorf("invalid value %q (must be HIT or MISS)", raw)
	}
}

// parseExcludeInternalParam keeps backward compatibility with the original
// default (exclude internal rows). Both empty and false-like inputs keep
// excluding internal traffic, which means rows with NULL/” internal_purpose
// are still included.
func parseExcludeInternalParam(raw string) bool {
	if raw == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return false
	default:
		return true
	}
}

// GetTrafficEvent returns a single traffic event by ID. When the row's
// payload was spilled to a SpillStore backend (large body), the handler
// resolves the SpillRef in-line and folds the bytes back onto
// RequestBody / ResponseBody so UI consumers see a single response shape
// regardless of inline-vs-spill storage.
func (h *Handler) GetTrafficEvent(c echo.Context) error {
	id := c.Param("id")
	record, err := h.traffic.GetTrafficEvent(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if record == nil {
		return c.JSON(http.StatusNotFound, errJSON("Traffic event not found", "not_found", ""))
	}

	// Resolve spilled bodies if the SpillStore is wired and the row has a
	// non-NULL spill_ref. Failures fall back to leaving the inline body
	// as-is (which may already be NULL); the spillRef remains on the
	// payload so the UI can still surface "stored externally" information.
	if h.spillStore != nil {
		ctx := c.Request().Context()
		if record.RequestBody == nil && len(record.RequestSpillRef) > 0 {
			if body, err := h.resolveSpillBody(ctx, record.RequestSpillRef); err == nil {
				record.RequestBody = body
			} else {
				h.logSpillFetchFailure("view", "request", record.ID, err)
			}
		}
		if record.ResponseBody == nil && len(record.ResponseSpillRef) > 0 {
			if body, err := h.resolveSpillBody(ctx, record.ResponseSpillRef); err == nil {
				record.ResponseBody = body
			} else {
				h.logSpillFetchFailure("view", "response", record.ID, err)
			}
		}
	}

	// Render the stored inline bodies for the UI. The hub stores the captured
	// body as raw bytes in the inline BYTEA column, tagged by its
	// inline_*_encoding discriminator ("text", "base64", "zstd", ...);
	// renderBody decodes per the encoding and produces a value the UI can
	// render directly — valid JSON passes through, anything else is wrapped as
	// a JSON string. Spill-resolved bodies are already UI-ready and pass through.
	record.RequestBody = renderBody(record.RequestBody, record.RequestBodyEncoding)
	record.ResponseBody = renderBody(record.ResponseBody, record.ResponseBodyEncoding)

	return c.JSON(http.StatusOK, record)
}

// renderBody turns a stored inline body column plus its inline_*_encoding
// discriminator into a value the UI can render directly. The hub stores the
// captured body as raw bytes in the inline BYTEA column, tagged by the encoding
// discriminator; this decodes per the encoding, then a body that is valid JSON
// passes through as JSON and
// anything else (SSE text, decoded binary) is wrapped as a JSON string so the
// drawer always receives a printable value. Spill-resolved bodies arrive
// already UI-ready with an empty encoding and pass through unchanged.
func renderBody(col json.RawMessage, encoding string) json.RawMessage {
	if len(col) == 0 {
		return nil
	}
	raw := sharedaudit.DecodeBodyForColumn(col, encoding)
	if len(raw) == 0 {
		return nil
	}
	// stdlib json.Valid is zero-alloc (goccy's decodes into interface{}, ~4x).
	if stdjson.Valid(raw) {
		return json.RawMessage(raw)
	}
	out, _ := json.Marshal(string(raw))
	return json.RawMessage(out)
}

// resolveSpillBody decodes a JSONB spill_ref into an audit.SpillRef and
// fetches the bytes via the wired SpillStore. Returned bytes mirror the
// shape produced by renderBody's inline path: JSON-like content
// types whose bytes parse as JSON are returned as raw JSON; everything
// else (SSE, multipart, binary) is wrapped as a JSON string. This keeps
// the UI shape identical regardless of inline-vs-spill storage.
//
// The fetch, the sha256 integrity gate (which refuses a tampered at-rest blob
// so fabricated evidence can never be served as the genuine capture) and the
// failure diagnosis all live in fetchSpillBytes, shared with the normalize
// reader. This function owns only the UI shaping.
func (h *Handler) resolveSpillBody(ctx context.Context, refJSON []byte) (json.RawMessage, error) {
	body, ref, err := h.fetchSpillBytes(ctx, refJSON)
	if err != nil {
		return nil, err
	}
	if isJSONContentType(ref.ContentType) && stdjson.Valid(body) {
		return json.RawMessage(body), nil
	}
	// Non-JSON / unparseable payload — wrap as a JSON string so the UI's
	// body renderer (which types this field as json.RawMessage) receives
	// a parseable value.
	out, err := json.Marshal(string(body))
	if err != nil {
		return nil, fmt.Errorf("marshal spill body string: %w", err)
	}
	return out, nil
}

// isJSONContentType returns true when the supplied content-type header
// indicates a JSON body. Accepts both `application/json` and the
// `+json` family (e.g. `application/vnd.openai+json`). The parameter
// segment after `;` is ignored.
func isJSONContentType(ct string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	return base == "application/json" || strings.HasSuffix(base, "+json")
}
