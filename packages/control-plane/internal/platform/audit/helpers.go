package audit

import (
	"context"
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/subagentmark"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// InitiatedByHeader names the HTTP request header that is scrubbed at ingress.
// The initiating channel is an UNFORGEABLE in-process context signal
// (initiator.With), NOT a wire header — EntryFor never reads this header, and
// StripInitiatorHeader deletes any inbound copy at ingress so a client-supplied
// value can never masquerade as a privileged channel. The constant exists only
// so the strip middleware names the header it scrubs.
//
// Deliberately NOT "X-Nexus-Via": that name is already owned by the data-plane
// service-hop chain marker (packages/shared/traffic/markers.go HeaderVia), a
// comma-separated list of service names on RESPONSE headers — a different grammar
// on a different plane. A distinct name keeps the two from ever colliding.
const InitiatedByHeader = "X-Nexus-Initiated-By"

// StripInitiatorHeader is a middleware that deletes any inbound InitiatedByHeader on
// every request before handlers run. The channel marker is now an in-process context
// signal (initiator.With), never a trusted wire header, so a copy arriving from a
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
		Via:            viaFor(c.Request().Context()),
	}
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		e.ActorID = aa.KeyID
		e.ActorLabel = aa.KeyName
	}
	return e
}

// viaFor composes the audit via channel. It is the initiator channel
// (initiator.From: "assistant" / "workflow" / "") optionally suffixed with the
// sub-agent marker when the call was made on a child agent's behalf — yielding
// "assistant ▸ subagent 2". The suffix only appears for in-process
// sub-agent tool calls; an ordinary call (empty marker) returns the bare channel
// unchanged, so existing rows and human/UI writes are byte-identical to before.
func viaFor(ctx context.Context) string {
	via := initiator.From(ctx)
	if label := subagentmark.From(ctx); label != "" {
		if via == "" {
			return label
		}
		return via + " ▸ " + label
	}
	return via
}
