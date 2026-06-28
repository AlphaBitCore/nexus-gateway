// Package redact applies a compliance hook's match action to the RAW
// captured wire body that an audit producer is about to persist. All
// three data-plane services (ai-gateway, compliance-proxy, agent) route
// their persisted raw copy through StorageRawBody so the audit store can
// never retain content the operator's match action forbids:
//
//   - approve       → persist the captured bytes as-is
//   - redact / block → persist ONLY the redacted wire copy; when the
//     producer has none (no inflight rewrite, or reverse-encode
//     unsupported) the raw copy is dropped rather than stored unredacted
//   - otherwise      → store nothing
//
// The shared invariant: when a redaction cannot be applied, drop the
// content rather than persist it. The package also exposes MarshalSpans /
// CollectRuleIDs helpers for serializing post-redact span metadata.
package redact

import (
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// marshalJSON is an injection seam for asserting the fail-safe contract:
// every marshal failure in this package must yield nil (store nothing),
// never the original bytes.
var marshalJSON = json.Marshal

// StorageRawBody selects the RAW wire bytes allowed onto the persisted
// payload store (traffic_event_payload, agent SQLite payload columns)
// under the hook's match action. approve persists the captured bytes
// as-is. redact and block persist ONLY the redacted wire copy — when the
// producer has none (no inflight rewrite, or reverse-encode unsupported)
// the raw copy is dropped: an unredactable raw copy would make the audit
// store the leak. Any other action persists nothing.
//
// A nil captured copy always yields nil regardless of action: capture was
// disabled (or the request had no body), and a storage policy must never
// resurrect bytes the capture config chose not to store.
func StorageRawBody(captured, redacted []byte, a decision.Action) []byte {
	if len(captured) == 0 {
		return nil
	}
	switch a {
	case decision.ActionApprove:
		return captured
	case decision.ActionRedact, decision.ActionBlock:
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
// that triggered redaction, preserving first-seen order. Spans without a
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
