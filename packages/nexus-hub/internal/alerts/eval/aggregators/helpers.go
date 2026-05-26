package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
)

// EvalCountInWindow returns a Fire Decision when count over windowSec
// reaches threshold. Returns nil otherwise. Caller is responsible for
// having previously called rt.Window(targetKey, windowSec).Add() during
// OnEvent for matching events.
func EvalCountInWindow(rt *alerteval.Runtime, targetKey string, windowSec, threshold int, now time.Time, message string) *alerteval.Decision {
	w := rt.Window(targetKey, windowSec)
	count, _ := w.Sum(time.Duration(windowSec)*time.Second, now)
	if int(count) < threshold {
		// If we previously fired and the count has dropped below threshold,
		// resolve.
		if rt.HasFired(targetKey) {
			return &alerteval.Decision{
				Action:    alerteval.Resolve,
				TargetKey: targetKey,
				Message:   fmt.Sprintf("%s (recovered: count=%d < %d)", message, int(count), threshold),
			}
		}
		return nil
	}
	return &alerteval.Decision{
		Action:    alerteval.Fire,
		TargetKey: targetKey,
		Message:   message,
		Details: map[string]any{
			"count":     int(count),
			"threshold": threshold,
			"windowSec": windowSec,
		},
	}
}

// EvalRatioInWindow returns a Fire Decision when (num/denom)*100 >=
// thresholdPct AND denom >= minSamples. Returns a Resolve Decision when
// the ratio drops below (threshold-1)pp (1pp hysteresis). Otherwise nil.
//
// Caller's OnEvent is responsible for Add'ing (1, 1) for "matching event"
// or (0, 1) for "non-matching event" so the bucket holds (num, denom).
func EvalRatioInWindow(rt *alerteval.Runtime, targetKey string, windowSec, thresholdPct, minSamples int, now time.Time, message string) *alerteval.Decision {
	w := rt.Window(targetKey, windowSec)
	num, denom := w.Sum(time.Duration(windowSec)*time.Second, now)
	if int(denom) < minSamples {
		if rt.HasFired(targetKey) {
			return &alerteval.Decision{
				Action:    alerteval.Resolve,
				TargetKey: targetKey,
				Message:   fmt.Sprintf("%s (recovered: samples=%d < %d)", message, int(denom), minSamples),
			}
		}
		return nil
	}
	pct := (num / denom) * 100.0

	if pct >= float64(thresholdPct) {
		return &alerteval.Decision{
			Action:    alerteval.Fire,
			TargetKey: targetKey,
			Message:   message,
			Details: map[string]any{
				"pct":          pct,
				"num":          int(num),
				"denom":        int(denom),
				"thresholdPct": thresholdPct,
				"windowSec":    windowSec,
			},
		}
	}
	if pct < float64(thresholdPct-1) && rt.HasFired(targetKey) {
		return &alerteval.Decision{
			Action:    alerteval.Resolve,
			TargetKey: targetKey,
			Message:   fmt.Sprintf("%s (recovered: %.1f%% < %d%%)", message, pct, thresholdPct-1),
		}
	}
	return nil
}

// EvalSumInWindow returns a Fire Decision when the sum over windowSec >=
// thresholdValue. Bucket's a is the metric (e.g. cost), b is the count
// (used only for diagnostics).
func EvalSumInWindow(rt *alerteval.Runtime, targetKey string, windowSec int, thresholdValue float64, now time.Time, message string) *alerteval.Decision {
	w := rt.Window(targetKey, windowSec)
	sum, count := w.Sum(time.Duration(windowSec)*time.Second, now)
	if sum < thresholdValue {
		if rt.HasFired(targetKey) {
			return &alerteval.Decision{
				Action:    alerteval.Resolve,
				TargetKey: targetKey,
				Message:   fmt.Sprintf("%s (recovered: sum=%.2f < %.2f)", message, sum, thresholdValue),
			}
		}
		return nil
	}
	return &alerteval.Decision{
		Action:    alerteval.Fire,
		TargetKey: targetKey,
		Message:   message,
		Details: map[string]any{
			"sum":            sum,
			"count":          int(count),
			"thresholdValue": thresholdValue,
			"windowSec":      windowSec,
		},
	}
}

