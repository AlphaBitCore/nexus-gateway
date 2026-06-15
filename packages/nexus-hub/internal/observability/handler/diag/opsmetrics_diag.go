package diag

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	hubopsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// maxDiagDrainBatchSize caps the events the agent can drop in a single
// drain. The local SQLCipher buffer is unbounded by design (the process is
// dying when it writes there), so a long-disconnected agent could in
// principle accumulate hundreds of crash events before it reconnects.
// Mirror the agent_audit handler's defensive cap (500) — far above expected
// real-world counts but still shields the Hub from a malformed client.
const maxDiagDrainBatchSize = 500

// DiagDrainEvent is the wire-payload variant of opsmetrics.DiagEvent for the
// HTTP drain endpoint. It adds a top-level `id` field (the agent's local
// SQLCipher row id) so the response can ack which rows the agent should
// prune. The remaining fields embed DiagEvent unchanged.
type DiagDrainEvent struct {
	ID string `json:"id"`
	opsmetrics.DiagEvent
}

// DiagDrainRequest is the JSON request body.
type DiagDrainRequest struct {
	Events []DiagDrainEvent `json:"events"`
}

// DiagDrainResponse is the JSON success response.
type DiagDrainResponse struct {
	AcceptedIds []string `json:"acceptedIds"`
}

// DiagDrainAPI handles POST /api/internal/things/diag-events:batch — the
// crash-buffer drain that the agent calls on startup over mTLS. Unlike the
// WS path (which goes through opsmetrics.DiagWriterImpl's bounded channel),
// the drain uses synchronous per-event INSERT ... ON CONFLICT DO NOTHING so
// that (a) the agent gets a definitive acceptedIds list before pruning local
// rows, and (b) at-least-once delivery semantics from the spec hold —
// replaying a previously-acked event is a no-op rather than an error.
//
// The handler lives in package handler (not opsmetrics) so it can resolve
// the auth-attached *store.Thing via ThingFromContext without an import
// cycle, matching AgentAuditAPI.UploadAgentAudit's pattern.
//
// Pool is typed as the store.PgxPool interface so tests can inject pgxmock;
// *pgxpool.Pool satisfies it in production. Mirrors the seam used by Store
// + Manager + RuntimeBridgeAPI.
type DiagDrainAPI struct {
	Pool   store.PgxPool
	Logger *slog.Logger
}

// UploadDiagEvents handles the POST. Echo wires this onto the existing
// device-or-service auth middleware (DeviceOrServiceAuth) — same gating as
// the agent-audit handler.
func (h *DiagDrainAPI) UploadDiagEvents(c echo.Context) error {
	if h.Pool == nil {
		return serviceUnavailable(c, "diag store temporarily unavailable, retry later")
	}

	var req DiagDrainRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body: expected {\"events\": [...]}")
	}
	if len(req.Events) == 0 {
		return badRequest(c, "empty event batch")
	}
	if len(req.Events) > maxDiagDrainBatchSize {
		return c.JSON(http.StatusRequestEntityTooLarge, nexushttperr.ErrJSON("batch exceeds maximum size of 500 events", "validation_error", "PAYLOAD_TOO_LARGE"))
	}

	// Resolve thingID + thingType from the auth context. DeviceOrServiceAuth
	// stores the resolved Thing in the echo context under thingContextKey
	// for device-token callers. For service-token callers (e.g. CP loop-back
	// or Hub-internal jobs) the value is absent and we fall back to the
	// X-Thing-Id header — same pattern as AgentAuditAPI.UploadAgentAudit.
	var (
		thingID   string
		thingType string
	)
	if t := ThingFromContext(c); t != nil {
		thingID = t.ID
		thingType = t.Type
	}
	if thingID == "" {
		thingID = c.Request().Header.Get("X-Thing-Id")
	}
	if thingType == "" {
		// Drain is agent-only by design (services don't have a SQLCipher
		// crash buffer). Default to "agent" when the header didn't carry
		// the type — matches the only emitter today.
		thingType = "agent"
	}

	ctx := c.Request().Context()
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}

	accepted := make([]string, 0, len(req.Events))
	for _, evt := range req.Events {
		if evt.ID == "" {
			// Agent contract says id is mandatory. Skip without acking so
			// the agent surfaces the bug; partial-ack semantics treat
			// missing-id rows as "needs another attempt".
			logger.Warn("drop diag drain event with empty id",
				slog.String("thing_id", thingID),
				slog.String("source", evt.Source),
			)
			continue
		}
		if err := insertDiagDrainEvent(ctx, h.Pool, thingID, thingType, evt); err != nil {
			logger.Error("diag drain insert failed",
				slog.String("event_id", evt.ID),
				slog.String("thing_id", thingID),
				slog.String("error", err.Error()),
			)
			// Skip without acking; agent will retry next startup.
			continue
		}
		accepted = append(accepted, evt.ID)
	}

	return c.JSON(http.StatusOK, DiagDrainResponse{AcceptedIds: accepted})
}

// insertDiagDrainEvent writes a single drain event to thing_diag_event with
// ON CONFLICT DO NOTHING on the primary-key id. Returns nil for both
// "inserted" and "already-existed" — both states mean Hub has the event and
// the agent should prune it locally.
func insertDiagDrainEvent(ctx context.Context, pool store.PgxPool, thingID, thingType string, evt DiagDrainEvent) error {
	if evt.MessageHash == "" {
		evt.MessageHash = hubopsmetrics.ComputeMessageHash(evt.DiagEvent)
	}

	var attrsBytes []byte
	if evt.Attrs != nil {
		b, err := json.Marshal(evt.Attrs)
		if err != nil {
			return err
		}
		attrsBytes = b
	}
	var osInfoBytes []byte
	if evt.OSInfo != nil {
		b, err := json.Marshal(evt.OSInfo)
		if err != nil {
			return err
		}
		osInfoBytes = b
	}

	var stackPtr *string
	if evt.StackTrace != "" {
		s := evt.StackTrace
		stackPtr = &s
	}
	var agentVerPtr *string
	if evt.AgentVersion != "" {
		s := evt.AgentVersion
		agentVerPtr = &s
	}
	// trace_id mirror — same NULL-when-empty contract as the WS path's
	// insertBatch, so admin queries can filter `WHERE trace_id IS NULL`
	// regardless of which path (WS COPY vs HTTP drain INSERT) wrote the row.
	var tracePtr *string
	if evt.TraceID != "" {
		s := evt.TraceID
		tracePtr = &s
	}

	occurredAt := evt.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	const q = `
		INSERT INTO thing_diag_event
		    (id, thing_id, thing_type, occurred_at, received_at, level, event_type,
		     source, message, message_hash, trace_id, attrs, stack_trace, repeat_count,
		     agent_version, os_info)
		VALUES ($1, $2, $3, $4, NOW(), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (id) DO NOTHING
	`
	_, err := pool.Exec(ctx, q,
		evt.ID,
		thingID,
		thingType,
		occurredAt,
		evt.Level,
		evt.EventType,
		evt.Source,
		evt.Message,
		evt.MessageHash,
		tracePtr,
		attrsBytes,
		stackPtr,
		int32(evt.RepeatCount),
		agentVerPtr,
		osInfoBytes,
	)
	return err
}
