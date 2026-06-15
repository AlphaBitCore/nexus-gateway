// Package initiator carries the unforgeable in-process channel marker that
// records which internal subsystem initiated an admin request on a user's
// behalf. The marker is a request-context value with no HTTP representation, so
// it cannot be forged from the wire: only the in-process self-dispatch transport
// (internal/platform/selfdispatch) sets it, and the reader — the audit writer
// (the AdminAuditLog.via column) — trusts it precisely because a network client
// cannot produce a Go context value.
//
// It lives in its own leaf package, depending on nothing, so platform packages
// at any layer can read the marker without an import cycle.
package initiator

import "context"

// Channel values. Each names an in-process subsystem that dispatches admin calls
// on a user's behalf; the empty string is an ordinary human/UI request that
// arrived over the wire with no in-process initiator.
const (
	// ViaAssistant marks the admin calls the web assistant's in-process
	// self-call transport performs on the user's behalf. It is the
	// AdminAuditLog.via value that distinguishes AI-initiated writes from human
	// ones in the tamper-evident audit chain.
	ViaAssistant = "assistant"
)

// ctxKey types the context value carrying the initiating channel. Unexported so
// only With can set it — the value is unforgeable because there is no exported
// key for an outside package (or a wire client) to write under.
type ctxKey struct{}

// With returns a context recording via as the channel that initiated the
// request. It is set ONLY by internal/platform/selfdispatch, which dispatches the
// request straight into the CP router carrying this context — the value rides
// in-process and can never be forged from the wire (a Go context value has no
// HTTP form, so an authenticated admin hitting the public ingress cannot set it).
func With(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, ctxKey{}, via)
}

// From returns the initiating channel recorded by With, or "" for an ordinary
// human/UI request (no in-process initiator stamp).
func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
