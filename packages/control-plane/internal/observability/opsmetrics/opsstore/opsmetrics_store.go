// Package store — opsmetrics_store.go: queries against the ops-metrics
// (metric_ops_*) and diagnostic-event (thing_diag_*) tables that back the
// /api/admin/ops-metrics, /api/admin/diag-events, /api/admin/agents/diagnostic-mode,
// and /api/admin/observability/retention endpoints.
//
// Per architecture spec §10, Control Plane reads these tables directly via
// pgx — no Hub HTTP roundtrip — mirroring the existing traffic_event analytics
// path. The Postgres pool is the same as the Hub Postgres (T20 wired CP to it).
package opsstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// metric_ops_* — ops metrics queries

// OpsMetricSample is one row from metric_ops_raw or one synthesized "current"
// row from the latest-per-key projection. The handler uses the same struct for
// both so the response shape is uniform.
type OpsMetricSample struct {
	SampledAt    time.Time      `json:"sampledAt"`
	ThingID      string         `json:"nodeId"`
	ThingType    string         `json:"nodeType"`
	MetricName   string         `json:"metricName"`
	MetricKind   string         `json:"metricKind"`
	DimensionKey string         `json:"dimensionKey"`
	Value        *float64       `json:"value,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// OpsMetricBucket is one rollup row (or one raw row presented as a single-sample
// bucket). For raw rows BucketStart is sampledAt and SampleCount is 1 with
// ValueAvg/Sum/Min/Max all set to the raw value.
type OpsMetricBucket struct {
	BucketStart  time.Time      `json:"bucketStart"`
	ThingID      *string        `json:"nodeId,omitempty"`
	ThingType    string         `json:"nodeType"`
	MetricName   string         `json:"metricName"`
	MetricKind   string         `json:"metricKind"`
	DimensionKey string         `json:"dimensionKey"`
	ValueAvg     *float64       `json:"valueAvg,omitempty"`
	ValueSum     *float64       `json:"valueSum,omitempty"`
	ValueMin     *float64       `json:"valueMin,omitempty"`
	ValueMax     *float64       `json:"valueMax,omitempty"`
	SampleCount  int            `json:"sampleCount"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// OpsCurrentParams filters /api/admin/ops-metrics/current.
type OpsCurrentParams struct {
	ThingType string // "" = no filter
	ThingID   string // "" = no filter
}

// GetOpsMetricsCurrent returns the latest sample per (thing_id, metric_name,
// dimension_key) within the last 90 seconds, optionally filtered by thingType
// and/or thingId. Implemented with a window function over metric_ops_raw.
func (store *Store) GetOpsMetricsCurrent(ctx context.Context, p OpsCurrentParams) ([]OpsMetricSample, error) {
	var thingType, thingID *string
	if p.ThingType != "" {
		thingType = &p.ThingType
	}
	if p.ThingID != "" {
		thingID = &p.ThingID
	}

	const q = `
		WITH ranked AS (
			SELECT sampled_at, thing_id, thing_type, metric_name, metric_kind,
			       dimension_key, value, metadata,
			       ROW_NUMBER() OVER (
			         PARTITION BY thing_id, metric_name, dimension_key
			         ORDER BY sampled_at DESC
			       ) AS rn
			  FROM metric_ops_raw
			 WHERE sampled_at > NOW() - interval '90 seconds'
			   AND ($1::text IS NULL OR thing_type = $1)
			   AND ($2::text IS NULL OR thing_id = $2)
		)
		SELECT sampled_at, thing_id, thing_type, metric_name, metric_kind,
		       dimension_key, value, metadata
		  FROM ranked
		 WHERE rn = 1
		 ORDER BY sampled_at DESC
		 LIMIT 5000
	`
	rows, err := store.pool.Query(ctx, q, thingType, thingID)
	if err != nil {
		return nil, fmt.Errorf("ops_metrics_current query: %w", err)
	}
	defer rows.Close()

	out := make([]OpsMetricSample, 0, 64)
	for rows.Next() {
		var s OpsMetricSample
		var meta []byte
		if err := rows.Scan(&s.SampledAt, &s.ThingID, &s.ThingType, &s.MetricName,
			&s.MetricKind, &s.DimensionKey, &s.Value, &meta); err != nil {
			return nil, fmt.Errorf("ops_metrics_current scan: %w", err)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &s.Metadata)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ops_metrics_current iterate: %w", err)
	}
	return out, nil
}

// OpsTimeseriesParams filters /api/admin/ops-metrics/timeseries.
type OpsTimeseriesParams struct {
	ThingID      string
	MetricName   string
	DimensionKey *string // nil = no filter; pointer to allow ""-value match
	From         time.Time
	To           time.Time
	Granularity  string // "raw" | "5m" | "1h" | "1d" | "1mo"
}

// SelectGranularity picks the rollup tier for the given span when the caller
// asks for granularity=auto. The 5-minute tier keeps short dashboard windows
// off the partitioned raw table: ≤1h → raw, 1h-6h → 5m, 6h-7d → 1h,
// 7d-90d → 1d, >90d → 1mo.
func SelectGranularity(from, to time.Time) string {
	span := to.Sub(from)
	switch {
	case span <= time.Hour:
		return "raw"
	case span <= 6*time.Hour:
		return "5m"
	case span <= 7*24*time.Hour:
		return "1h"
	case span <= 90*24*time.Hour:
		return "1d"
	default:
		return "1mo"
	}
}

// GetOpsMetricsTimeseries returns time-bucketed rows for one (thingId, metric,
// dimension?) tuple over [from, to). Granularity selects the underlying table:
// raw → metric_ops_raw, 1h/1d/1mo → corresponding rollup. For raw, each row
// becomes a single-sample bucket (count=1, avg=sum=min=max=value).
func (store *Store) GetOpsMetricsTimeseries(ctx context.Context, p OpsTimeseriesParams) ([]OpsMetricBucket, error) {
	if p.ThingID == "" {
		return nil, errors.New("ops_metrics_timeseries: thingId is required")
	}
	if p.MetricName == "" {
		return nil, errors.New("ops_metrics_timeseries: metric is required")
	}
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) {
		return nil, errors.New("ops_metrics_timeseries: from < to is required")
	}

	switch p.Granularity {
	case "raw":
		return store.queryOpsRaw(ctx, p)
	case "5m", "1h", "1d", "1mo":
		return store.queryOpsRollup(ctx, "metric_ops_rollup_"+p.Granularity, p)
	default:
		return nil, fmt.Errorf("ops_metrics_timeseries: invalid granularity %q", p.Granularity)
	}
}

