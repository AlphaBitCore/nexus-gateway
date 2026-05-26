package siem

import (
	"regexp"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// TestSIEMClassifyAdminEventCanonicalShape is the end-to-end alignment
// test promised by SDD e43-s1 AC-3. For every (resource, verb) pair in
// iam.Catalog, it feeds the canonical EntityType + Action into
// ClassifyAdminEvent and asserts the resulting eventType:
//
//  1. matches the canonical kebab-case "<resource>.<verb>" regex; and
//  2. equals iam.SIEMEventType(resource.Name, verb) — i.e. the catalog
//     helper used by the IAM engine and the SIEM bridge agree on the
//     same string.
//
// This test would catch any future drift between audit.EntryFor's
// (EntityType, Action) assignments and the SIEM bridge's derivation
// rule (currently EntityType + "." + Action). The same test is the
// runtime guarantee that operators configuring SIEM filters can use
// the same string that appears in IAM policies.
func TestSIEMClassifyAdminEventCanonicalShape(t *testing.T) {
	canonicalRe := regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z-]*$`)

	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		for _, v := range r.Verbs {
			evt := Event{
				"entityType": r.Name,
				"action":     string(v),
			}
			got := ClassifyAdminEvent(evt)
			if got == "" {
				t.Errorf("%s/%s: ClassifyAdminEvent returned empty string", r.Name, v)
				continue
			}
			if !canonicalRe.MatchString(got) {
				t.Errorf("%s/%s: eventType %q is not canonical kebab-case shape", r.Name, v, got)
			}
			if want := iam.SIEMEventType(r.Name, v); got != want {
				t.Errorf("%s/%s: ClassifyAdminEvent = %q, iam.SIEMEventType = %q (must match)", r.Name, v, got, want)
			}
		}
	}
}

// TestClassifyAdminEvent_RejectsLegacyCamelCase guards against
// regressing to the legacy EntityType vocabulary. The bridge silently
// returns whatever entityType + "." + action it gets, so the test
// asserts that any output it produces for a legacy CamelCase EntityType
// fails our canonical regex — making the inconsistency loud at test time
// even though the bridge itself can't reject it.
func TestClassifyAdminEvent_RejectsLegacyCamelCase(t *testing.T) {
	canonicalRe := regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z-]*$`)
	legacy := []map[string]string{
		{"entityType": "virtualKey", "action": "create"},           // legacy camelCase
		{"entityType": "virtual_key", "action": "delete"},          // legacy snake_case
		{"entityType": "complianceKillswitch", "action": "toggle"}, // legacy camelCase
		{"entityType": "alertRule", "action": "update"},            // legacy sub-entity (collapsed to alert)
	}
	for _, l := range legacy {
		evt := Event{"entityType": l["entityType"], "action": l["action"]}
		got := ClassifyAdminEvent(evt)
		if canonicalRe.MatchString(got) {
			t.Errorf("legacy input %v produced canonical-shaped output %q — should fail regex",
				l, got)
		}
	}
}
