package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

const normalizeBackfillJobID = "normalize-backfill"

// normalizeBackfillBatchSize bounds one Run pass so a giant backlog can
// drain over multiple ticks without holding a long DB transaction or
// thrashing the normalize registry on a tight loop.
const normalizeBackfillBatchSize = 200

// spillReadCap bounds one spilled-body fetch so a corrupt or adversarial
// spill object cannot exhaust job memory; bodies above the cap skip with
// reason spill_fetch_failed (the raw object stays readable via the
// payload API, which streams instead of buffering).
const spillReadCap = 64 << 20

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
// The job reads inline bodies directly and fetches spilled bodies from
// the spill backend (bounded by spillReadCap); a row whose spill fetch
// fails is skip-marked `spill_fetch_failed` and retried at the next
// schema version. With no spill store wired, spill-only rows skip as
// `spill_ref_only`.
type NormalizeBackfill struct {
	pool     normalizeBackfillQueryer
	registry NormalizeRegistry
	spill    spillstore.SpillStore
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
	spill spillstore.SpillStore,
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
		spill:    spill,
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
	RequestSpillRef     []byte
	ResponseSpillRef    []byte
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
	// The LEFT JOIN on traffic_event_normalize_skip + `sk... IS NULL`
	// excludes rows this job has already attempted and could not fill
	// (spilled body / no payload / capture-time failure). Without it,
	// those rows matched the (normalized IS NULL) clause on every tick and,
	// under ORDER BY timestamp DESC LIMIT, a few hundred unfillable NEWER
	// rows permanently filled the batch and starved older fillable gaps.
	// Now every row leaves the scan set after exactly one attempt — filled
	// (normalized non-NULL) or skip-recorded — so the batch always advances.
	// Version clauses: any row stamped with an older normalize_version is
	// a re-normalization candidate — bumping core.SchemaVersion heals
	// every historical row through this one mechanism (no data
	// migration). The skip exclusion is version-aware for the same
	// reason: a row this job could not fill at version N gets exactly one
	// fresh attempt per subsequent version (whose decoders may fix
	// precisely what failed), instead of being excluded forever.
	const scanSQL = `
SELECT
    te.id,
    te.path,
    COALESCE(te.routed_provider_name, te.provider_name, '') AS adapter_type,
    COALESCE(te.routed_model_name, te.model_name, '')      AS model,
    tep.request_content_type,
    tep.response_content_type,
    tep.inline_request_body,
    tep.inline_response_body,
    tep.request_spill_ref,
    tep.response_spill_ref
FROM traffic_event te
JOIN traffic_event_payload tep ON tep.traffic_event_id = te.id
LEFT JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
LEFT JOIN traffic_event_normalize_skip sk ON sk.traffic_event_id = te.id
WHERE
    (sk.traffic_event_id IS NULL OR sk.normalize_version IS DISTINCT FROM $2)
    AND (
        ten.traffic_event_id IS NULL
        OR (ten.request_normalized IS NULL AND ten.response_normalized IS NULL)
        OR ten.normalize_version IS DISTINCT FROM $2
    )
    -- Governed rows are authoritative: a row carrying redaction spans was
    -- projected from storage-governed bytes, and its span offsets reference
    -- THAT projection. Re-normalizing (even at a version bump) would strand
    -- the offsets and erase a degradation diagnosis, so redacted rows are
    -- permanently excluded from the backfill rather than version-readmitted.
    AND (ten.traffic_event_id IS NULL
         OR (ten.request_redaction_spans IS NULL AND ten.response_redaction_spans IS NULL))
ORDER BY te.timestamp DESC
LIMIT $1
`
	rows, err := j.pool.Query(ctx, scanSQL, normalizeBackfillBatchSize, normcore.SchemaVersion)
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
			&c.RequestSpillRef, &c.ResponseSpillRef,
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
