package audit

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// recordToMessage builds the wire TrafficEventMessage from a Record. Splits
// the identity / entity / details derivation, applies cache-status unification,
// stamps Thing identity + payload-capture spill choices, and runs any wired
// normalize closure on the captured request/response bodies.
func (w *Writer) recordToMessage(rec *Record) *mq.TrafficEventMessage {
	// identity JSONB is the authoritative "who made this call" object.
	// Keys:
	//   vk            — virtual key that authenticated the call (always
	//                   present when VK auth resolved). Replaces the
	//                   previous misnamed "credential" key.
	//   user          — owning NexusUser (only for VKType="personal").
	//   project       — owning Project (only for VKType="application").
	//   apiCredential — upstream provider credential (Credential row),
	//                   orthogonal to caller identity.
	//   status        — "matched" when at least one of user/project
	//                   resolved at request time; "pending" when no
	//                   owner could be attached so the Hub
	//                   IdentityEnricher job picks the row up later via
	//                   DeviceAssignment.ip_address lookup.
	identity := map[string]any{}
	if rec.VirtualKeyID != "" {
		identity["vk"] = map[string]any{"id": rec.VirtualKeyID, "name": rec.VirtualKeyName}
	}
	if rec.UserID != "" {
		identity["user"] = map[string]any{"id": rec.UserID, "name": rec.UserDisplayName}
	}
	if rec.ProjectID != "" {
		identity["project"] = map[string]any{"id": rec.ProjectID, "name": rec.ProjectName}
	}
	if rec.CredentialID != "" {
		identity["apiCredential"] = map[string]any{"id": rec.CredentialID, "name": rec.CredentialName}
	}
	// "matched" iff at least one owner foreign key resolved; otherwise
	// "pending" so the Hub IdentityEnricher's IP-based resolver gets a
	// chance on the next 5-min tick.
	if rec.UserID != "" || rec.ProjectID != "" {
		identity["status"] = "matched"
	} else {
		identity["status"] = "pending"
	}

	// EntityType / EntityID drive the indexed analytics columns
	// (traffic_event.entity_id has a B-tree index for per-user / per-
	// project breakdown). Dispatch by VK type so the foreign key
	// downstream is consistent with the identity subtree above:
	//   personal VK    -> EntityType=user,    EntityID=NexusUser.id
	//   application VK -> EntityType=project, EntityID=Project.id
	//   anything else  -> EntityType="" (caller couldn't be classified)
	var entityType, entityID, entityName string
	switch {
	case rec.VKType == "personal" && rec.UserID != "":
		entityType, entityID, entityName = "user", rec.UserID, rec.UserDisplayName
	case rec.VKType == "application" && rec.ProjectID != "":
		entityType, entityID, entityName = "project", rec.ProjectID, rec.ProjectName
	}

	details := map[string]any{
		"requestId":              rec.RequestID,
		"clientRequestId":        rec.ClientRequestID,
		"sourceApp":              rec.SourceApp,
		"cacheKey":               rec.CacheKey,
		"responseHookReason":     rec.ResponseHookReason,
		"responseHookReasonCode": rec.ResponseHookReasonCode,
		"routingDecision":        rec.RoutingDecision,
		"qualitySignals":         rec.QualitySignals,
		"complianceFlags":        rec.ComplianceFlags,
		"metadata":               rec.Metadata,
	}
	if rec.HookRewritten {
		details["hookRewritten"] = true
		details["hookRewriteCount"] = rec.HookRewriteCount
	}
	if rec.ResponseHookRewritten {
		details["responseHookRewritten"] = true
		details["responseHookRewriteCount"] = rec.ResponseHookRewriteCount
	}

	msg := &mq.TrafficEventMessage{
		ID:     rec.RequestID,
		Source: "ai-gateway",
		// SourceProcess + Action carry the emitter taxonomy onto
		// traffic_event.source_process / .action. The consumer reads them
		// via stripNulPtr(e.SourceProcess) / stripNulPtr(e.Action); leaving
		// the fields empty here was the historical drift that wrote NULL.
		SourceProcess:          "ai-gateway",
		Action:                 "traffic",
		TraceID:                rec.TraceID,
		ExternalRequestID:      rec.ClientRequestID,
		Timestamp:              rec.Timestamp,
		SourceIP:               rec.SourceIP,
		TargetHost:             rec.TargetHost,
		Method:                 rec.Method,
		Path:                   rec.Path,
		TargetMethod:           firstNonEmptyStr(rec.TargetMethod, rec.Method),
		TargetPath:             firstNonEmptyStr(rec.TargetPath, rec.Path),
		StatusCode:             rec.StatusCode,
		LatencyMs:              rec.LatencyMs,
		EntityType:             entityType,
		EntityID:               entityID,
		EntityName:             entityName,
		OrgID:                  rec.OrganizationID,
		OrgName:                rec.OrganizationName,
		Identity:               identity,
		EndpointType:           rec.EndpointType,
		ProviderID:             rec.ProviderID,
		ProviderName:           rec.ProviderName,
		ModelID:                rec.ModelID,
		ModelName:              rec.ModelName,
		PromptTokens:           rec.PromptTokens,
		CompletionTokens:       rec.CompletionTokens,
		TotalTokens:            rec.TotalTokens,
		ReasoningTokens:        rec.ReasoningTokens,
		ReasoningCostUsd:       rec.ReasoningCostUsd,
		EstimatedCostUsd:       rec.EstimatedCostUsd,
		// Unified cache_status: if the producer already set rec.CacheStatus
		// (e.g. ai-guard), honor it. Otherwise derive from the two internal
		// statuses via DeriveCacheStatus.
		CacheStatus:            string(unifiedCacheStatus(rec)),
		GatewayCacheStatus:     string(rec.GatewayCacheStatus),
		GatewayCacheSkipReason: string(rec.GatewayCacheSkipReason),
		GatewayCacheKind:       string(rec.GatewayCacheKind),
		GatewayCacheL2EntryKey: rec.GatewayCacheL2EntryKey,
		ProviderCacheStatus:    string(rec.ProviderCacheStatus),
		RoutedProviderID:       rec.RoutedProviderID,
		RoutedProviderName:     rec.RoutedProviderName,
		RoutedModelID:          rec.RoutedModelID,
		RoutedModelName:        rec.RoutedModelName,
		RoutingRuleID:          rec.RoutingRuleID,
		RoutingRuleName:        rec.RoutingRuleName,
		// HooksPipeline is split by stage for the wire format: request-stage
		// executions land on request_hooks_pipeline and response-stage
		// executions on response_hooks_pipeline; "connection" stage stays
		// grouped with request since it occurs pre-upstream.
		RequestHookDecision:   rec.HookDecision,
		RequestHookReason:     rec.HookReason,
		RequestHookReasonCode: rec.HookReasonCode,
		ResponseHookDecision:  rec.ResponseHookDecision,
		ComplianceTags:        rec.ComplianceTags,
		APIKeyClass:           rec.APIKeyClass,
		APIKeyFingerprint:     rec.APIKeyFingerprint,
		UsageExtractionStatus: rec.UsageExtractionStatus,
		ErrorCode:             nilIfEmpty(rec.ErrorCode),
		ErrorReason:           nilIfEmpty(rec.ErrorReason),
		RequestHooksPipeline:  filterHookStage(rec.HooksPipeline, "request", "connection"),
		ResponseHooksPipeline: filterHookStage(rec.HooksPipeline, "response"),
		RoutingTrace:          rec.RoutingTrace,
		Details:               details,
		CredentialID:          rec.CredentialID,
		ThingID:               w.thingID,
		ThingName:             w.thingName,
		// Hook aggregates derive from the existing per-hook latencyMs values
		// in HooksPipeline so even callers that don't wire a PhaseTimer still
		// emit useful data. Upstream-side fields (Ttfb / Total) and the
		// long-tail breakdown require explicit instrumentation by the proxy
		// handler / executor.
		UpstreamTtfbMs:   rec.UpstreamTtfbMs,
		UpstreamTotalMs:  rec.UpstreamTotalMs,
		RequestHooksMs:   firstNonNil(rec.RequestHooksMs, sumHookLatenciesMs(rec.HooksPipeline, "request", "connection")),
		ResponseHooksMs:  firstNonNil(rec.ResponseHooksMs, sumHookLatenciesMs(rec.HooksPipeline, "response")),
		LatencyBreakdown: rec.LatencyBreakdown,
	}
	// Gateway response cache savings.
	if rec.GatewayCacheSavingsUsd != 0 {
		v := rec.GatewayCacheSavingsUsd
		msg.GatewayCacheSavingsUsd = &v
	}
	// Provider-side prompt cache metrics. Only set non-zero values
	// to keep the wire message compact; Hub consumer writes NULL for absent fields.
	if rec.CacheCreationTokens != 0 {
		v := rec.CacheCreationTokens
		msg.CacheCreationTokens = &v
	}
	if rec.CacheReadTokens != 0 {
		v := rec.CacheReadTokens
		msg.CacheReadTokens = &v
	}
	if rec.CacheWriteCostUsd != 0 {
		v := rec.CacheWriteCostUsd
		msg.CacheWriteCostUsd = &v
	}
	if rec.CacheReadSavingsUsd != 0 {
		v := rec.CacheReadSavingsUsd
		msg.CacheReadSavingsUsd = &v
	}
	if rec.CacheNetSavingsUsd != 0 {
		v := rec.CacheNetSavingsUsd
		msg.CacheNetSavingsUsd = &v
	}
	// Embedding cost + model on L1-miss paths that triggered an embed.
	if rec.EmbeddingCostUsd != 0 {
		v := rec.EmbeddingCostUsd
		msg.EmbeddingCostUsd = &v
	}
	if rec.EmbeddingModelID != "" {
		msg.EmbeddingModelID = rec.EmbeddingModelID
	}
	// ai-guard classifier cost. Stamped on rows where
	// internal_purpose='ai-guard' (the classify call's own row). NULL
	// on regular user-traffic rows.
	if rec.AIGuardCostUsd != 0 {
		v := rec.AIGuardCostUsd
		msg.AIGuardCostUsd = &v
	}
	if rec.NormalizedStripCount != 0 {
		v := rec.NormalizedStripCount
		msg.NormalizedStripCount = &v
	}
	if rec.NormalizedStripBytes != 0 {
		v := rec.NormalizedStripBytes
		msg.NormalizedStripBytes = &v
	}
	if rec.CacheMarkerInjected != 0 {
		v := rec.CacheMarkerInjected
		msg.CacheMarkerInjected = &v
	}
	if msg.TargetHost == "" { //nolint:staticcheck // keep existing fallback
		msg.TargetHost = rec.RoutedProviderName
	}
	// Default to BodyAbsent so the wire form discriminator is set even
	// when payload capture is off. spillstore.EmitBody decides between
	// inline (size <= MaxInlineBodyBytes) and spill (size >); when
	// w.spill is nil EmitBody always returns inline (matches the
	// no-spill-backend deployment shape).
	threshold := payloadcapture.DefaultMaxInlineBodyBytes
	if w.payloadCapture != nil {
		threshold = w.payloadCapture.Get().MaxInlineBodyBytes
	}
	ctx := context.Background()
	msg.RequestBody = spillstore.EmitBody(ctx, w.spill, threshold, rec.RequestBody, rec.RequestContentType, rec.RequestID, "request", rec.RequestTruncated, w.logger)
	msg.ResponseBody = spillstore.EmitBody(ctx, w.spill, threshold, rec.ResponseBody, rec.ResponseContentType, rec.RequestID, "response", rec.ResponseTruncated, w.logger)
	// Produce normalized payloads when a normalizer is wired and we have raw
	// bytes for the direction. Failures populate status + error_reason but
	// never block the wire message (normalize is observability, not a gate).
	// When the request's effective passthrough config has BypassNormalize=true,
	// skip the response-side normalize emission. Request-side normalize still
	// runs — it happens before passthrough is resolved, and the resulting
	// payload helps incident triage even when response normalize is bypassed.
	skipResponseNormalize := false
	for _, f := range rec.PassthroughFlags {
		if f == "bypassNormalize" {
			skipResponseNormalize = true
			break
		}
	}
	if w.normalize != nil {
		stream := strings.Contains(strings.ToLower(rec.ResponseContentType), "event-stream")
		if len(rec.RequestBody) > 0 {
			raw, status, errReason := w.normalize("request", rec.RequestContentType,
				normalizeAdapterType(rec, "request"), rec.ModelName, rec.Path, false, rec.RequestBody)
			raw = applyStorageAction(raw, rec.RequestStorageAction, rec.RequestTransformSpans, rec.RequestRedactRuleIDs)
			msg.RequestNormalized = raw
			msg.RequestNormalizeStatus = status
			msg.RequestNormalizeError = errReason
		}
		if len(rec.ResponseBody) > 0 && !skipResponseNormalize {
			raw, status, errReason := w.normalize("response", rec.ResponseContentType,
				normalizeAdapterType(rec, "response"), rec.ModelName, rec.Path, stream, rec.ResponseBody)
			raw = applyStorageAction(raw, rec.ResponseStorageAction, rec.ResponseTransformSpans, rec.ResponseRedactRuleIDs)
			msg.ResponseNormalized = raw
			msg.ResponseNormalizeStatus = status
			msg.ResponseNormalizeError = errReason
		}
		if msg.RequestNormalized != nil || msg.ResponseNormalized != nil {
			msg.NormalizeVersion = normalizeWireVersion
		}
	}
	if rec.InternalPurpose != "" {
		p := rec.InternalPurpose
		msg.InternalPurpose = &p
	}
	// Stamp passthrough audit fan-out on the wire envelope so Hub persists
	// onto traffic_event.passthrough_flags + passthrough_reason.
	// Empty/nil left as zero-value (omitempty in JSON tags) so unaffected
	// rows do not pay the wire/storage cost.
	if len(rec.PassthroughFlags) > 0 {
		msg.PassthroughFlags = rec.PassthroughFlags
		msg.PassthroughReason = rec.PassthroughReason
	}
	if rec.OriginTZ != "" {
		tz := rec.OriginTZ
		msg.OriginTZ = &tz
	}
	if rec.BlockingRule != nil {
		if b, err := json.Marshal(rec.BlockingRule); err == nil {
			raw := json.RawMessage(b)
			msg.RequestBlockingRule = &raw
		}
	}
	return msg
}
