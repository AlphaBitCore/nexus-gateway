package quota

import "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"

// CheckLevel identifies a single level in the hierarchical quota check chain.
//
// TargetType must match the canonical dimension name used by both
// metric_rollup_1h.dimensionKey ("virtual_key=..." / "user=..." /
// "project=..." / "organization=...") and the UsageCache.Backfill seed
// path. Drift between runtime IncrMulti and Backfill writes the same
// usage to two disjoint Redis keys, and Backfill can no longer correct
// the runtime-accumulated value.
type CheckLevel struct {
	TargetType string // "virtual_key" | "user" | "project" | "organization"
	TargetID   string

	// Populated by Engine.Check after policy + override resolve.
	// Zero values when no policy/override matched (HasLimit = false).
	HasLimit     bool
	CurrentCents int64
	LimitCents   int64
	PeriodKey    string
	// EnforcementMode is the resolved enforcement mode for this level
	// ("reject" | "downgrade" | "notify-and-proceed"); empty when no
	// enforcing policy/override matched (track-only and no-limit levels are
	// never stamped). Lets post-settlement evaluators — the realtime relay's
	// per-response over-limit sever — act on reject-mode levels without a
	// second policy resolve or Redis read.
	EnforcementMode string
}

// BuildCheckChain returns check levels including org hierarchy walk-up.
// orgParents maps orgID -> parentOrgID (empty string for root nodes).
//
// Personal VK chain:  VK -> User -> Org (walk up tree)
// Application VK chain: VK -> Project -> Org (walk up tree)
func BuildCheckChain(meta *vkauth.VKMeta, orgParents map[string]string) []CheckLevel {
	var chain []CheckLevel

	// VK level. TargetType "virtual_key" matches metric_rollup_1h's
	// dimensionKey prefix so UsageCache.Backfill seeds the same Redis key
	// the runtime IncrMulti writes to.
	chain = append(chain, CheckLevel{TargetType: "virtual_key", TargetID: meta.ID})

	if meta.VKType == "personal" || meta.VKType == "" {
		if meta.OwnerID != "" {
			chain = append(chain, CheckLevel{TargetType: "user", TargetID: meta.OwnerID})
		}
	} else { // application
		if meta.ProjectID != "" {
			chain = append(chain, CheckLevel{TargetType: "project", TargetID: meta.ProjectID})
		}
	}

	// Walk up org tree from user's org to root.
	orgID := meta.OrganizationID
	visited := map[string]bool{} // prevent infinite loops
	for orgID != "" && !visited[orgID] {
		visited[orgID] = true
		chain = append(chain, CheckLevel{TargetType: "organization", TargetID: orgID})
		orgID = orgParents[orgID]
	}

	return chain
}
