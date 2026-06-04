package audit

import (
	"context"
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// InitiatedByHeader is the legacy HTTP request header that once marked the channel
// which initiated an admin request. As of the P2b in-process self-call (#16) the
// channel is an UNFORGEABLE in-process context signal (see WithInitiator), NOT a
// wire header — so EntryFor no longer reads this header, and StripInitiatorHeader
// deletes any inbound copy at ingress. The constant is retained only so the strip
// middleware names the header it scrubs.
//
// Deliberately NOT "X-Nexus-Via": that name is already owned by the data-plane
// service-hop chain marker (packages/shared/traffic/markers.go HeaderVia), a
// comma-separated list of service names on RESPONSE headers — a different grammar
// on a different plane. A distinct name keeps the two from ever colliding.
const InitiatedByHeader = "X-Nexus-Initiated-By"

// ViaAssistant is the channel value the web assistant's in-process self-call
// transport stamps (via WithInitiator) on the admin calls it performs on the user's
// behalf, and the AdminAuditLog.via column value, so AI-initiated writes are
// distinguishable from human ones in the tamper-evident audit chain (E90 I5).
const ViaAssistant = "assistant"

// initiatorCtxKey types the request-context value that carries the initiating
// channel. Unexported so only this package can set it (via WithInitiator).
type initiatorCtxKey struct{}

// WithInitiator returns a context that records via as the channel that initiated the
// request. It is set ONLY by the web assistant's in-process self-call transport
// (packages/control-plane/internal/assistant): the transport dispatches the request
// straight into the CP router with this context, so the value rides in-process and
// can never be forged from the wire — a Go context value has no HTTP representation,
// so an authenticated admin hitting the public ingress cannot set it (closing the
// #18b H1 forgery residual). EntryFor reads it back via InitiatorFromContext.
func WithInitiator(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, initiatorCtxKey{}, via)
}

// InitiatorFromContext returns the initiating channel recorded by WithInitiator, or
// "" for an ordinary human/UI request (no in-process initiator stamp).
func InitiatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(initiatorCtxKey{}).(string)
	return v
}

// StripInitiatorHeader is a middleware that deletes any inbound InitiatedByHeader on
// every request before handlers run. The channel marker is now an in-process context
// signal (WithInitiator), never a trusted wire header, so a copy arriving from a
// client is a forgery attempt — dropped here so it can never reach EntryFor or be
// echoed downstream. Apply globally at the ingress edge.
func StripInitiatorHeader(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Request().Header.Del(InitiatedByHeader)
		return next(c)
	}
}

// EntryFor builds a partial audit Entry pre-populated with actor + request
// context extracted from c, plus EntityType set to resource.Name and Action
// set to string(verb). The caller fills EntityID, BeforeState, AfterState
// as the handler progresses, then passes the entry to Writer.LogObserved
// (or Writer.Log) when the upstream mutation has committed.
//
// EntryFor is the only allowed constructor for an audit Entry that carries
// EntityType + Action — free-form ae.Action = "..." / ae.EntityType = "..."
// assignment in handler code is blocked by a forbidigo lint rule, and the
// consistency test asserts every emitted entry's (EntityType, Action) pair
// resolves to a real (resource, verb) pair in iam.Catalog.
//
// Panics if verb is not declared on resource — a programmer error caught
// at server startup, not at request time. Verbs are a closed enum per
// resource (packages/shared/identity/iam/catalog.go); adding a new operation must
// add the verb to the catalog before this constructor accepts it.
func EntryFor(c echo.Context, resource *iam.ResourceDef, verb iam.Verb) Entry {
	if !resource.Allows(verb) {
		panic(fmt.Sprintf("audit.EntryFor: verb %q not declared on resource %q (catalog verbs: %v)",
			verb, resource.Name, resource.Verbs))
	}
	e := Entry{
		EntityType:     resource.Name,
		Action:         string(verb),
		SourceIP:       c.RealIP(),
		NexusRequestID: middleware.NexusRequestIDFromContext(c),
		Via:            InitiatorFromContext(c.Request().Context()),
	}
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		e.ActorID = aa.KeyID
		e.ActorLabel = aa.KeyName
	}
	return e
}
