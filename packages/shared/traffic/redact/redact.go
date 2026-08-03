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
//   - unrecognised  → store nothing
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
// store the leak. An unrecognised action persists nothing.
//
// A nil captured copy always yields nil regardless of action: capture was
// disabled (or the request had no body), and a storage policy must never
// resurrect bytes the capture config chose not to store.
//
// Prefer StorageRawBodyChecked in a producer that can log: this form cannot
// distinguish "there was nothing to store" from "the action was not
// understood", and the second is a defect worth a line in the log.
func StorageRawBody(captured, redacted []byte, a decision.Action) []byte {
	body, _ := StorageRawBodyChecked(captured, redacted, a)
	return body
}

// StorageRawBodyChecked is StorageRawBody with the verdict the plain form
// throws away: ok is false ONLY when the action itself was not understood.
//
// # The empty action means approve, and that rule lives HERE
//
// decision.Action is a string, so a result built as
// `&CompliancePipelineResult{Decision: Approve}` — decision set, action not —
// carries Action == "". Matching only approve/redact/block would drop that
// body: the row persists with a NULL body, and nothing counts or logs it. That
// is the actual mechanism by which the compliance proxy came to record a
// request body on every bumped row and a response body on none.
//
// An empty action carries no redaction demand — nothing asked for the content
// to be withheld — so it stores the captured bytes. The rule is stated once,
// in the one function all three services route through, because the previous
// arrangement is what this program's S-14 is about: the gateway had learned the
// trap and guarded it at each of its own call sites while the shared emitter
// that the proxy and the agent depend on never got the same guard. The
// knowledge lived in one copy, and the copy that had it was not the shared one.
//
// ok=false is reserved for a non-empty action that is none of the three. That
// is a producer bug rather than a policy, and it still stores nothing (a body
// must never be persisted under a policy nobody can name) — but the caller now
// has something to log instead of a silent NULL.
func StorageRawBodyChecked(captured, redacted []byte, a decision.Action) (body []byte, ok bool) {
	if a == "" {
		a = decision.ActionApprove
	}
	if len(captured) == 0 {
		// Nothing captured: not a policy failure, so ok stays true. An
		// unrecognised action still reports false, because the caller asked
		// with an action nobody can act on either way.
		return nil, a == decision.ActionApprove || a == decision.ActionRedact || a == decision.ActionBlock
	}
	switch a {
	case decision.ActionApprove:
		return captured, true
	case decision.ActionRedact, decision.ActionBlock:
		return redacted, true
	}
	return nil, false
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
