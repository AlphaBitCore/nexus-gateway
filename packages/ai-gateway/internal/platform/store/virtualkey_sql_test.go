package store

import (
	"strings"
	"testing"
)

// TestVKSelectSQL_CoalescesPersonalAndApplicationOrgChains pins the
// fix to vkSelectSQL: org_id / org_name / org_timezone must resolve via
// either the application chain (Project → Organization) OR the personal
// chain (NexusUser → Organization), preferring the application chain
// when present.
//
// Pre-fix the SQL only joined the application chain — personal VKs
// always reported empty org_id, breaking the audit pipeline's
// org_id/org_name columns on traffic_event for personal-VK traffic.
// Surfaced when a prod-deploy probe used a personal VK (it was the first
// personal-VK AI traffic ever in prod) and the resulting traffic_event row
// showed empty Organization in the admin UI.
//
// We pin three properties via string match because a real round-trip
// test would need a live Postgres (out of scope for a unit test;
// prod re-probe is the authoritative end-to-end verification).
func TestVKSelectSQL_CoalescesPersonalAndApplicationOrgChains(t *testing.T) {
	required := []struct {
		name    string
		needle  string
		comment string
	}{
		{
			"COALESCE org id",
			`COALESCE(p."organizationId", u."organizationId")`,
			"app-VK-via-Project preferred; personal-VK falls back to NexusUser.organizationId",
		},
		{
			"COALESCE org name",
			`COALESCE(org.name, u_org.name)`,
			"same precedence on the joined Organization.name",
		},
		{
			"COALESCE org timezone",
			`COALESCE(org.timezone, u_org.timezone)`,
			"same precedence on the joined Organization.timezone",
		},
		{
			"second Organization JOIN via owner",
			`LEFT JOIN "Organization" u_org ON u."organizationId" = u_org.id`,
			"the new join chain that resolves personal-VK org via the owner user",
		},
		{
			"first Organization JOIN via project (preserved)",
			`LEFT JOIN "Organization" org   ON p."organizationId" = org.id`,
			"original application-VK chain — must remain so existing behaviour is preserved",
		},
	}
	for _, c := range required {
		if !strings.Contains(vkSelectSQL, c.needle) {
			t.Errorf("vkSelectSQL missing %q (%s) — %s\n\nfull SQL:\n%s",
				c.name, c.comment, c.needle, vkSelectSQL)
		}
	}
}

// TestVKSelectSQL_NoLegacyOrgOnlyJoin pins that the legacy
// "org via project ONLY" form is gone — catches an accidental revert
// that strips the COALESCE and breaks personal-VK org again.
func TestVKSelectSQL_NoLegacyOrgOnlyJoin(t *testing.T) {
	// The pre-fix line `vk."projectId", p."organizationId",` (with no
	// COALESCE) was the bug. Assert the legacy form is gone.
	legacyForm := "vk.\"projectId\", p.\"organizationId\",\n"
	if strings.Contains(vkSelectSQL, legacyForm) {
		t.Errorf("vkSelectSQL contains the pre-fix legacy `vk.\"projectId\", p.\"organizationId\",` form — personal VK org would be NULL again.\n\nfull SQL:\n%s", vkSelectSQL)
	}
}
