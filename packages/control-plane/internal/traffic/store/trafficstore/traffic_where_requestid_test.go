package trafficstore

import (
	"strings"
	"testing"
)

// The value an operator pastes into this filter is the one the gateway handed
// back in the X-Nexus-Request-Id response header, which is persisted as
// trace_id. It must NOT match traffic_event.id: that column is a minted
// per-row key the caller never sees, so matching on it returns nothing for
// every search an operator can actually perform — silently, as an empty
// result rather than an error.
//
// This asserts the predicate itself. The suite's broader filter tests stop
// their pgxmock regex at "FROM traffic_event a WHERE", and pgxmock never
// executes SQL, so swapping the column underneath them leaves them green.
func TestBuildTrafficEventWhere_RequestIDMatchesTraceIDNotPrimaryKey(t *testing.T) {
	where, args, _ := buildTrafficEventWhere(TrafficEventListParams{RequestID: "rid-from-the-response-header"})

	if !strings.Contains(where, "a.trace_id = $") {
		t.Errorf("requestId filter must match trace_id; predicate was:\n%s", where)
	}
	if strings.Contains(where, "a.id = $") {
		t.Errorf("requestId filter still matches the primary key; predicate was:\n%s", where)
	}
	var bound int
	for _, a := range args {
		if a == "rid-from-the-response-header" {
			bound++
		}
	}
	if bound != 1 {
		t.Errorf("filter value bound %d times, want exactly 1; args = %v", bound, args)
	}
}
