package audit

import (
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

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
	}
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		e.ActorID = aa.KeyID
		e.ActorLabel = aa.KeyName
	}
	return e
}
