package audit

import (
	"encoding/json"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// applyStorageAction transforms the marshalled NormalizedPayload bytes
// before they land on the wire to traffic_event_normalized, per the
// operator's onMatch.storageAction:
//
//   - keep / "" / nil raw   → no change
//   - redact                → ApplySpans(payload, spans) → re-marshal
//   - drop-content          → replace payload with the redacted-placeholder
//     {redacted:true, kind, ruleIds}
//
// Failures fall back to leaving the original bytes intact so the audit
// row still carries the normalized snapshot; the storage policy is
// observability, not a runtime gate.
func applyStorageAction(raw json.RawMessage, action string, spans []normcore.TransformSpan, ruleIDs []string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	switch action {
	case "", "keep":
		return raw
	case "redact":
		if len(spans) == 0 {
			return raw
		}
		var payload normcore.NormalizedPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return raw
		}
		patched, _ := normcore.ApplySpans(payload, spans)
		b, err := json.Marshal(patched)
		if err != nil {
			return raw
		}
		return b
	case "drop-content":
		var payload normcore.NormalizedPayload
		_ = json.Unmarshal(raw, &payload)
		placeholder := normcore.NormalizedPayload{
			Kind:             payload.Kind,
			NormalizeVersion: payload.NormalizeVersion,
			Protocol:         payload.Protocol,
			Redacted:         true,
			RuleIDs:          ruleIDs,
		}
		if placeholder.NormalizeVersion == "" {
			placeholder.NormalizeVersion = normcore.SchemaVersion
		}
		if placeholder.Kind == "" {
			placeholder.Kind = normcore.KindAIChat
		}
		b, err := json.Marshal(placeholder)
		if err != nil {
			return raw
		}
		return b
	}
	return raw
}
