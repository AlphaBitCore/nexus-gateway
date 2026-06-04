package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

const normalizeBackfillJobID = "normalize-backfill"

// normalizeBackfillBatchSize bounds one Run pass so a giant backlog can
// drain over multiple ticks without holding a long DB transaction or
// thrashing the normalize registry on a tight loop.
const normalizeBackfillBatchSize = 200

// normalizeBackfillQueryer is the minimum pgx surface the job needs.
// Declared narrowly so tests can inject pgxmock.PgxPoolIface without
// sharing the real traffic_event tables. Both *pgxpool.Pool and
// pgxmock.PgxPoolIface satisfy it via structural typing.
type normalizeBackfillQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NormalizeRegistry is the normalize seam the backfill job calls into.
// *normcore.Registry satisfies it via structural typing; tests inject a stub.
type NormalizeRegistry interface {
	Normalize(ctx context.Context, raw []byte, meta normcore.Meta) (normcore.NormalizedPayload, error)
}

// NormalizeBackfill periodically rescans traffic_event_normalized for rows
// whose sidecar is missing (the parent traffic_event row committed but
// insertNormalizedPayloads partially failed in the consumer, leaving raw
// bytes in traffic_event_payload but no normalized snapshot). For each
// gap it re-runs the normalize Registry against the inline raw bytes
// and upserts the sidecar.
//
// Why this job exists: traffic.go's flush wraps insertNormalizedPayloads
// in a "log warn but don't return err" guard so a single bad row cannot
// hold the whole batch hostage. Without this backfill, every row whose
// sidecar insert failed would stay NULL forever — admin Traffic-drawer
// "Normalized" panel would say "no normalized payload" indefinitely
// even though the raw bytes are sitting one table over.
//
// The job is best-effort: it only handles inline bodies (when the body
// was below the spill threshold). Spill-ref bodies require a fetch
// against the spill backend; this job logs them as `skipped_spill`
// without failing the batch so future iterations can add backend access.
type NormalizeBackfill struct {
	pool     normalizeBackfillQueryer
	registry NormalizeRegistry
	interval time.Duration
	logger   *slog.Logger
	scanned  *opsmetrics.Counter
	filled   *opsmetrics.Counter
	skipped  *opsmetrics.Counter
	errors   *opsmetrics.Counter
}

// NewNormalizeBackfill wires the job. interval defaults to 5 minutes when
// non-positive — long enough that the backlog of newly-failed rows has
// time to accumulate into a useful batch, short enough that a one-time
// MQ-consumer hiccup recovers within a single dashboard refresh.
// opsReg may be nil in tests.
func NewNormalizeBackfill(
	pool *pgxpool.Pool,
	registry NormalizeRegistry,
	interval time.Duration,
	opsReg *opsmetrics.Registry,
	logger *slog.Logger,
) *NormalizeBackfill {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	var scanned, filled, skipped, errors *opsmetrics.Counter
	if opsReg != nil {
		scanned = opsReg.NewCounter("normalize_backfill.scanned_total", []string{})
		filled = opsReg.NewCounter("normalize_backfill.filled_total", []string{})
		skipped = opsReg.NewCounter("normalize_backfill.skipped_total", []string{"reason"})
		errors = opsReg.NewCounter("normalize_backfill.errors_total", []string{"phase"})
	}
	return &NormalizeBackfill{
		pool:     pool,
		registry: registry,
		interval: interval,
		logger:   logger.With("job", normalizeBackfillJobID),
		scanned:  scanned,
		filled:   filled,
		skipped:  skipped,
		errors:   errors,
	}
}

func (j *NormalizeBackfill) ID() string   { return normalizeBackfillJobID }
func (j *NormalizeBackfill) Name() string { return "Normalize Backfill" }
func (j *NormalizeBackfill) Description() string {
	return "Re-runs normalize against raw bytes for traffic_event rows whose normalized sidecar is missing (consumer partial-failure recovery)."
}
func (j *NormalizeBackfill) Interval() time.Duration { return j.interval }

// RunOnStart=false: a fresh service may not yet have an MQ consumer running
// to produce traffic_event rows. The first periodic tick grants startup grace.
func (j *NormalizeBackfill) RunOnStart() bool { return false }

