package opsmetrics

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
)

// HistogramBucketCount mirrors the canonical 6-bucket layout used by the
// agent registry (shared/opsmetrics.HistogramBucketsMs has 5 finite bounds
// plus +Inf, so 6 cumulative slots total). Spec §6.4.
const HistogramBucketCount = 6

// HistogramBuckets is a fixed-size element-wise sum target. Encoded into
// metric_ops_raw.metadata as `{"buckets": [...]}`.
type HistogramBuckets [HistogramBucketCount]int64

// ParseHistogramBuckets decodes a `{"buckets":[...]}` JSON payload from a
// metric_ops_raw row. Accepts JSON numbers (which always decode to float64
// through encoding/json) and silently rounds to int64. Missing or extra
// elements are zero-padded / truncated so a malformed row never crashes the
// rollup job.
func ParseHistogramBuckets(raw []byte) (HistogramBuckets, error) {
	var out HistogramBuckets
	if len(raw) == 0 {
		return out, nil
	}

	var wrapper struct {
		Buckets []json.Number `json:"buckets"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&wrapper); err != nil {
		return out, fmt.Errorf("parse histogram buckets: %w", err)
	}
	for i, n := range wrapper.Buckets {
		if i >= HistogramBucketCount {
			break
		}
		v, err := n.Int64()
		if err != nil {
			// Fall back to float interpretation; floor to int64.
			f, ferr := n.Float64()
			if ferr != nil {
				continue
			}
			v = int64(f)
		}
		out[i] = v
	}
	return out, nil
}

// MergeHistogramBuckets returns the element-wise sum of two histograms.
// Used by the ops 1h rollup job to fold N raw histogram rows into one row.
func MergeHistogramBuckets(a, b HistogramBuckets) HistogramBuckets {
	var out HistogramBuckets
	for i := range out {
		out[i] = a[i] + b[i]
	}
	return out
}

// HistogramMetadata is the JSON payload written back to
// metric_ops_rollup_1h.metadata for histogram rows.
type HistogramMetadata struct {
	Buckets [HistogramBucketCount]int64 `json:"buckets"`
}

// EncodeHistogramBuckets serialises a HistogramBuckets to the canonical JSON
// shape expected by the analytics readers.
func EncodeHistogramBuckets(h HistogramBuckets) ([]byte, error) {
	return json.Marshal(HistogramMetadata{Buckets: h})
}

// SumHistogramBuckets returns the total count across all buckets — used by the
// rollup job to populate value_sum for histogram rows so consumers that only
// ever look at scalar columns still see a usable count.
func SumHistogramBuckets(h HistogramBuckets) int64 {
	var s int64
	for _, c := range h {
		s += c
	}
	return s
}
