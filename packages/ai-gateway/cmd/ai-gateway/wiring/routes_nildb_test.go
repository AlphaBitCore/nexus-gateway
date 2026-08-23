package wiring

import "testing"

// A nil *store.DB assigned to an interface parameter produces a NON-nil
// interface holding a nil pointer. The read handlers all open with a
// `db == nil` / `models == nil` guard that answers 500 "database not
// available"; boxed that way, the guard never fires and the first method call
// panics, so a deployment brought up without a database takes the process down
// instead of answering. modelcatalog.go's selectModelCatalog was written for
// exactly this and says so — the usage and public-catalog routes handed
// deps.DB straight in and did not get the same treatment.
//
// The assertion is the property that was violated: the selector must return an
// interface that IS nil, not one merely holding nil. Reverting a selector to an
// unconditional `return deps.DB` turns this red, which a test of the handlers'
// own guards would not — those guards are correct, and passing them an explicit
// nil interface passes either way.
func TestRouteDepsSelectors_ReturnATrulyNilInterfaceWithoutADatabase(t *testing.T) {
	empty := RouteDeps{}

	if got := selectUsageStore(empty); got != nil {
		t.Errorf("selectUsageStore returned a non-nil interface (%T) with no database — the handler's db == nil guard is defeated and the route panics instead of answering", got)
	}
	// selectModelCatalog backs the public catalog too, so both are asserted
	// here and stay one rule rather than one rule and a copy that can drift.
	if got := selectModelCatalog(empty); got != nil {
		t.Errorf("selectModelCatalog returned a non-nil interface (%T) with no database or cache layer", got)
	}
}