// backfillCandidate is the row shape scanned out of the LEFT-JOIN query.
type backfillCandidate struct {
	EventID             string
	Path                string
	AdapterType         string
	Model               string
	RequestContentType  *string
	ResponseContentType *string
	InlineRequestBody   []byte
	InlineResponseBody  []byte
}

// Run scans up to normalizeBackfillBatchSize traffic_event rows that lack a
// non-empty traffic_event_normalized sidecar and attempts to backfill each.
//
// SQL strategy:
//   - LEFT JOIN traffic_event_normalized ten ON id; pick rows where ten is
//     missing OR both normalized JSONB columns are NULL.
//   - INNER JOIN traffic_event_payload to filter out events captured without
//     bodies (no raw bytes available to normalize against).
//   - ORDER BY timestamp DESC LIMIT batchSize so the freshest gaps fill first
//     (the admin Traffic drawer shows newest events on top).
//
// Per-row work happens outside the cursor so a slow Normalize call does not
// hold the read lock.
func (j *NormalizeBackfill) Run(ctx context.Context) error {
	const scanSQL = `
SELECT
    te.id,
    te.path,
    COALESCE(te.routed_provider_name, te.provider_name, '') AS adapter_type,
    COALESCE(te.routed_model_name, te.model_name, '')      AS model,
    tep.request_content_type,
    tep.response_content_type,
    tep.inline_request_body,
    tep.inline_response_body
FROM traffic_event te
JOIN traffic_event_payload tep ON tep.traffic_event_id = te.id
LEFT JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
WHERE
    ten.traffic_event_id IS NULL
    OR (ten.request_normalized IS NULL AND ten.response_normalized IS NULL)
ORDER BY te.timestamp DESC
LIMIT $1
`
	rows, err := j.pool.Query(ctx, scanSQL, normalizeBackfillBatchSize)
	if err != nil {
		j.bumpErr("scan")
		return fmt.Errorf("normalize_backfill: scan: %w", err)
	}

	var candidates []backfillCandidate
	for rows.Next() {
		var c backfillCandidate
		var reqEnvelope, respEnvelope []byte
		if err := rows.Scan(
			&c.EventID, &c.Path, &c.AdapterType, &c.Model,
			&c.RequestContentType, &c.ResponseContentType,
			&reqEnvelope, &respEnvelope,
		); err != nil {
			j.bumpErr("scan_row")
			rows.Close()
			return fmt.Errorf("normalize_backfill: scan row: %w", err)
		}
		c.InlineRequestBody = extractInlineBytes(reqEnvelope)
		c.InlineResponseBody = extractInlineBytes(respEnvelope)
		candidates = append(candidates, c)
	}
	rows.Close()

	if j.scanned != nil {
		j.scanned.With().Add(float64(len(candidates)))
	}
	if len(candidates) == 0 {
		return nil
	}

	for _, c := range candidates {
		j.backfillOne(ctx, c)
	}
	return nil
}