func (store *Store) queryOpsRaw(ctx context.Context, p OpsTimeseriesParams) ([]OpsMetricBucket, error) {
	var dim *string
	if p.DimensionKey != nil {
		v := *p.DimensionKey
		dim = &v
	}
	const q = `
		SELECT sampled_at, thing_id, thing_type, metric_name, metric_kind,
		       dimension_key, value, metadata
		  FROM metric_ops_raw
		 WHERE thing_id = $1
		   AND metric_name = $2
		   AND ($3::text IS NULL OR dimension_key = $3)
		   AND sampled_at >= $4 AND sampled_at < $5
		 ORDER BY sampled_at ASC
		 LIMIT 50000
	`
	rows, err := store.pool.Query(ctx, q, p.ThingID, p.MetricName, dim, p.From, p.To)
	if err != nil {
		return nil, fmt.Errorf("ops_metrics_raw query: %w", err)
	}
	defer rows.Close()

	out := make([]OpsMetricBucket, 0, 64)
	for rows.Next() {
		var (
			sampledAt time.Time
			thingID   string
			thingType string
			metric    string
			kind      string
			dimKey    string
			value     *float64
			meta      []byte
		)
		if err := rows.Scan(&sampledAt, &thingID, &thingType, &metric, &kind, &dimKey, &value, &meta); err != nil {
			return nil, fmt.Errorf("ops_metrics_raw scan: %w", err)
		}
		tid := thingID
		b := OpsMetricBucket{
			BucketStart:  sampledAt,
			ThingID:      &tid,
			ThingType:    thingType,
			MetricName:   metric,
			MetricKind:   kind,
			DimensionKey: dimKey,
			ValueAvg:     value,
			ValueSum:     value,
			ValueMin:     value,
			ValueMax:     value,
			SampleCount:  1,
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &b.Metadata)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ops_metrics_raw iterate: %w", err)
	}
	return out, nil
}

