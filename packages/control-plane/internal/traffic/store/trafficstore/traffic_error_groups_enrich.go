package trafficstore

// Enrichment for the rollup-first error-governance read: the top-100 cut,
// the per-class sample reason, and the exact affected-end-user count. Kept
// beside traffic_error_groups_rollup.go (same read path) on its own
// responsibility seam: everything here runs per DISPLAYED class against raw
// traffic_event, bounded by traffic_event_error_class_idx.

import (
	"context"
	"fmt"
	"sort"
	"time"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// assembleErrorClassGroups cuts the aggregation to the top-100 classes and
// enriches exactly those with a sample reason (LIMIT-1 point query) and,
// when affordable, the distinct affected-end-user count.
func (store *Store) assembleErrorClassGroups(ctx context.Context, p TrafficErrorGroupsParams, sources []string, agg map[string]*errClassAgg) ([]TrafficErrorGroup, error) {
	all := make([]*errClassAgg, 0, len(agg))
	for _, e := range agg {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].lastSeen.After(all[j].lastSeen)
	})
	if len(all) > 100 {
		all = all[:100]
	}

	out := make([]TrafficErrorGroup, 0, len(all))
	for _, e := range all {
		g := TrafficErrorGroup{
			ErrorCode:   e.class.ErrorCode,
			StatusRange: e.class.StatusRange,
			Provider:    e.class.Provider,
			Model:       e.class.Model,
			Count:       e.count,
			FirstSeen:   e.firstSeen,
			LastSeen:    e.lastSeen,
			Buckets:     make([]TrafficErrorBucket, 0, len(e.buckets)),
		}
		bins := make([]time.Time, 0, len(e.buckets))
		for ts := range e.buckets {
			bins = append(bins, ts)
		}
		sort.Slice(bins, func(i, j int) bool { return bins[i].Before(bins[j]) })
		for _, ts := range bins {
			g.Buckets = append(g.Buckets, TrafficErrorBucket{Ts: ts, Count: e.buckets[ts]})
		}

		// The overflow class has no underlying rows matching its key — its
		// enrichments stay empty/null by construction.
		if e.class.ErrorCode != metrics.ErrorClassOverflowCode {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			g.SampleReason = store.errorClassSampleReason(ctx, p, sources, e.class)
			if e.count <= affectedEndUsersMaxRows {
				n, hasRows, err := store.errorClassAffectedEndUsers(ctx, p, sources, e.class)
				if err != nil {
					return nil, err
				}
				// A rollup-counted class whose raw rows are gone (purged by
				// traffic_event retention) must report null, not a
				// fabricated exact 0.
				if hasRows || e.count == 0 {
					g.AffectedEndUsers = &n
				}
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// errorClassWhere extends the shared row filter with the exact class
// boundary. Placeholders $1..$3 are window+sources (trafficErrorWhere);
// $4..$7 are the class key values.
const errorClassWhere = trafficErrorWhere + `
	  AND COALESCE(a.error_code, '') = $4
	  AND CASE WHEN a.status_code >= 500 THEN '5xx' ELSE '4xx' END = $5
	  AND COALESCE(a.routed_provider_name, a.provider_name, '') = $6
	  AND COALESCE(a.routed_model_name, a.model_name, '') = $7`

// errorClassSampleReason fetches one representative error_reason for the
// class — a single index-bounded point query per displayed class. Degrades
// to "" on no-rows and on transient failure alike; a missing sample must
// never fail the whole aggregation.
func (store *Store) errorClassSampleReason(ctx context.Context, p TrafficErrorGroupsParams, sources []string, class metrics.ErrorClass) string {
	q := fmt.Sprintf(`
		SELECT COALESCE(a.error_reason, '')
		  FROM traffic_event a
		 WHERE %s
		   AND a.error_reason IS NOT NULL AND a.error_reason <> ''
		 ORDER BY a.timestamp DESC
		 LIMIT 1`, errorClassWhere)
	var reason string
	if err := store.pool.QueryRow(ctx, q, p.From, p.To, sources,
		class.ErrorCode, class.StatusRange, class.Provider, class.Model).Scan(&reason); err != nil {
		return ""
	}
	return reason
}

// errorClassAffectedEndUsers computes the exact distinct end-user count for
// one class, plus whether ANY raw rows still back the class (false when
// traffic_event retention purged them — the caller reports null instead of
// a fabricated 0). Only called when the class's rollup count is at or below
// affectedEndUsersMaxRows, so the scan is bounded.
func (store *Store) errorClassAffectedEndUsers(ctx context.Context, p TrafficErrorGroupsParams, sources []string, class metrics.ErrorClass) (int, bool, error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*)::INT, COUNT(DISTINCT a.end_user_id)::INT
		  FROM traffic_event a
		 WHERE %s`, errorClassWhere)
	var rows, n int
	err := store.pool.QueryRow(ctx, q, p.From, p.To, sources,
		class.ErrorCode, class.StatusRange, class.Provider, class.Model).Scan(&rows, &n)
	if err != nil {
		return 0, false, fmt.Errorf("error-class affected end users: %w", err)
	}
	return n, rows > 0, nil
}