// backfillOne re-runs normalize for one candidate and upserts the sidecar.
// Each direction is independent: a request that normalizes cleanly while
// the response fails still updates the row with what we have.
func (j *NormalizeBackfill) backfillOne(ctx context.Context, c backfillCandidate) {
	// Skip entirely when there are no inline bytes for either direction
	// (spill-ref-only payloads — handled by a future spill-aware pass).
	if len(c.InlineRequestBody) == 0 && len(c.InlineResponseBody) == 0 {
		j.bumpSkipped("spill_ref_only")
		return
	}

	requestNormalized, requestStatus, requestErr := j.normalizeDirection(
		ctx, c.InlineRequestBody, c, normcore.DirectionRequest,
	)
	responseNormalized, responseStatus, responseErr := j.normalizeDirection(
		ctx, c.InlineResponseBody, c, normcore.DirectionResponse,
	)

	if len(requestNormalized) == 0 && len(responseNormalized) == 0 {
		// Both directions failed or were absent — nothing useful to write.
		j.bumpSkipped("no_payload_produced")
		return
	}

	const upsertSQL = `
INSERT INTO traffic_event_normalized (
    traffic_event_id,
    request_normalized, response_normalized,
    request_status, response_status,
    request_error_reason, response_error_reason,
    normalize_version
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (traffic_event_id) DO UPDATE SET
    request_normalized    = COALESCE(EXCLUDED.request_normalized,    traffic_event_normalized.request_normalized),
    response_normalized   = COALESCE(EXCLUDED.response_normalized,   traffic_event_normalized.response_normalized),
    request_status        = COALESCE(EXCLUDED.request_status,        traffic_event_normalized.request_status),
    response_status       = COALESCE(EXCLUDED.response_status,       traffic_event_normalized.response_status),
    request_error_reason  = COALESCE(EXCLUDED.request_error_reason,  traffic_event_normalized.request_error_reason),
    response_error_reason = COALESCE(EXCLUDED.response_error_reason, traffic_event_normalized.response_error_reason),
    normalize_version     = EXCLUDED.normalize_version
`
	_, err := j.pool.Exec(ctx, upsertSQL,
		c.EventID,
		nilJSONIfEmpty(requestNormalized),
		nilJSONIfEmpty(responseNormalized),
		nilIfEmpty(requestStatus),
		nilIfEmpty(responseStatus),
		nilIfEmpty(requestErr),
		nilIfEmpty(responseErr),
		normcore.SchemaVersion,
	)
	if err != nil {
		j.bumpErr("upsert")
		j.logger.Warn("normalize_backfill upsert failed",
			"eventId", c.EventID,
			"error", err,
		)
		return
	}
	if j.filled != nil {
		j.filled.With().Inc()
	}
}

// normalizeDirection runs the registry against the given raw bytes for one
// direction. Returns marshalled JSON of the NormalizedPayload, the status
// string ("ok" / "partial" / "failed"), and an error-reason string for the
// non-ok statuses. Empty raw input returns all-empty (caller skips writing).
func (j *NormalizeBackfill) normalizeDirection(
	ctx context.Context,
	raw []byte,
	c backfillCandidate,
	direction normcore.Direction,
) (marshalled []byte, status, errReason string) {
	if len(raw) == 0 {
		return nil, "", ""
	}
	contentType := ""
	if direction == normcore.DirectionRequest && c.RequestContentType != nil {
		contentType = *c.RequestContentType
	}
	if direction == normcore.DirectionResponse && c.ResponseContentType != nil {
		contentType = *c.ResponseContentType
	}
	meta := normcore.Meta{
		AdapterType:  c.AdapterType,
		Model:        c.Model,
		ContentType:  contentType,
		Direction:    direction,
		EndpointPath: c.Path,
	}
	payload, err := j.registry.Normalize(ctx, raw, meta)
	if err != nil {
		// "failed" — no usable payload.
		return nil, "failed", err.Error()
	}
	out, mErr := json.Marshal(payload)
	if mErr != nil {
		j.bumpErr("marshal")
		return nil, "failed", mErr.Error()
	}
	return out, "ok", ""
}

func (j *NormalizeBackfill) bumpErr(phase string) {
	if j.errors != nil {
		j.errors.With(phase).Inc()
	}
}
func (j *NormalizeBackfill) bumpSkipped(reason string) {
	if j.skipped != nil {
		j.skipped.With(reason).Inc()
	}
}

// extractInlineBytes pulls the raw byte slice from a body envelope JSON
// (the wire form produced by spillstore.EmitBody → audit.Body) when the
// body kind is "inline". Returns nil for absent / spill-ref bodies — the
// caller treats a nil slice as "no inline bytes available".
func extractInlineBytes(envelope []byte) []byte {
	if len(envelope) == 0 {
		return nil
	}
	var body sharedaudit.Body
	if err := json.Unmarshal(envelope, &body); err != nil {
		return nil
	}
	if body.Kind != sharedaudit.BodyInline {
		return nil
	}
	return body.InlineBytes
}

// nilJSONIfEmpty returns nil for an empty byte slice; otherwise wraps the
// bytes as json.RawMessage. Mirrors traffic.go's nullableJSON.
func nilJSONIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// nilIfEmpty is the same helper traffic.go uses — empty string maps to SQL
// NULL, non-empty stays as the value.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