func (store *Store) queryOpsRollup(ctx context.Context, table string, p OpsTimeseriesParams) ([]OpsMetricBucket, error) {
	var dim *string
	if p.DimensionKey != nil {
		v := *p.DimensionKey
		dim = &v
	}
	// Per-thing rollups have non-NULL thing_id; fleet rollups (thing_id IS NULL)
	// are queried by the fleet endpoint, not here.
	q := fmt.Sprintf(`
		SELECT bucket_start, thing_id, thing_type, metric_name, metric_kind,
		       dimension_key, value_avg, value_sum, value_min, value_max,
		       sample_count, metadata
		  FROM %s
		 WHERE thing_id = $1
		   AND metric_name = $2
		   AND ($3::text IS NULL OR dimension_key = $3)
		   AND bucket_start >= $4 AND bucket_start < $5
		 ORDER BY bucket_start ASC
		 LIMIT 50000
	`, table)
	rows, err := store.pool.Query(ctx, q, p.ThingID, p.MetricName, dim, p.From, p.To)
	if err != nil {
		return nil, fmt.Errorf("%s query: %w", table, err)
	}
	defer rows.Close()
	return scanOpsBuckets(rows, table)
}

// OpsFleetParams filters /api/admin/ops-metrics/fleet.
type OpsFleetParams struct {
	ThingType    string // required
	MetricName   string // required
	DimensionKey *string
	From         time.Time
	To           time.Time
	Granularity  string // 5m | 1h | 1d | 1mo (raw has no fleet aggregate)
}

// GetOpsMetricsFleet returns the fleet aggregate (thing_id IS NULL) for the
// given metric over [from, to). Defaults to 1h granularity when unset.
func (store *Store) GetOpsMetricsFleet(ctx context.Context, p OpsFleetParams) ([]OpsMetricBucket, error) {
	if p.ThingType == "" {
		return nil, errors.New("ops_metrics_fleet: thingType is required")
	}
	if p.MetricName == "" {
		return nil, errors.New("ops_metrics_fleet: metric is required")
	}
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) {
		return nil, errors.New("ops_metrics_fleet: from < to is required")
	}
	gran := p.Granularity
	if gran == "" {
		gran = SelectGranularity(p.From, p.To)
		if gran == "raw" {
			// Raw has no fleet slice. Bump to the smallest rollup tier (5m).
			gran = "5m"
		}
	}
	if gran != "5m" && gran != "1h" && gran != "1d" && gran != "1mo" {
		return nil, fmt.Errorf("ops_metrics_fleet: invalid granularity %q", gran)
	}
	table := "metric_ops_rollup_" + gran

	var dim *string
	if p.DimensionKey != nil {
		v := *p.DimensionKey
		dim = &v
	}
	q := fmt.Sprintf(`
		SELECT bucket_start, thing_id, thing_type, metric_name, metric_kind,
		       dimension_key, value_avg, value_sum, value_min, value_max,
		       sample_count, metadata
		  FROM %s
		 WHERE thing_id IS NULL
		   AND thing_type = $1
		   AND metric_name = $2
		   AND ($3::text IS NULL OR dimension_key = $3)
		   AND bucket_start >= $4 AND bucket_start < $5
		 ORDER BY bucket_start ASC
		 LIMIT 50000
	`, table)
	rows, err := store.pool.Query(ctx, q, p.ThingType, p.MetricName, dim, p.From, p.To)
	if err != nil {
		return nil, fmt.Errorf("%s fleet query: %w", table, err)
	}
	defer rows.Close()
	return scanOpsBuckets(rows, table)
}

func scanOpsBuckets(rows pgx.Rows, table string) ([]OpsMetricBucket, error) {
	out := make([]OpsMetricBucket, 0, 64)
	for rows.Next() {
		var (
			b     OpsMetricBucket
			tid   *string
			meta  []byte
			count int32
		)
		if err := rows.Scan(&b.BucketStart, &tid, &b.ThingType, &b.MetricName, &b.MetricKind,
			&b.DimensionKey, &b.ValueAvg, &b.ValueSum, &b.ValueMin, &b.ValueMax,
			&count, &meta); err != nil {
			return nil, fmt.Errorf("%s scan: %w", table, err)
		}
		b.ThingID = tid
		b.SampleCount = int(count)
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &b.Metadata)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s iterate: %w", table, err)
	}
	return out, nil
}

// thing_diag_event — diagnostic event queries

// DiagEvent is one row from thing_diag_event.
type DiagEvent struct {
	ID           string         `json:"id"`
	ThingID      string         `json:"nodeId"`
	ThingType    string         `json:"nodeType"`
	OccurredAt   time.Time      `json:"occurredAt"`
	ReceivedAt   time.Time      `json:"receivedAt"`
	Level        string         `json:"level"`
	EventType    string         `json:"eventType"`
	Source       string         `json:"source"`
	Message      string         `json:"message"`
	MessageHash  string         `json:"messageHash"`
	Attrs        map[string]any `json:"attrs,omitempty"`
	StackTrace   string         `json:"stackTrace,omitempty"`
	RepeatCount  int            `json:"repeatCount"`
	AgentVersion string         `json:"agentVersion,omitempty"`
	OSInfo       map[string]any `json:"osInfo,omitempty"`
}

