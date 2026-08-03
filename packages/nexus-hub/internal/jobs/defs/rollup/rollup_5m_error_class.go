package rollup

import (
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// errorClassBucketCap bounds the distinct error classes emitted per 5-minute
// bucket. The model part of the class key is a caller-controlled string, so
// without a cap a client spraying unique model names through a rejecting
// endpoint would mint one rollup row per request. Classes beyond the cap fold
// into one overflow class per (status range, sub-dimension) with
// ErrorClassOverflowCode, so the total error count stays exact even when
// class identities are shed.
const errorClassBucketCap = 500

// errorClassBucketHardCap bounds the accumulator's IN-MEMORY class count
// while a bucket is being scanned — the output cap above is applied at
// drain, which by itself would let a unique-model spray grow the maps
// unboundedly first. Once this many distinct classes are admitted, further
// NEW classes fold straight into the overflow class for their status range.
// Which classes get admitted then depends on scan order, so under an active
// spray the per-class identities are best-effort (totals stay exact); below
// the hard cap the accumulation — and therefore bucket re-runs — remain
// fully deterministic.
const errorClassBucketHardCap = 2000

// errorClassPartMaxLen truncates the provider/model parts of the class key.
// The model part originates from the raw client request body and has no
// upstream length bound; unbounded parts would make each admitted class an
// attacker-sized allocation. Names beyond this length are absurd inputs —
// their classes diverge from the (untruncated) direct path by design.
const errorClassPartMaxLen = 256

// errorClassAcc accumulates the traffic.error_class.count /
// traffic.error_class.seen series for one 5-minute bucket. It is filled
// per-event by observe and drained once by foldInto, which applies the
// deterministic cap before handing rows to the shared assemblers.
type errorClassAcc struct {
	counts map[accKey5m]float64
	seen   map[accKey5m]metrics.TimestampMeta
	// classTotals sums counts per dimension key across sub-dimensions so the
	// cap keeps whole classes, not per-source slices.
	classTotals map[string]float64
}

func newErrorClassAcc() *errorClassAcc {
	return &errorClassAcc{
		counts:      make(map[accKey5m]float64),
		seen:        make(map[accKey5m]metrics.TimestampMeta),
		classTotals: make(map[string]float64),
	}
}

// coalesceName mirrors SQL COALESCE(routed, requested, ”): NULL (nil) falls
// through, an empty non-NULL string is kept — the direct error-governance
// query uses exactly that expression, and the two paths must produce
// byte-identical class keys (up to the length truncation below).
func coalesceName(routed, requested *string) string {
	s := ""
	if routed != nil {
		s = *routed
	} else if requested != nil {
		s = *requested
	}
	if len(s) > errorClassPartMaxLen {
		// Cut on a rune boundary — a mid-rune byte slice would produce an
		// invalid-UTF-8 dimension key that PostgreSQL rejects (failing the
		// whole bucket's insert).
		cut := errorClassPartMaxLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// observe records one traffic_event row into the error-class series. Mirrors
// the direct view's row filter: failed rows only, internal operational
// traffic excluded. The caller has already skipped rows outside the
// data-plane source mapping.
func (a *errorClassAcc) observe(
	statusCode *int, internalPurpose, errorCode *string,
	routedProviderName, providerName, routedModelName, modelName *string,
	subDim string, ts time.Time,
) {
	sc := derefInt5m(statusCode)
	if sc < 400 {
		return
	}
	if deref5m(internalPurpose) != "" {
		return
	}
	statusRange := "4xx"
	if sc >= 500 {
		statusRange = "5xx"
	}
	class := metrics.ErrorClass{
		ErrorCode:   deref5m(errorCode),
		StatusRange: statusRange,
		Provider:    coalesceName(routedProviderName, providerName),
		Model:       coalesceName(routedModelName, modelName),
	}
	dimKey := metrics.BuildDimensionKey(metrics.DimensionErrorClass, metrics.BuildErrorClassValue(class))

	// Hard admission bound: a NEW class beyond the in-memory cap folds
	// straight into the overflow class for its status range so accumulator
	// memory cannot grow with a unique-name spray.
	if _, known := a.classTotals[dimKey]; !known && len(a.classTotals) >= errorClassBucketHardCap {
		class = metrics.ErrorClass{ErrorCode: metrics.ErrorClassOverflowCode, StatusRange: statusRange}
		dimKey = metrics.BuildDimensionKey(metrics.DimensionErrorClass, metrics.BuildErrorClassValue(class))
	}

	ck := accKey5m{metrics.MetricTrafficErrorClassCount, dimKey, subDim}
	a.counts[ck]++
	a.classTotals[dimKey]++

	tsStr := ts.UTC().Format(time.RFC3339)
	sk := accKey5m{metrics.MetricTrafficErrorClassSeen, dimKey, subDim}
	a.seen[sk] = metrics.MergeTimestampMeta(a.seen[sk], metrics.TimestampMeta{FirstSeen: tsStr, LastSeen: tsStr})
}

// foldInto applies the per-bucket class cap and merges the surviving rows
// into the shared scalar/timestamp accumulators that assembleRollupRows
// drains. Selection is deterministic — classes rank by total count
// descending, then dimension key ascending — so re-running a bucket
// (idempotent DELETE+INSERT, correction backfill) reproduces identical rows.
func (a *errorClassAcc) foldInto(
	logger *slog.Logger,
	accValues map[accKey5m]float64,
	accTimestamp map[accKey5m]metrics.TimestampMeta,
) {
	overflow := map[string]struct{}{}
	if len(a.classTotals) > errorClassBucketCap {
		keys := make([]string, 0, len(a.classTotals))
		for k := range a.classTotals {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			ti, tj := a.classTotals[keys[i]], a.classTotals[keys[j]]
			if ti != tj {
				return ti > tj
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys[errorClassBucketCap:] {
			overflow[k] = struct{}{}
		}
		logger.Warn("error-class bucket cap tripped — folding excess classes into overflow",
			"distinct_classes", len(a.classTotals),
			"cap", errorClassBucketCap,
			"folded", len(overflow),
		)
	}

	// overflowKey rewrites a folded class's dimension key to the overflow
	// class for its status range, preserving the sub-dimension.
	overflowKey := func(k accKey5m) accKey5m {
		value := strings.TrimPrefix(k.dimensionKey, metrics.DimensionErrorClass+"=")
		class, ok := metrics.ParseErrorClassValue(value)
		if !ok {
			// Cannot happen for keys this accumulator built; keep the row
			// rather than dropping counts.
			return k
		}
		ovf := metrics.ErrorClass{ErrorCode: metrics.ErrorClassOverflowCode, StatusRange: class.StatusRange}
		return accKey5m{k.metricName, metrics.BuildDimensionKey(metrics.DimensionErrorClass, metrics.BuildErrorClassValue(ovf)), k.subDimension}
	}

	for k, v := range a.counts {
		out := k
		if _, folded := overflow[k.dimensionKey]; folded {
			out = overflowKey(k)
		}
		accValues[out] += v
	}
	for k, ts := range a.seen {
		out := k
		if _, folded := overflow[k.dimensionKey]; folded {
			out = overflowKey(k)
		}
		accTimestamp[out] = metrics.MergeTimestampMeta(accTimestamp[out], ts)
	}
}
