package trafficstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// TrafficErrorGroupsParams bounds the error-governance aggregation.
// From/To are required (half-open [From, To)). DBSources restricts to
// specific traffic_event.source values; empty = all data-plane sources.
// BucketInterval is the sparkline bin width the handler picked for the
// window size.
type TrafficErrorGroupsParams struct {
	From           time.Time
	To             time.Time
	DBSources      []string
	BucketInterval time.Duration
}

// TrafficErrorBucket is one sparkline bin of a TrafficErrorGroup.
type TrafficErrorBucket struct {
	Ts    time.Time `json:"ts"`
	Count int       `json:"count"`
}

// TrafficErrorGroup is one aggregated error class from the traffic log:
// every non-2xx traffic_event row in the window, grouped by
// (errorCode, statusRange, provider, model). Attribution is computed by
// the handler layer (first-suspect triage bucket), never stored.
type TrafficErrorGroup struct {
	// ErrorCode is the raw stored failure classification; empty when the
	// producer did not classify (e.g. compliance rejections, raw upstream
	// 5xx pass-through).
	ErrorCode   string `json:"errorCode"`
	StatusRange string `json:"statusRange"` // "4xx" | "5xx"
	// Provider/Model attribute by what actually SERVED the request
	// (routed_*), falling back to the requested name; empty when neither
	// is stamped (early rejections).
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SampleReason string `json:"sampleReason"`
	Count        int    `json:"count"`
	// AffectedEndUsers counts DISTINCT caller-declared end-user tags in
	// the group; 0 when callers send no X-Nexus-End-User-Id. Null (nil)
	// when the count was not computed — the rollup read path skips the
	// distinct scan for classes above its affordability threshold and the
	// UI shows "—" instead of a fabricated number.
	AffectedEndUsers *int                 `json:"affectedEndUsers"`
	FirstSeen        time.Time            `json:"firstSeen"`
	LastSeen         time.Time            `json:"lastSeen"`
	Buckets          []TrafficErrorBucket `json:"buckets"`
	// Attribution is filled by the handler from the error-code taxonomy:
	// "ours" | "client" | "upstream".
	Attribution string `json:"attribution"`
}

// trafficErrorGroupKeyExprs are the four grouping expressions. Kept as one
// constant so the group query and the bucket query can never drift.
const trafficErrorGroupKeyExprs = `
	COALESCE(a.error_code, '')                                            AS error_code,
	CASE WHEN a.status_code >= 500 THEN '5xx' ELSE '4xx' END              AS status_range,
	COALESCE(a.routed_provider_name, a.provider_name, '')                 AS provider,
	COALESCE(a.routed_model_name, a.model_name, '')                       AS model`

// trafficErrorWhere is the shared row filter: failed data-plane rows in the
// window, internal operational traffic (ai-guard classify calls) excluded so
// the governance view reflects customer-visible failures only.
const trafficErrorWhere = `
	  a.timestamp >= $1 AND a.timestamp < $2
	  AND a.status_code >= 400
	  AND a.source = ANY($3)
	  AND (a.internal_purpose IS NULL OR a.internal_purpose = '')`