// DiagEventListParams filters /api/admin/diag-events. Cursor encodes
// (occurred_at, id) of the last item from the previous page so newest-first
// pagination is stable across concurrent inserts.
type DiagEventListParams struct {
	ThingID string
	Level   string
	Source  string
	// EventType filters to a single event_type (error / crash /
	// lifecycle / watchdog). Empty = no filter. See DiagGroupsParams
	// for the rationale — lifecycle events from agent.startup /
	// shutdown / pause / resume need to be isolatable from the
	// crash/error noise on the Recent Errors page.
	EventType string
	From      *time.Time
	To        *time.Time
	Search    string
	Limit     int
	Cursor    string
}

// DiagEventListResult bundles the page rows with the next cursor (empty when
// there is no next page).
type DiagEventListResult struct {
	Items      []DiagEvent
	NextCursor string
}

// EncodeDiagCursor builds the wire cursor for (occurred_at, id).
func EncodeDiagCursor(occurredAt time.Time, id string) string {
	raw := occurredAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeDiagCursor parses a cursor back into (occurred_at, id). Returns
// non-nil err on malformed input so the handler can 400 instead of silently
// reading from the start.
func DecodeDiagCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decode diag cursor: %w", err)
	}
	idx := strings.IndexByte(string(raw), '|')
	if idx <= 0 {
		return time.Time{}, "", errors.New("decode diag cursor: missing separator")
	}
	t, err := time.Parse(time.RFC3339Nano, string(raw[:idx]))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decode diag cursor: parse time: %w", err)
	}
	return t, string(raw[idx+1:]), nil
}

// ListDiagEvents returns a newest-first page of diagnostic events. The query
// fetches limit+1 rows so the handler can detect whether a next page exists.
func (store *Store) ListDiagEvents(ctx context.Context, p DiagEventListParams) (*DiagEventListResult, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	args := make([]any, 0, 8)
	conds := []string{"1=1"}
	pos := 1
	add := func(cond string, v any) {
		conds = append(conds, fmt.Sprintf(cond, pos))
		args = append(args, v)
		pos++
	}
	if p.ThingID != "" {
		add("thing_id = $%d", p.ThingID)
	}
	if p.Level != "" {
		add("level = $%d", p.Level)
	}
	if p.EventType != "" {
		add("event_type = $%d", p.EventType)
	}
	if p.Source != "" {
		add("source = $%d", p.Source)
	}
	if p.From != nil {
		add("occurred_at >= $%d", *p.From)
	}
	if p.To != nil {
		add("occurred_at < $%d", *p.To)
	}
	if p.Search != "" {
		add("message ILIKE '%%' || $%d || '%%'", p.Search)
	}
	if p.Cursor != "" {
		t, id, err := DecodeDiagCursor(p.Cursor)
		if err != nil {
			return nil, err
		}
		// (occurred_at, id) < (cursor_t, cursor_id) — newest-first order.
		conds = append(conds, fmt.Sprintf("(occurred_at, id) < ($%d, $%d)", pos, pos+1))
		args = append(args, t, id)
		pos += 2
	}

	q := fmt.Sprintf(`
		SELECT id, thing_id, thing_type, occurred_at, received_at, level, event_type,
		       source, message, message_hash, attrs, stack_trace, repeat_count,
		       agent_version, os_info
		  FROM thing_diag_event
		 WHERE %s
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT $%d
	`, strings.Join(conds, " AND "), pos)
	args = append(args, limit+1)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list_diag_events: %w", err)
	}
	defer rows.Close()

	items := make([]DiagEvent, 0, limit)
	for rows.Next() {
		var e DiagEvent
		var attrs, osInfo []byte
		var stack, agentVer *string
		if err := rows.Scan(&e.ID, &e.ThingID, &e.ThingType, &e.OccurredAt, &e.ReceivedAt,
			&e.Level, &e.EventType, &e.Source, &e.Message, &e.MessageHash, &attrs,
			&stack, &e.RepeatCount, &agentVer, &osInfo); err != nil {
			return nil, fmt.Errorf("list_diag_events scan: %w", err)
		}
		if len(attrs) > 0 {
			_ = json.Unmarshal(attrs, &e.Attrs)
		}
		if len(osInfo) > 0 {
			_ = json.Unmarshal(osInfo, &e.OSInfo)
		}
		if stack != nil {
			e.StackTrace = *stack
		}
		if agentVer != nil {
			e.AgentVersion = *agentVer
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_diag_events iterate: %w", err)
	}

	res := &DiagEventListResult{}
	if len(items) > limit {
		next := items[limit-1]
		res.NextCursor = EncodeDiagCursor(next.OccurredAt, next.ID)
		items = items[:limit]
	}
	res.Items = items
	return res, nil
}