// EvalPercentileBaseline compares the percentile of the most-recent
// alertWindow against the percentile of the baseline window for the same
// target. Fires when:
//
//	alertPct >= absFloor       (absolute floor — don't fire on tiny values)
//	alertSamples >= minSamples (avoid noise)
//	alertPct > multiplier * baselinePct  (trending up)
//
// Used by provider.high_latency_percentile and vk.latency_degradation.
// Caller's OnEvent must Add the sample (e.g. latency_ms) into a
// SampleWindow capped at samplesCap (e.g. 1000) sized to baseline+alert.
func EvalPercentileBaseline(rt *alerteval.Runtime, targetKey string, alertWindowSec, baselineWindowSec int, percentile, multiplier, absFloor float64, minSamples int, samplesCap int, now time.Time, message string) *alerteval.Decision {
	w := rt.SampleWindow(targetKey, samplesCap)
	alertPct, alertN := w.Percentile(time.Duration(alertWindowSec)*time.Second, now, percentile)
	if alertN < minSamples {
		return nil
	}
	if alertPct < absFloor {
		return nil
	}
	// Baseline window: same percentile measured over (baseline+alert)
	// minus the alert tail. SampleWindow doesn't slice that elegantly, so
	// we approximate: take the percentile over the full baseline+alert
	// window and treat it as the "comparison" point. For latency this
	// approximation is fine — slow tails dominate the percentile already.
	basePct, _ := w.Percentile(time.Duration(baselineWindowSec+alertWindowSec)*time.Second, now, percentile)
	if basePct <= 0 {
		// First-tick edge case: no baseline yet. Don't fire.
		return nil
	}
	if alertPct > multiplier*basePct {
		return &alerteval.Decision{
			Action:    alerteval.Fire,
			TargetKey: targetKey,
			Message:   message,
			Details: map[string]any{
				"percentile":         percentile,
				"alertPercentile":    alertPct,
				"baselinePercentile": basePct,
				"multiplier":         multiplier,
				"absFloor":           absFloor,
				"alertWindowSec":     alertWindowSec,
				"baselineWindowSec":  baselineWindowSec,
				"alertSamples":       alertN,
			},
		}
	}
	return nil
}

// EvalCompareToBaseline returns a Fire Decision when the count over
// alertWindowSec > spikeMultiplier × avg(per-alertWindow chunks over
// baselineWindowSec) AND >= absFloorReq. Used by vk.traffic_spike.
//
// Caller's OnEvent must have called rt.Window(target, baselineSec+alertSec).Add(time, 1, 0)
// per matching event.
func EvalCompareToBaseline(rt *alerteval.Runtime, targetKey string, alertWindowSec, baselineWindowSec int, spikeMultiplier float64, absFloorReq int, now time.Time, message string) *alerteval.Decision {
	w := rt.Window(targetKey, baselineWindowSec+alertWindowSec)

	lastWindow, _ := w.Sum(time.Duration(alertWindowSec)*time.Second, now)
	if int(lastWindow) < absFloorReq {
		return nil
	}

	totalAll, _ := w.Sum(time.Duration(baselineWindowSec+alertWindowSec)*time.Second, now)
	baselineTotal := totalAll - lastWindow
	chunks := float64(baselineWindowSec) / float64(alertWindowSec)
	if chunks < 1 {
		chunks = 1
	}
	baselineAvg := baselineTotal / chunks

	if lastWindow > spikeMultiplier*baselineAvg {
		return &alerteval.Decision{
			Action:    alerteval.Fire,
			TargetKey: targetKey,
			Message:   message,
			Details: map[string]any{
				"lastWindow":        int(lastWindow),
				"baselineAvg":       baselineAvg,
				"spikeMultiplier":   spikeMultiplier,
				"alertWindowSec":    alertWindowSec,
				"baselineWindowSec": baselineWindowSec,
				"absFloorReq":       absFloorReq,
			},
		}
	}
	return nil
}
