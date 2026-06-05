package resource

import (
	"reflect"
	"strings"
	"testing"
)

// capability_test.go pins the total display mapping of design doc §3.3 against
// the REAL embedded catalog — including the exact example table the design
// publishes (a wrong example misleads implementation; adversarial review R2-1
// caught precisely that in an earlier draft).

func capsFor(t *testing.T, kind string) []string {
	t.Helper()
	rk, ok := resIdx[kind]
	if !ok {
		t.Fatalf("kind %q not in catalog", kind)
	}
	return rk.capabilities()
}

// TestCapabilitiesDesignDocExamples re-derives the §3.3 example table.
func TestCapabilitiesDesignDocExamples(t *testing.T) {
	cases := map[string][]string{
		// GET+PUT /ai-guard/config pair + POST /ai-guard/dry-run RPC
		"ai-guard": {"config", "action:dry-run"},
		// 14 GET endpoints, none CRUD-shaped
		"analytics": {"report"},
		// full CRUD (incl. PATCH dup that used to render "update update") + simulate RPC
		"routing-rules": {"crud", "action:simulate"},
		// root GET+PUT pair absorbs `list`; sub-pairs config; event-types GET report; siem/test RPC
		"settings": {"config", "report", "action:test"},
	}
	for kind, want := range cases {
		if got := capsFor(t, kind); !reflect.DeepEqual(got, want) {
			t.Errorf("%s capabilities = %v, want %v", kind, got, want)
		}
	}
}

// TestCapabilitiesNeverEmpty is the totality invariant: rules 1-4 partition
// every operation, so no kind in the catalog may render an empty profile —
// the "silent kinds" bug this design exists to kill.
func TestCapabilitiesNeverEmpty(t *testing.T) {
	for _, k := range resCatalog.Kinds {
		if caps := k.capabilities(); len(caps) == 0 {
			t.Errorf("kind %s has an empty capability profile (%d ops)", k.Kind, len(k.Operations))
		}
	}
}

// TestCapabilitiesDeduplicated kills the `update update` bug at its root: no
// verb may appear twice in a profile.
func TestCapabilitiesDeduplicated(t *testing.T) {
	for _, k := range resCatalog.Kinds {
		seen := map[string]bool{}
		for _, v := range k.capabilities() {
			if seen[v] {
				t.Errorf("kind %s repeats capability %q", k.Kind, v)
			}
			seen[v] = true
		}
	}
}

// TestCapabilitiesRootPairAbsorbsList: a kind whose collection root is a
// GET+PUT (or GET+PATCH) pair is a singleton-config surface — it must say
// `config`, and must NOT claim the canonical `list` its root GET would
// otherwise produce. `me` is the GET+PATCH variant.
func TestCapabilitiesRootPairAbsorbsList(t *testing.T) {
	for _, kind := range []string{"settings", "me"} {
		caps := capsFor(t, kind)
		joined := strings.Join(caps, " ")
		if !strings.Contains(joined, "config") {
			t.Errorf("%s must include config, got %v", kind, caps)
		}
		for _, v := range caps {
			if v == "list" {
				t.Errorf("%s root pair must absorb the canonical list, got %v", kind, caps)
			}
		}
	}
}

// TestCapabilitiesPartialCrudStaysVerbByVerb: only the FULL canonical set
// collapses to "crud"; a list+get-only kind keeps its individual verbs.
func TestCapabilitiesPartialCrudStaysVerbByVerb(t *testing.T) {
	caps := capsFor(t, "alerts") // list/get + ack/resolve actions + channel CRUD subset
	for _, v := range caps {
		if v == "crud" {
			t.Errorf("alerts is not full CRUD at the collection root; got %v", caps)
		}
	}
	caps = capsFor(t, "virtual-keys") // full CRUD + 5 item actions
	if caps[0] != "crud" {
		t.Errorf("virtual-keys must collapse to crud first, got %v", caps)
	}
}

// TestCapabilitiesActionSegNaming: rule-4 action names come from the last
// non-{param} segment — multi-segment RPC tails and param-ending paths both
// produce a readable token, never a "{...}" brace.
func TestCapabilitiesActionSegNaming(t *testing.T) {
	if got := actionSeg("/api/admin/settings/siem/test"); got != "test" {
		t.Errorf("actionSeg(siem/test) = %q", got)
	}
	if got := actionSeg("/api/admin/nodes/{id}/overrides/{configKey}"); got != "overrides" {
		t.Errorf("actionSeg(param-ending) = %q, want overrides", got)
	}
	for _, k := range resCatalog.Kinds {
		for _, v := range k.capabilities() {
			if strings.Contains(v, "{") {
				t.Errorf("kind %s capability %q leaked a path placeholder", k.Kind, v)
			}
		}
	}
}

// TestCapabilitiesReportCoversParamTails: rule 3 is "any remaining GET" —
// a GET with a {param} in its tail (things' only op is /things/{id}/stats)
// is a report, not a silent miss.
func TestCapabilitiesReportCoversParamTails(t *testing.T) {
	if got := capsFor(t, "things"); !reflect.DeepEqual(got, []string{"report"}) {
		t.Errorf("things = %v, want [report]", got)
	}
}