// DiagGroup is one row from /api/admin/diag-events/groups: a message_hash
// bucket with affected-things and total occurrence counts, a per-5min
// sparkline series, and a silenced flag derived from diag_silence.
type DiagGroup struct {
	MessageHash      string            `json:"messageHash"`
	SampleMessage    string            `json:"sampleMessage"`
	Source           string            `json:"source"`
	AffectedThings   int               `json:"affectedNodes"`
	TotalOccurrences int               `json:"totalOccurrences"`
	FirstSeen        time.Time         `json:"firstSeen"`
	LastSeen         time.Time         `json:"lastSeen"`
	MaxLevel         string            `json:"maxLevel"`
	Buckets          []DiagGroupBucket `json:"buckets"`
	Silenced         bool              `json:"silenced"`
}

// DiagGroupBucket is one 5-minute bucket of a DiagGroup's sparkline.
type DiagGroupBucket struct {
	Ts    time.Time `json:"ts"`
	Count int       `json:"count"`
}

// DiagGroupsParams filters /api/admin/diag-events/groups.
type DiagGroupsParams struct {
	From      time.Time
	To        time.Time
	ThingType string
	// EventType filters to a single event_type (error / crash /
	// lifecycle / watchdog). Empty = no filter. Lifecycle events
	// from agent.startup / shutdown / pause / resume / sso_login
	// flow through the same thing_diag_event table — without this
	// admin sees crash/error and lifecycle interleaved in the
	// Recent Errors page and can't focus on either class.
	EventType string
}

// ListDiagGroups returns the top-100 message_hash buckets in [from, to)
// ordered by total occurrences descending, then last_seen descending.
func (store *Store) ListDiagGroups(ctx context.Context, p DiagGroupsParams) ([]DiagGroup, error) {
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) {
		return nil, errors.New("list_diag_groups: from < to is required")
	}
	var thingType *string
	if p.ThingType != "" {
		thingType = &p.ThingType
	}
	var eventType *string
	if p.EventType != "" {
		eventType = &p.EventType
	}
	// Single query: aggregate the group facts AND mark silenced via EXISTS
	// against diag_silence. Buckets fetched in a second query against the
	// top-N hashes so we don't blow up the inner GROUP BY cardinality.
	const q = `
		SELECT e.message_hash,
		       MIN(e.message)            AS sample_message,
		       MIN(e.source)             AS source,
		       COUNT(DISTINCT e.thing_id) AS affected_things,
		       COUNT(*)::INT             AS total_occurrences,
		       MIN(e.occurred_at)        AS first_seen,
		       MAX(e.occurred_at)        AS last_seen,
		       MAX(e.level)              AS max_level,
		       EXISTS (
		         SELECT 1 FROM diag_silence ds
		          WHERE ds.message_hash = e.message_hash
		            AND ds.level        = MAX(e.level)
		            AND (ds.expires_at IS NULL OR ds.expires_at > NOW())
		       )                         AS silenced
		  FROM thing_diag_event e
		 WHERE e.occurred_at >= $1 AND e.occurred_at < $2
		   AND ($3::text IS NULL OR e.thing_type = $3)
		   AND ($4::text IS NULL OR e.event_type = $4)
		 GROUP BY e.message_hash
		 ORDER BY total_occurrences DESC, last_seen DESC
		 LIMIT 100
	`
	rows, err := store.pool.Query(ctx, q, p.From, p.To, thingType, eventType)
	if err != nil {
		return nil, fmt.Errorf("list_diag_groups: %w", err)
	}

	out := make([]DiagGroup, 0, 64)
	hashes := make([]string, 0, 64)
	for rows.Next() {
		var g DiagGroup
		if err := rows.Scan(&g.MessageHash, &g.SampleMessage, &g.Source,
			&g.AffectedThings, &g.TotalOccurrences, &g.FirstSeen, &g.LastSeen, &g.MaxLevel, &g.Silenced); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list_diag_groups scan: %w", err)
		}
		g.Buckets = []DiagGroupBucket{}
		out = append(out, g)
		hashes = append(hashes, g.MessageHash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_diag_groups iterate: %w", err)
	}

	if len(hashes) == 0 {
		return out, nil
	}

	// Per-5min bucket counts for every group hash in one pass. Index lives
	// on (message_hash, occurred_at) so the predicate is selective.
	const bq = `
		SELECT message_hash,
		       date_bin(INTERVAL '5 minutes', occurred_at, TIMESTAMPTZ '2000-01-01') AS bucket_ts,
		       COUNT(*)::INT AS cnt
		  FROM thing_diag_event
		 WHERE occurred_at >= $1 AND occurred_at < $2
		   AND ($3::text IS NULL OR thing_type = $3)
		   AND message_hash = ANY($4)
		 GROUP BY message_hash, bucket_ts
		 ORDER BY message_hash, bucket_ts
	`
	brows, err := store.pool.Query(ctx, bq, p.From, p.To, thingType, hashes)
	if err != nil {
		return nil, fmt.Errorf("list_diag_groups buckets: %w", err)
	}
	defer brows.Close()

	byHash := make(map[string][]DiagGroupBucket, len(hashes))
	for brows.Next() {
		var hash string
		var b DiagGroupBucket
		if err := brows.Scan(&hash, &b.Ts, &b.Count); err != nil {
			return nil, fmt.Errorf("list_diag_groups buckets scan: %w", err)
		}
		byHash[hash] = append(byHash[hash], b)
	}
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("list_diag_groups buckets iterate: %w", err)
	}
	for i := range out {
		if bs := byHash[out[i].MessageHash]; bs != nil {
			out[i].Buckets = bs
		}
	}
	return out, nil
}

