package consumer

import (
	"reflect"
	"testing"
	"time"
)

// benchTrafficEvent builds one fully-populated event to drive the row builder.
//
// The string fields are filled by reflection rather than by hand. A hand-written
// literal covering ninety-odd columns drifts the moment a column is added, and it
// drifts in the direction that hides work: a field left at its zero value takes
// the nil branch and the benchmark quietly stops measuring it. Reflection fills
// whatever the struct currently declares, so a new column is measured the day it
// lands. Numeric and time fields keep their zero values — they cost the same
// boxed either way, and the row builder has no branch on them.
func benchTrafficEvent() TrafficEventMessage {
	var e TrafficEventMessage
	v := reflect.ValueOf(&e).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("a-representative-value")
		case reflect.Pointer:
			if f.Type().Elem().Kind() == reflect.String {
				s := "a-representative-value"
				f.Set(reflect.ValueOf(&s))
			}
		}
	}
	e.Timestamp = time.Unix(1756000000, 0).UTC()
	e.ComplianceTags = []string{"pii", "gdpr"}
	return e
}

// BenchmarkAppendTrafficEventRow measures the per-event row builder on the hub's
// highest-volume path: every traffic_event the fleet produces passes through it
// once for the COPY staging load, and again through the identical value builder
// if the batch falls back to the pgx.Batch path.
func BenchmarkAppendTrafficEventRow(b *testing.B) {
	e := benchTrafficEvent()
	dst := make([]any, 0, len(trafficEventColumns))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dst = appendTrafficEventRow(dst[:0], e)
	}
	_ = dst
}

// BenchmarkAppendTrafficEventRow_BoxingFloor is the control arm. It appends the
// SAME fields in the same order onto the same pre-sized buffer, but reads each
// one straight off the struct instead of routing it through stripNul /
// stripNulPtr / nullableJSON.
//
// Without this arm the first benchmark's number is unattributable: boxing ninety
// values into []any allocates on its own, and a measurement that cannot separate
// that from the helpers' cost is the shape that made an earlier round of this
// work optimise a test harness. The difference between the two arms is what the
// NUL-stripping layer actually costs.
func BenchmarkAppendTrafficEventRow_BoxingFloor(b *testing.B) {
	e := benchTrafficEvent()
	dst := make([]any, 0, len(trafficEventColumns))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dst = appendRawRow(dst[:0], e)
	}
	_ = dst
}

// appendRawRow mirrors appendTrafficEventRow's shape with the stripping removed.
// It exists only as the control arm's body and is never wired into production.
func appendRawRow(dst []any, e TrafficEventMessage) []any {
	return append(dst,
		e.ID, e.Source, e.TraceID, e.ExternalRequestID, e.Timestamp,
		e.SourceIP, e.TargetHost, e.Method, e.Path, e.StatusCode, e.LatencyMs,
		e.EntityType, e.EntityID, e.EntityName, e.OrgID, e.OrgName,
		e.Identity,
		e.ProviderID, e.ProviderName, e.ModelID, e.ModelName,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.EstimatedCostUSD,
		e.CacheStatus,
		e.RoutedProviderID, e.RoutedProviderName, e.RoutedModelID, e.RoutedModelName,
		e.RoutingRuleID, e.RoutingRuleName,
		e.RequestHookDecision, e.RequestHookReason, e.RequestHookReasonCode,
		e.ResponseHookDecision, e.ResponseHookReason, e.ResponseHookReasonCode,
		e.ComplianceTags, e.BumpStatus,
		e.APIKeyClass, e.APIKeyFingerprint, e.UsageExtractionStatus,
		e.SourceProcess, e.Action,
		e.RequestHooksPipeline, e.ResponseHooksPipeline,
		e.RoutingTrace, e.Details,
		e.InternalPurpose,
		e.RequestBlockingRule, e.ResponseBlockingRule,
		e.OriginTZ,
		e.ErrorCode, e.ErrorReason,
		e.CacheCreationTokens, e.CacheReadTokens,
		e.NormalizedStripCount, e.NormalizedStripBytes, e.CacheMarkerInjected,
		e.CacheWriteCostUsd, e.CacheReadSavingsUsd, e.CacheNetSavingsUsd,
		e.GatewayCacheSavingsUsd,
		e.ThingID, e.ThingName,
		e.CredentialID,
		e.PassthroughFlags, e.PassthroughReason,
		e.UpstreamTtfbMs, e.UpstreamTotalMs,
		e.RequestHooksMs, e.ResponseHooksMs,
		e.LatencyBreakdown,
		e.ReasoningTokens,
		e.ReasoningCostUsd,
		e.TargetMethod, e.TargetPath,
		e.GatewayCacheStatus, e.GatewayCacheSkipReason, e.GatewayCacheKind, e.ProviderCacheStatus,
		e.AttestationVerified, e.AttestationAgentID,
		e.EmbeddingCostUsd, e.EmbeddingModelID,
		e.AIGuardCostUsd, e.InternalOpsBreakdown,
		e.GatewayCacheL2EntryKey,
		e.EndpointType,
		e.IngressFormat,
		e.RequestHooksUs, e.ResponseHooksUs,
		e.ArtifactRefs, e.ComplianceCoverage,
		e.EndUserID, e.SessionID,
		e.RouterCostUsd, e.RouterProviderID, e.EmbeddingProviderID,
	)
}
