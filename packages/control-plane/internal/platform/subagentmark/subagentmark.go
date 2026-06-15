// Package subagentmark carries an in-process correlation marker naming the
// sub-agent on whose behalf an admin self-call is being made, so the
// audit row can attribute a child agent's tool call to that child rather than to
// the bare assistant channel ("via assistant ▸ subagent 2", T4).
//
// Like internal/platform/initiator it is a request-context value with no HTTP
// representation: only the in-process dispatch path sets it (the chat dispatch
// tool wraps each child's run context before agent.RunSubagent), and it rides
// in-process through the self-dispatch transport, which clones the request
// context. Unlike initiator it is NOT security-load-bearing — it never gates
// authentication; it only enriches the audit via column — but the unexported-key
// pattern keeps it unforgeable from the wire for free.
//
// It lives in its own leaf package, depending on nothing, so the audit writer
// (which composes it into AdminAuditLog.via) can read it without an import cycle.
package subagentmark

import "context"

// ctxKey types the context value carrying the sub-agent label. Unexported so only
// With can set it — no outside package (or wire client) can write under it.
type ctxKey struct{}

// With returns a context recording label as the sub-agent on whose behalf the
// request is being dispatched (e.g. "subagent 2"). Set ONLY by the chat dispatch
// tool around a child run; it rides in-process and cannot be forged from the wire.
func With(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, ctxKey{}, label)
}

// From returns the sub-agent label recorded by With, or "" for an ordinary call
// (no sub-agent in the dispatch chain — a parent's own tool call).
func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