// CrashCohort is one row from /api/admin/diag-events/crash-cohorts.
type CrashCohort struct {
	AgentVersion   string    `json:"agentVersion"`
	OS             string    `json:"os"`
	OSVersion      string    `json:"osVersion"`
	CrashCount     int       `json:"crashCount"`
	AffectedThings int       `json:"affectedNodes"`
	LastSeen       time.Time `json:"lastSeen"`
}

// ListCrashCohorts returns FATAL/crash events grouped by
// (agent_version, os, os_version) over [from, to).
func (store *Store) ListCrashCohorts(ctx context.Context, from, to time.Time) ([]CrashCohort, error) {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("list_crash_cohorts: from < to is required")
	}
	const q = `
		SELECT COALESCE(agent_version, '')           AS agent_version,
		       COALESCE(os_info->>'os', '')          AS os,
		       COALESCE(os_info->>'osVersion', '')   AS os_version,
		       COUNT(*)::INT                         AS crash_count,
		       COUNT(DISTINCT thing_id)              AS affected_things,
		       MAX(occurred_at)                      AS last_seen
		  FROM thing_diag_event
		 WHERE event_type = 'crash'
		   AND occurred_at >= $1 AND occurred_at < $2
		 GROUP BY agent_version, os_info->>'os', os_info->>'osVersion'
		 ORDER BY crash_count DESC, last_seen DESC
		 LIMIT 200
	`
	rows, err := store.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("list_crash_cohorts: %w", err)
	}
	defer rows.Close()

	out := make([]CrashCohort, 0, 16)
	for rows.Next() {
		var c CrashCohort
		if err := rows.Scan(&c.AgentVersion, &c.OS, &c.OSVersion, &c.CrashCount,
			&c.AffectedThings, &c.LastSeen); err != nil {
			return nil, fmt.Errorf("list_crash_cohorts scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_crash_cohorts iterate: %w", err)
	}
	return out, nil
}

// thing_diag_mode_window — diagnostic mode window operations

