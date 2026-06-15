// Package redact applies a compliance hook's onMatch.storageAction to
// every copy of matched traffic that an audit producer is about to
// persist — the raw captured wire body and the normalized payload
// sidecar. All three data-plane services (ai-gateway, compliance-proxy,
// agent) route their persisted copies through these helpers so the
// audit store can never retain content the operator's storage policy
// forbids. The shared invariant: when a redaction cannot be applied
// precisely, drop the content rather than persist it.
//
// # Re-detection seam
//
// Hook-time TransformSpans address content on the hook-time normalized
// projection. The storage-time normalized payload can index the same
// content differently (cross-format requests project system/tool
// segments at different addresses), so a span can fail to resolve even
// though its content is present in the stored copy. Rather than
// degrading immediately, ApplyStorageAction accepts an optional
// Redetector — a function that re-locates rule-attributed sensitive
// content within one text block. The hook pipeline builds it from the
// matched hooks' compiled patterns and stamps it on the pipeline result
// (the only place the patterns are reachable; the audit writers receive
// only data). This package never imports the hooks packages — it sees
// only the function value. A nil Redetector, or a re-detection that
// cannot re-locate every failed rule's content, degrades to the drop
// placeholder with a structured diagnosis (NormalizedPayload
// RedactedReason / RedactedDetail).
package redact

import (
	"encoding/json"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// marshalJSON is an injection seam for asserting the fail-safe contract:
// every marshal failure in this package must yield nil (store nothing),
// never the original bytes.
var marshalJSON = json.Marshal

// Storage-redaction outcome label values reported through
// OnStorageOutcome. "rescued" = one or more spans failed to resolve but
// storage-time re-detection redacted the content in place; "degraded" =
// the redaction could not be applied safely and the stored copy was
// replaced with the drop placeholder.
const (
	StorageOutcomeRescued  = "rescued"
	StorageOutcomeDegraded = "degraded"
)

// OnStorageOutcome, when non-nil, observes every redetect rescue and
// every degradation produced by ApplyStorageAction, with the degradation
// cause (a rescue carries the cause it recovered from). It is a callback
// seam rather than a direct Prometheus counter so this package — linked
// into all three data-plane services' audit writers — stays free of the
// metrics dependency; pipeline.RegisterMetrics (reached at service boot
// via pipeline.RegisterDefaultMetrics in each service's wiring) wires it
// to the nexus_redact_storage_outcome_total counter. Outcomes are only
// decidable inside ApplyStorageAction, so emitting at the call sites
// would mean re-deriving them from the returned bytes.
var OnStorageOutcome func(outcome, cause string)

func reportStorageOutcome(outcome, cause string) {
	if f := OnStorageOutcome; f != nil {
		f(outcome, cause)
	}
}

// failedAddresses returns the de-duplicated content addresses of the
// skipped spans, preserving first-seen order. Addresses only — never the
// content they point at.
func failedAddresses(skipped []normcore.TransformSpan) []string {
	seen := make(map[string]struct{}, len(skipped))
	out := make([]string, 0, len(skipped))
	for _, s := range skipped {
		if s.ContentAddress == "" {
			continue
		}
		if _, ok := seen[s.ContentAddress]; ok {
			continue
		}
		seen[s.ContentAddress] = struct{}{}
		out = append(out, s.ContentAddress)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// degradedPlaceholder builds the drop placeholder for a redact policy
// that could not be applied precisely, stamped with the degradation
// diagnosis (cause + failed addresses). Every degradation flows through
// here, so this is also the single emission point for the "degraded"
// storage outcome.
func degradedPlaceholder(raw json.RawMessage, ruleIDs []string, cause string, addresses []string) json.RawMessage {
	reportStorageOutcome(StorageOutcomeDegraded, cause)
	return placeholderPayload(raw, ruleIDs, normcore.RedactedReasonDegraded, &normcore.RedactionDegradeDetail{
		Cause:           cause,
		FailedAddresses: addresses,
	})
}

// placeholderPayload replaces a NormalizedPayload with the
// {redacted:true, redactedReason, kind, ruleIds} marker. On any failure it
// returns nil (SQL NULL) rather than the original bytes — the caller
// reached here because the storage policy forbids persisting the content,
// so the fail-safe is to store nothing.
func placeholderPayload(raw json.RawMessage, ruleIDs []string, reason string, detail *normcore.RedactionDegradeDetail) json.RawMessage {
	var payload normcore.NormalizedPayload
	_ = json.Unmarshal(raw, &payload)
	placeholder := normcore.NormalizedPayload{
		Kind:             payload.Kind,
		NormalizeVersion: payload.NormalizeVersion,
		Protocol:         payload.Protocol,
		Redacted:         true,
		RedactedReason:   reason,
		RedactedDetail:   detail,
		RuleIDs:          ruleIDs,
	}
	if placeholder.NormalizeVersion == "" {
		placeholder.NormalizeVersion = normcore.SchemaVersion
	}
	if placeholder.Kind == "" {
		placeholder.Kind = normcore.KindAIChat
	}
	b, err := marshalJSON(placeholder)
	if err != nil {
		return nil
	}
	return b
}

// StorageRawBody selects the RAW wire bytes allowed onto the persisted
// payload store (traffic_event_payload, agent SQLite payload columns)
// under the operator's storage policy. "keep" (and unset) persists the
// captured bytes as-is. "redact" persists ONLY the redacted wire copy —
// when the producer has none (no inflight rewrite, or reverse-encode
// unsupported) the raw copy is dropped: the redacted normalized payload
// + spans still carry the content, and an unredactable raw copy would
// make the audit store the leak. "drop-content" and any unrecognized
// action never persist raw bytes.
//
// A nil captured copy always yields nil regardless of action: capture
// was disabled (or the request had no body), and a storage policy must
// never resurrect bytes the capture config chose not to store.
func StorageRawBody(captured, redacted []byte, action string) []byte {
	if len(captured) == 0 {
		return nil
	}
	switch action {
	case "", "keep":
		return captured
	case "redact":
		return redacted
	}
	return nil
}

// MarshalSpans serializes post-redact spans for a wire envelope or DB
// column. Empty/nil yields nil so omitempty JSON tags drop the field and
// the store keeps SQL NULL — unredacted rows pay no wire or storage cost.
func MarshalSpans(spans []normcore.TransformSpan) json.RawMessage {
	if len(spans) == 0 {
		return nil
	}
	b, err := marshalJSON(spans)
	if err != nil {
		return nil
	}
	return b
}

// CollectRuleIDs extracts the de-duplicated rule IDs (TransformSpan.SourceID)
// that triggered redaction, preserving first-seen order. Used to populate
// the drop-content placeholder's ruleIds attribution. Spans without a
// SourceID are skipped.
func CollectRuleIDs(spans []normcore.TransformSpan) []string {
	if len(spans) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(spans))
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		if s.SourceID == "" {
			continue
		}
		if _, ok := seen[s.SourceID]; ok {
			continue
		}
		seen[s.SourceID] = struct{}{}
		out = append(out, s.SourceID)
	}
	return out
}