// ListTrafficErrorGroups aggregates failed traffic rows into top-100 error
// classes (by occurrence count), then fetches per-bucket sparkline counts
// for exactly those classes in a second pass — same two-query shape as the
// diag-events groups store, so the group-level DISTINCT counts stay exact
// (a single-pass GROUP BY including the bucket would make COUNT(DISTINCT
// end_user_id) unsummable across buckets).
func (store *Store) ListTrafficErrorGroups(ctx context.Context, p TrafficErrorGroupsParams) ([]TrafficErrorGroup, error) {
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) {
		return nil, errors.New("list_traffic_error_groups: from < to is required")
	}
	if p.BucketInterval <= 0 {
		return nil, errors.New("list_traffic_error_groups: bucket interval must be positive")
	}
	sources := p.DBSources
	if len(sources) == 0 {
		sources = domain.AllDataPlaneDBSources()
	}

	// NULLIF keeps the sample a real reason: a bare MIN over COALESCE'd
	// empties would collapse to '' whenever any row in the group lacks one.
	q := fmt.Sprintf(`
		SELECT %s,
		       COALESCE(MIN(NULLIF(a.error_reason, '')), '') AS sample_reason,
		       COUNT(*)::INT                            AS cnt,
		       COUNT(DISTINCT a.end_user_id)::INT       AS affected_end_users,
		       MIN(a.timestamp)                         AS first_seen,
		       MAX(a.timestamp)                         AS last_seen
		  FROM traffic_event a
		 WHERE %s
		 GROUP BY 1, 2, 3, 4
		 ORDER BY cnt DESC, last_seen DESC
		 LIMIT 100`, trafficErrorGroupKeyExprs, trafficErrorWhere)

	rows, err := store.pool.Query(ctx, q, p.From, p.To, sources)
	if err != nil {
		return nil, fmt.Errorf("list traffic error groups: %w", err)
	}

	out := make([]TrafficErrorGroup, 0, 32)
	codes := make([]string, 0, 32)
	ranges := make([]string, 0, 32)
	providers := make([]string, 0, 32)
	models := make([]string, 0, 32)
	for rows.Next() {
		var g TrafficErrorGroup
		var affected int
		if err := rows.Scan(&g.ErrorCode, &g.StatusRange, &g.Provider, &g.Model,
			&g.SampleReason, &g.Count, &affected, &g.FirstSeen, &g.LastSeen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan traffic error group: %w", err)
		}
		g.AffectedEndUsers = &affected
		g.Buckets = []TrafficErrorBucket{}
		out = append(out, g)
		codes = append(codes, g.ErrorCode)
		ranges = append(ranges, g.StatusRange)
		providers = append(providers, g.Provider)
		models = append(models, g.Model)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traffic error groups: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	// Sparkline bins for exactly the selected groups. The derived table
	// reuses the same key-expression constant as the group query (no third
	// hand-written copy to drift), and the tuple-IN against unnest'd
	// parallel arrays restricts the second pass to the top-100 classes so
	// its cardinality stays groups x bins.
	bq := fmt.Sprintf(`
		SELECT sub.error_code, sub.status_range, sub.provider, sub.model,
		       date_bin($4, sub.ts, TIMESTAMPTZ '2000-01-01') AS bucket_ts,
		       COUNT(*)::INT                                   AS cnt
		  FROM (SELECT %s, a.timestamp AS ts
		          FROM traffic_event a
		         WHERE %s) sub
		 WHERE (sub.error_code, sub.status_range, sub.provider, sub.model)
		       IN (SELECT * FROM unnest($5::text[], $6::text[], $7::text[], $8::text[]))
		 GROUP BY 1, 2, 3, 4, 5
		 ORDER BY 1, 2, 3, 4, 5`, trafficErrorGroupKeyExprs, trafficErrorWhere)

	brows, err := store.pool.Query(ctx, bq, p.From, p.To, sources, p.BucketInterval, codes, ranges, providers, models)
	if err != nil {
		return nil, fmt.Errorf("list traffic error buckets: %w", err)
	}
	defer brows.Close()

	idx := make(map[string]int, len(out))
	for i, g := range out {
		idx[g.ErrorCode+"\x00"+g.StatusRange+"\x00"+g.Provider+"\x00"+g.Model] = i
	}
	for brows.Next() {
		var code, rng, prov, model string
		var b TrafficErrorBucket
		if err := brows.Scan(&code, &rng, &prov, &model, &b.Ts, &b.Count); err != nil {
			return nil, fmt.Errorf("scan traffic error bucket: %w", err)
		}
		if i, ok := idx[code+"\x00"+rng+"\x00"+prov+"\x00"+model]; ok {
			out[i].Buckets = append(out[i].Buckets, b)
		}
	}
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traffic error buckets: %w", err)
	}
	return out, nil
}