// DiagModeWindow is one row from thing_diag_mode_window. ThingType is joined
// from thing.type so the admin UI can render thing_type alongside.
type DiagModeWindow struct {
	ID        string    `json:"id"`
	ThingID   string    `json:"nodeId"`
	ThingType string    `json:"nodeType,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt"`
	SetBy     *string   `json:"setBy,omitempty"`
	Reason    *string   `json:"reason,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// EnableDiagModeParams describes a single-thing diag-mode enable.
type EnableDiagModeParams struct {
	ThingID string
	Until   time.Time
	SetBy   string // empty → NULL
	Reason  string
}

// EnableDiagMode opens (or replaces) the audit-history diag-mode window row.
// Delivery to the agent is the diag_mode override written separately by the
// handler. Returns ErrThingNotFound (a sentinel exposed below) if the thing
// does not exist.
func (store *Store) EnableDiagMode(ctx context.Context, p EnableDiagModeParams) (*DiagModeWindow, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("enable_diag_mode begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Confirm the thing exists. We rely on the FK from thing_diag_mode_window
	// to "thing"(id), but distinguishing 404 from 500 needs the explicit lookup.
	var existing string
	if err := tx.QueryRow(ctx, `SELECT id FROM thing WHERE id = $1`, p.ThingID).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrThingNotFound
		}
		return nil, fmt.Errorf("enable_diag_mode lookup: %w", err)
	}

	// Close any active window so the new one is the unique active record.
	if _, err := tx.Exec(ctx, `
		UPDATE thing_diag_mode_window
		   SET ended_at = NOW()
		 WHERE thing_id = $1
		   AND ended_at > NOW()
	`, p.ThingID); err != nil {
		return nil, fmt.Errorf("enable_diag_mode close prior: %w", err)
	}

	var setBy *string
	if p.SetBy != "" {
		s := p.SetBy
		setBy = &s
	}
	var reason *string
	if p.Reason != "" {
		s := p.Reason
		reason = &s
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO thing_diag_mode_window (id, thing_id, started_at, ended_at, set_by, reason, created_at)
		VALUES (gen_random_uuid(), $1, NOW(), $2, $3, $4, NOW())
		RETURNING id, thing_id, started_at, ended_at, set_by, reason, created_at
	`, p.ThingID, p.Until, setBy, reason)

	var w DiagModeWindow
	if err := row.Scan(&w.ID, &w.ThingID, &w.StartedAt, &w.EndedAt, &w.SetBy, &w.Reason, &w.CreatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			// FK violation on set_by — caller passed a NexusUser id that does not exist.
			return nil, fmt.Errorf("enable_diag_mode unknown setBy: %w", err)
		}
		return nil, fmt.Errorf("enable_diag_mode insert: %w", err)
	}

	// Delivery to the agent is the diag_mode thing_config_override (state
	// {until}, expires_at=until), written by the handler through the Hub
	// override API — Hub recomputes thing.desired, bumps desired_ver, and
	// pushes the key. This store owns only the audit-history window row; it
	// does not stamp thing.metadata.

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("enable_diag_mode commit: %w", err)
	}
	return &w, nil
}

// DisableDiagMode closes any active diag-mode window for the thing. The
// diag_mode override is cleared separately by the handler. Returns
// ErrNoActiveDiagMode when there is no open window (so the handler can 404).
func (store *Store) DisableDiagMode(ctx context.Context, thingID string) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("disable_diag_mode begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
		UPDATE thing_diag_mode_window
		   SET ended_at = NOW()
		 WHERE thing_id = $1
		   AND ended_at > NOW()
	`, thingID)
	if err != nil {
		return fmt.Errorf("disable_diag_mode close: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoActiveDiagMode
	}

	// The diag_mode override is cleared by the handler through the Hub
	// override API; this store only closes the audit-history window row.

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("disable_diag_mode commit: %w", err)
	}
	return nil
}

// ListActiveDiagModeWindows returns every window with ended_at > NOW(),
// joined to thing.type so the admin UI sees thing_type alongside.
func (store *Store) ListActiveDiagModeWindows(ctx context.Context) ([]DiagModeWindow, error) {
	const q = `
		SELECT w.id, w.thing_id, t.type, w.started_at, w.ended_at, w.set_by, w.reason, w.created_at
		  FROM thing_diag_mode_window w
		  JOIN thing t ON t.id = w.thing_id
		 WHERE w.ended_at > NOW()
		 ORDER BY w.started_at DESC
		 LIMIT 1000
	`
	rows, err := store.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list_active_diag_mode: %w", err)
	}
	defer rows.Close()

	out := make([]DiagModeWindow, 0, 16)
	for rows.Next() {
		var w DiagModeWindow
		if err := rows.Scan(&w.ID, &w.ThingID, &w.ThingType, &w.StartedAt, &w.EndedAt, &w.SetBy, &w.Reason, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("list_active_diag_mode scan: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list_active_diag_mode iterate: %w", err)
	}
	return out, nil
}

// BulkResolveAgentThings resolves the candidate thing IDs for a bulk diag-mode
// enable. Filter precedence: an explicit ThingIDs list wins; otherwise a
// metadata-staticInfo query against agent things is run with optional
// agentVersion / os filters. The result is capped at maxThings; the handler
// surfaces a 400 when the actual count exceeds the cap.
type BulkAgentFilter struct {
	ThingIDs     []string
	AgentVersion string
	OS           string
}

// ResolveBulkAgents returns the matching agent thing IDs (up to maxThings+1 so
// the caller can detect overflow).
func (store *Store) ResolveBulkAgents(ctx context.Context, f BulkAgentFilter, maxThings int) ([]string, error) {
	if maxThings <= 0 {
		maxThings = 500
	}
	if len(f.ThingIDs) > 0 {
		// Validate the provided ids: only return ones that exist AND are agents.
		const q = `
			SELECT id
			  FROM thing
			 WHERE id = ANY($1)
			   AND type = 'agent'
			 LIMIT $2
		`
		rows, err := store.pool.Query(ctx, q, f.ThingIDs, maxThings+1)
		if err != nil {
			return nil, fmt.Errorf("resolve_bulk_agents by id: %w", err)
		}
		defer rows.Close()
		out := make([]string, 0, len(f.ThingIDs))
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("resolve_bulk_agents scan: %w", err)
			}
			out = append(out, id)
		}
		return out, rows.Err()
	}
	// Attribute filter against thing.metadata.staticInfo. Both attributes are
	// optional; when both are empty the query returns every agent (capped).
	var agentVer, osName *string
	if f.AgentVersion != "" {
		s := f.AgentVersion
		agentVer = &s
	}
	if f.OS != "" {
		s := f.OS
		osName = &s
	}
	const q = `
		SELECT id
		  FROM thing
		 WHERE type = 'agent'
		   AND ($1::text IS NULL OR metadata->'staticInfo'->>'serviceVersion' = $1)
		   AND ($2::text IS NULL OR metadata->'staticInfo'->>'os' = $2)
		 ORDER BY id
		 LIMIT $3
	`
	rows, err := store.pool.Query(ctx, q, agentVer, osName, maxThings+1)
	if err != nil {
		return nil, fmt.Errorf("resolve_bulk_agents: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("resolve_bulk_agents scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// metric_ops_retention_config — retention layer config

// RetentionEntry is one (layer, retention_days) row.
type RetentionEntry struct {
	Layer         string    `json:"layer"`
	RetentionDays int       `json:"retentionDays"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// ListRetentionConfig returns every row from metric_ops_retention_config.
func (store *Store) ListRetentionConfig(ctx context.Context) ([]RetentionEntry, error) {
	const q = `
		SELECT layer, retention_days, updated_at
		  FROM metric_ops_retention_config
		 ORDER BY layer ASC
	`
	rows, err := store.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list_retention_config: %w", err)
	}
	defer rows.Close()
	out := make([]RetentionEntry, 0, 11)
	for rows.Next() {
		var e RetentionEntry
		if err := rows.Scan(&e.Layer, &e.RetentionDays, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list_retention_config scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateRetentionConfig atomically updates one or more layers in a single
// transaction. updates maps layer name → retention_days; updatedBy can be
// empty (NULL). updated_by is TEXT with FK to NexusUser(id) ON DELETE SET NULL,
// so admin API key principals (e.g. "nexus-user-super-admin"), agent UUIDs,
// or any other NexusUser.id are valid; an unknown id triggers a 23503 FK
// violation surfaced as the wrapped error.
func (store *Store) UpdateRetentionConfig(ctx context.Context, updates map[string]int, updatedBy string) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update_retention_config begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var by *string
	if updatedBy != "" {
		s := updatedBy
		by = &s
	}
	for layer, days := range updates {
		tag, err := tx.Exec(ctx, `
			UPDATE metric_ops_retention_config
			   SET retention_days = $1, updated_at = NOW(), updated_by = $2
			 WHERE layer = $3
		`, days, by, layer)
		if err != nil {
			return fmt.Errorf("update_retention_config %s: %w", layer, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("update_retention_config: unknown layer %q", layer)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("update_retention_config commit: %w", err)
	}
	return nil
}

// Sentinel errors

// ErrThingNotFound signals that the addressed thing does not exist. Handlers
// translate this into HTTP 404.
var ErrThingNotFound = errors.New("thing not found")

// ErrNoActiveDiagMode signals that a DELETE diagnostic-mode call found no
// open window. Handlers translate this into HTTP 404.
var ErrNoActiveDiagMode = errors.New("no active diagnostic mode window")
