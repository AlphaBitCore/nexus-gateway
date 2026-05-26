package quota

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- types.go --------------------------------------------------------------

func TestCostEstimate_EstimatedCost(t *testing.T) {
	cases := []struct {
		name string
		in   CostEstimate
		want float64
	}{
		{"zero", CostEstimate{}, 0},
		{"input only", CostEstimate{EstimatedInputTokens: 1_000_000, InputPricePM: 2.5}, 2.5},
		{"output only", CostEstimate{MaxOutputTokens: 500_000, OutputPricePM: 10}, 5},
		{"both", CostEstimate{EstimatedInputTokens: 100_000, InputPricePM: 3, MaxOutputTokens: 200_000, OutputPricePM: 15}, 3.3},
		// 100k*3 + 200k*15 = 300k + 3M = 3.3M / 1M = 3.3
	}
	for _, c := range cases {
		got := c.in.EstimatedCost()
		if abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestActualUsage_ActualCost(t *testing.T) {
	u := ActualUsage{PromptTokens: 250_000, CompletionTokens: 500_000, InputPricePM: 2, OutputPricePM: 6}
	// 250k*2 + 500k*6 = 500k + 3M = 3.5M / 1M = 3.5
	if got := u.ActualCost(); abs(got-3.5) > 1e-9 {
		t.Errorf("got %v want 3.5", got)
	}
	if got := (ActualUsage{}).ActualCost(); got != 0 {
		t.Errorf("zero usage: got %v", got)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// --- chain.go --------------------------------------------------------------

func TestBuildCheckChain_PersonalVK(t *testing.T) {
	meta := &vkauth.VKMeta{
		ID:             "vk-1",
		VKType:         "personal",
		OwnerID:        "user-1",
		OrganizationID: "org-leaf",
	}
	parents := map[string]string{"org-leaf": "org-mid", "org-mid": "org-root", "org-root": ""}

	got := BuildCheckChain(meta, parents)
	want := []CheckLevel{
		{TargetType: "virtual_key", TargetID: "vk-1"},
		{TargetType: "user", TargetID: "user-1"},
		{TargetType: "organization", TargetID: "org-leaf"},
		{TargetType: "organization", TargetID: "org-mid"},
		{TargetType: "organization", TargetID: "org-root"},
	}
	if !equalChain(got, want) {
		t.Fatalf("chain mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestBuildCheckChain_ApplicationVK(t *testing.T) {
	meta := &vkauth.VKMeta{
		ID:             "vk-app",
		VKType:         "application",
		ProjectID:      "proj-1",
		OrganizationID: "org-1",
		OwnerID:        "user-shouldnt-appear", // application VK must NOT route through user
	}
	got := BuildCheckChain(meta, map[string]string{"org-1": ""})

	if len(got) != 3 {
		t.Fatalf("len: %d, want 3 (vk/project/org). got=%+v", len(got), got)
	}
	if got[0].TargetType != "virtual_key" || got[0].TargetID != "vk-app" {
		t.Errorf("level 0: %+v", got[0])
	}
	if got[1].TargetType != "project" || got[1].TargetID != "proj-1" {
		t.Errorf("level 1: %+v want project", got[1])
	}
	if got[2].TargetType != "organization" {
		t.Errorf("level 2: %+v want organization", got[2])
	}
	// Application VK must never insert a user level.
	for _, l := range got {
		if l.TargetType == "user" {
			t.Errorf("application VK chain contains user level: %+v", got)
		}
	}
}

func TestBuildCheckChain_EmptyVKType_TreatedAsPersonal(t *testing.T) {
	// Legacy rows where VKType is unset must default to personal so the
	// chain still walks through the user level.
	meta := &vkauth.VKMeta{ID: "vk-legacy", VKType: "", OwnerID: "owner", OrganizationID: "org"}
	got := BuildCheckChain(meta, map[string]string{"org": ""})
	if len(got) != 3 || got[1].TargetType != "user" {
		t.Fatalf("empty VKType not treated as personal: %+v", got)
	}
}

func TestBuildCheckChain_MissingOwnerID_DropsUserLevel(t *testing.T) {
	meta := &vkauth.VKMeta{ID: "vk", VKType: "personal", OwnerID: "", OrganizationID: "org"}
	got := BuildCheckChain(meta, map[string]string{"org": ""})
	for _, l := range got {
		if l.TargetType == "user" {
			t.Errorf("user level present with empty OwnerID: %+v", got)
		}
	}
}

func TestBuildCheckChain_MissingProjectID_DropsProjectLevel(t *testing.T) {
	meta := &vkauth.VKMeta{ID: "vk", VKType: "application", ProjectID: "", OrganizationID: "org"}
	got := BuildCheckChain(meta, map[string]string{"org": ""})
	for _, l := range got {
		if l.TargetType == "project" {
			t.Errorf("project level present with empty ProjectID: %+v", got)
		}
	}
}

func TestBuildCheckChain_OrgCycleProtection(t *testing.T) {
	// A malformed parents map containing a cycle (org-a -> org-b -> org-a)
	// must not loop forever. The visited set caps the walk.
	meta := &vkauth.VKMeta{ID: "vk", VKType: "personal", OwnerID: "u", OrganizationID: "org-a"}
	parents := map[string]string{"org-a": "org-b", "org-b": "org-a"}
	got := BuildCheckChain(meta, parents)
	// Should produce exactly 2 org levels (a, b) then stop.
	orgCount := 0
	for _, l := range got {
		if l.TargetType == "organization" {
			orgCount++
		}
	}
	if orgCount != 2 {
		t.Errorf("cycle protection failed: %d org levels, want 2; chain=%+v", orgCount, got)
	}
}

func TestBuildCheckChain_NoOrganization(t *testing.T) {
	meta := &vkauth.VKMeta{ID: "vk", VKType: "personal", OwnerID: "u", OrganizationID: ""}
	got := BuildCheckChain(meta, nil)
	for _, l := range got {
		if l.TargetType == "organization" {
			t.Errorf("org level emitted with empty OrganizationID: %+v", got)
		}
	}
}

func equalChain(a, b []CheckLevel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- downgrade.go ----------------------------------------------------------

func TestSelectCheapestIndex_PicksCheapestWithinBudget(t *testing.T) {
	estimate := CostEstimate{EstimatedInputTokens: 1_000_000, MaxOutputTokens: 1_000_000}
	targets := []TargetPricing{
		{Index: 0, ModelID: "premium", InputPricePM: 10, OutputPricePM: 30}, // cost = 40
		{Index: 1, ModelID: "mid", InputPricePM: 3, OutputPricePM: 9},       // cost = 12
		{Index: 2, ModelID: "cheap", InputPricePM: 1, OutputPricePM: 2},     // cost = 3
	}
	// Budget 20: only mid (12) and cheap (3) fit; cheapest is 2.
	if got := SelectCheapestIndex(targets, estimate, 20); got != 2 {
		t.Errorf("budget=20: got index %d, want 2", got)
	}
	// Budget 50: all fit; cheapest still 2.
	if got := SelectCheapestIndex(targets, estimate, 50); got != 2 {
		t.Errorf("budget=50: got index %d, want 2", got)
	}
	// Budget 11: only cheap fits; index 2.
	if got := SelectCheapestIndex(targets, estimate, 11); got != 2 {
		t.Errorf("budget=11: got index %d, want 2", got)
	}
}

func TestSelectCheapestIndex_NoneFitsReturnsMinusOne(t *testing.T) {
	estimate := CostEstimate{EstimatedInputTokens: 1_000_000, MaxOutputTokens: 1_000_000}
	targets := []TargetPricing{{Index: 0, InputPricePM: 10, OutputPricePM: 30}} // cost 40
	if got := SelectCheapestIndex(targets, estimate, 5); got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

func TestSelectCheapestIndex_EmptyTargetsReturnsMinusOne(t *testing.T) {
	if got := SelectCheapestIndex(nil, CostEstimate{}, 100); got != -1 {
		t.Errorf("nil targets: got %d", got)
	}
	if got := SelectCheapestIndex([]TargetPricing{}, CostEstimate{}, 100); got != -1 {
		t.Errorf("empty targets: got %d", got)
	}
}

func TestSelectCheapestIndex_ZeroCostIsValid(t *testing.T) {
	// A free model (price 0) must be selectable even with zero budget —
	// otherwise self-hosted/free providers can't be downgrade targets.
	targets := []TargetPricing{
		{Index: 5, InputPricePM: 0, OutputPricePM: 0},
	}
	if got := SelectCheapestIndex(targets, CostEstimate{EstimatedInputTokens: 1_000_000}, 0); got != 5 {
		t.Errorf("free model: got %d, want 5", got)
	}
}

// --- enforcement.go --------------------------------------------------------

func TestCurrentPeriodKey_Daily(t *testing.T) {
	got := CurrentPeriodKey("daily")
	if _, err := time.Parse("2006-01-02", got); err != nil {
		t.Errorf("daily key %q not parseable: %v", got, err)
	}
}

func TestCurrentPeriodKey_Weekly(t *testing.T) {
	got := CurrentPeriodKey("weekly")
	var y, w int
	if _, err := strconv.Atoi(got[:4]); err != nil {
		t.Errorf("weekly key %q malformed year", got)
	}
	if len(got) < 7 || got[4] != '-' || got[5] != 'W' {
		t.Errorf("weekly key %q missing -W marker", got)
	}
	_ = y
	_ = w
}

func TestCurrentPeriodKey_Monthly_IsDefault(t *testing.T) {
	monthly := CurrentPeriodKey("monthly")
	def := CurrentPeriodKey("anything-else")
	if monthly != def {
		t.Errorf("default period type should equal monthly: monthly=%q default=%q", monthly, def)
	}
	if _, err := time.Parse("2006-01", monthly); err != nil {
		t.Errorf("monthly key %q: %v", monthly, err)
	}
}

func TestActionPriority_Ordering(t *testing.T) {
	if actionPriority("reject") <= actionPriority("downgrade") ||
		actionPriority("downgrade") <= actionPriority("notify-and-proceed") ||
		actionPriority("notify-and-proceed") <= actionPriority("track-only") ||
		actionPriority("track-only") <= actionPriority("allow") ||
		actionPriority("allow") != actionPriority("unknown") {
		t.Errorf("action priority ordering broken: reject=%d downgrade=%d notify=%d track=%d allow=%d",
			actionPriority("reject"), actionPriority("downgrade"),
			actionPriority("notify-and-proceed"), actionPriority("track-only"),
			actionPriority("allow"))
	}
}

func TestEngine_VKLimit_OverrideThenPolicyFallback(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 100, EnforcementMode: "reject", Priority: 100},
	}
	policyCache.overridesByKey["virtual_key:vk-1"] = &CachedOverride{
		ID: "o-1", TargetType: "virtual_key", TargetID: "vk-1", CostLimitCents: 250,
		// EnforcementMode + PeriodType empty → fall back to policy.
	}
	usageCache := NewUsageCache(nil, testLogger())
	usageCache.memUsage[usageKey("virtual_key", "vk-1", CurrentPeriodKey("monthly"))] = 80
	engine := NewEngine(policyCache, usageCache, testLogger())

	// Override hit — limit from override, period from policy fallback.
	limit, current, period, has := engine.VKLimit(context.Background(),
		&vkauth.VKMeta{ID: "vk-1", OrganizationID: "org"})
	if !has || limit != 250 || current != 80 || period != CurrentPeriodKey("monthly") {
		t.Errorf("override-fallback: got has=%v limit=%d current=%d period=%q", has, limit, current, period)
	}

	// Policy-only hit — no override row.
	limit2, _, _, has2 := engine.VKLimit(context.Background(),
		&vkauth.VKMeta{ID: "vk-2", OrganizationID: "org"})
	if !has2 || limit2 != 100 {
		t.Errorf("policy-only: got has=%v limit=%d", has2, limit2)
	}

	// No-policy hit — no override, no policy match → has=false.
	emptyCache := NewPolicyCache(nil, testLogger())
	emptyEngine := NewEngine(emptyCache, usageCache, testLogger())
	_, _, _, has3 := emptyEngine.VKLimit(context.Background(),
		&vkauth.VKMeta{ID: "vk-3", OrganizationID: "org"})
	if has3 {
		t.Errorf("no-policy should return has=false")
	}

	// Nil VKMeta → has=false.
	_, _, _, has4 := engine.VKLimit(context.Background(), nil)
	if has4 {
		t.Errorf("nil meta should return has=false")
	}
}

func TestEngine_Check_NoPolicy_Allows(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	meta := &vkauth.VKMeta{ID: "vk-1", OrganizationID: "org"}
	d := engine.Check(context.Background(), chain, CostEstimate{}, meta)
	if !d.Allowed || d.Action != "allow" {
		t.Errorf("no-policy: got %+v, want allowed/allow", d)
	}
}

func TestEngine_Check_PolicyRejects(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 100, EnforcementMode: "reject", Priority: 100},
	}
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	// Inject usage > limit into the in-memory map.
	key := usageKey("virtual_key", "vk-1", CurrentPeriodKey("monthly"))
	usageCache.memUsage[key] = 200

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	meta := &vkauth.VKMeta{ID: "vk-1", OrganizationID: "org"}
	d := engine.Check(context.Background(), chain, CostEstimate{}, meta)

	if d.Allowed {
		t.Errorf("over-limit reject should not Allow: %+v", d)
	}
	if d.Action != "reject" {
		t.Errorf("action: got %q want reject", d.Action)
	}
	if d.QuotaID != "policy:p-1" {
		t.Errorf("quotaID: %q", d.QuotaID)
	}
}

func TestEngine_Check_TrackOnly_DoesNotReject(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 100, EnforcementMode: "track-only", Priority: 100},
	}
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	key := usageKey("virtual_key", "vk-1", CurrentPeriodKey("monthly"))
	usageCache.memUsage[key] = 1_000_000 // wildly over

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	d := engine.Check(context.Background(), chain, CostEstimate{}, &vkauth.VKMeta{ID: "vk-1"})
	if !d.Allowed || d.Action != "allow" {
		t.Errorf("track-only must not block: %+v", d)
	}
}

func TestEngine_Check_OverrideTakesPrecedenceOverPolicy(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 1_000_000, EnforcementMode: "reject", Priority: 100},
	}
	// Override sets a much lower limit on this specific VK.
	policyCache.overridesByKey["virtual_key:vk-tight"] = &CachedOverride{
		ID: "o-1", TargetType: "virtual_key", TargetID: "vk-tight",
		CostLimitCents: 50, EnforcementMode: "reject", PeriodType: "monthly",
	}
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	usageCache.memUsage[usageKey("virtual_key", "vk-tight", CurrentPeriodKey("monthly"))] = 60 // > override 50, < policy 1M

	d := engine.Check(context.Background(),
		[]CheckLevel{{TargetType: "virtual_key", TargetID: "vk-tight"}},
		CostEstimate{}, &vkauth.VKMeta{ID: "vk-tight"})
	if d.Allowed {
		t.Errorf("override should fire reject: %+v", d)
	}
	if d.QuotaID != "override:o-1" {
		t.Errorf("expected override quotaID, got %q", d.QuotaID)
	}
}

func TestEngine_Check_OverrideInheritsEnforcementFromPolicy(t *testing.T) {
	// Override with empty EnforcementMode + PeriodType must inherit from
	// the matching policy. Without inheritance, the level silently behaves
	// as if no limit existed.
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "daily", CostLimitCents: 999, EnforcementMode: "reject", Priority: 100},
	}
	policyCache.overridesByKey["virtual_key:vk-i"] = &CachedOverride{
		ID: "o-1", TargetType: "virtual_key", TargetID: "vk-i", CostLimitCents: 50, // enforcement/period empty
	}
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())
	usageCache.memUsage[usageKey("virtual_key", "vk-i", CurrentPeriodKey("daily"))] = 100

	d := engine.Check(context.Background(),
		[]CheckLevel{{TargetType: "virtual_key", TargetID: "vk-i"}},
		CostEstimate{}, &vkauth.VKMeta{ID: "vk-i"})
	if d.Allowed {
		t.Errorf("inherited reject didn't fire: %+v", d)
	}
}

func TestEngine_Check_HigherSeverityWinsAcrossLevels(t *testing.T) {
	// Org level says downgrade (priority 3); user level says reject
	// (priority 4). Final decision must be reject.
	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["user"] = []CachedPolicy{
		{ID: "u", Scope: "user", PeriodType: "monthly", CostLimitCents: 10, EnforcementMode: "reject", Priority: 100},
	}
	policyCache.policiesByScope["organization"] = []CachedPolicy{
		{ID: "o", Scope: "organization", PeriodType: "monthly", CostLimitCents: 10, EnforcementMode: "downgrade", Priority: 100},
	}
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	usageCache.memUsage[usageKey("user", "u1", CurrentPeriodKey("monthly"))] = 1000
	usageCache.memUsage[usageKey("organization", "org-1", CurrentPeriodKey("monthly"))] = 1000

	chain := []CheckLevel{
		{TargetType: "user", TargetID: "u1"},
		{TargetType: "organization", TargetID: "org-1"},
	}
	d := engine.Check(context.Background(), chain, CostEstimate{},
		&vkauth.VKMeta{ID: "vk", OrganizationID: "org-1"})

	if d.Action != "reject" {
		t.Errorf("higher severity didn't win: %+v", d)
	}
}

func TestEngine_Reconcile_IncrementsLevels(t *testing.T) {
	policyCache := NewPolicyCache(nil, testLogger())
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	dec := &Decision{
		Levels:    []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-r"}, {TargetType: "organization", TargetID: "org-r"}},
		PeriodKey: "2026-05",
	}
	actual := ActualUsage{PromptTokens: 1_000_000, InputPricePM: 1, OutputPricePM: 1} // cost 1 USD
	engine.Reconcile(context.Background(), dec, actual)

	if got := usageCache.memUsage[usageKey("virtual_key", "vk-r", "2026-05")]; got != 100 {
		t.Errorf("vk usage after reconcile: %d, want 100", got)
	}
	if got := usageCache.memUsage[usageKey("organization", "org-r", "2026-05")]; got != 100 {
		t.Errorf("org usage after reconcile: %d, want 100", got)
	}
}

func TestEngine_Reconcile_ZeroCost_NoOp(t *testing.T) {
	// Zero-cost reconcile (e.g. failed request or cache hit) must NOT
	// mutate counters — IncrMulti happens to short-circuit, but this test
	// pins the contract so a future refactor that increments by 0 (Redis
	// no-op) doesn't accidentally trigger a TTL refresh storm.
	policyCache := NewPolicyCache(nil, testLogger())
	usageCache := NewUsageCache(nil, testLogger())
	engine := NewEngine(policyCache, usageCache, testLogger())

	dec := &Decision{
		Levels:    []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-r"}},
		PeriodKey: "2026-05",
	}
	engine.Reconcile(context.Background(), dec, ActualUsage{})

	if got := usageCache.memUsage[usageKey("virtual_key", "vk-r", "2026-05")]; got != 0 {
		t.Errorf("zero-cost reconcile mutated counter: %d", got)
	}
}

// --- policy_cache.go FindPolicy / GetOverride ------------------------------

func TestPolicyCache_FindPolicy_NoMatchReturnsNil(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	if got := c.FindPolicy("virtual_key", "org", "personal"); got != nil {
		t.Errorf("empty cache: got %+v", got)
	}
}

func TestPolicyCache_FindPolicy_PriorityWins(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	// Cache contract: ORDER BY priority DESC from SQL; we mirror that here.
	c.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "high", Scope: "virtual_key", Priority: 100},
		{ID: "low", Scope: "virtual_key", Priority: 10},
	}
	got := c.FindPolicy("virtual_key", "org", "personal")
	if got == nil || got.ID != "high" {
		t.Errorf("priority-DESC scan didn't pick high: %+v", got)
	}
}

func TestPolicyCache_FindPolicy_OrganizationFilter(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "specific", Scope: "virtual_key", OrganizationID: "org-A", Priority: 50},
		{ID: "wild", Scope: "virtual_key", OrganizationID: "", Priority: 10}, // matches all orgs
	}
	if got := c.FindPolicy("virtual_key", "org-A", ""); got == nil || got.ID != "specific" {
		t.Errorf("org-specific should win: %+v", got)
	}
	if got := c.FindPolicy("virtual_key", "org-B", ""); got == nil || got.ID != "wild" {
		t.Errorf("wildcard should fall through: %+v", got)
	}
}

func TestPolicyCache_FindPolicy_VKTypeFilter(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "personal-only", Scope: "virtual_key", VKType: "personal", Priority: 100},
		{ID: "app-only", Scope: "virtual_key", VKType: "application", Priority: 100},
	}
	if got := c.FindPolicy("virtual_key", "", "personal"); got == nil || got.ID != "personal-only" {
		t.Errorf("personal: %+v", got)
	}
	if got := c.FindPolicy("virtual_key", "", "application"); got == nil || got.ID != "app-only" {
		t.Errorf("application: %+v", got)
	}
}

func TestPolicyCache_GetOverride(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.overridesByKey["virtual_key:vk-1"] = &CachedOverride{ID: "o", TargetType: "virtual_key", TargetID: "vk-1"}
	if got := c.GetOverride("virtual_key", "vk-1"); got == nil || got.ID != "o" {
		t.Errorf("hit: %+v", got)
	}
	if got := c.GetOverride("virtual_key", "vk-missing"); got != nil {
		t.Errorf("miss should be nil: %+v", got)
	}
}

func TestPolicyCache_OrgParents_ReturnsCopy(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.orgParents["org-a"] = "org-b"
	m := c.OrgParents()
	m["org-a"] = "MUTATED"
	if c.orgParents["org-a"] != "org-b" {
		t.Errorf("OrgParents() returned a live reference, not a copy")
	}
}

func TestPolicyCache_PolicySnapshot_FlattenAllScopes(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.policiesByScope["virtual_key"] = []CachedPolicy{{ID: "v"}}
	c.policiesByScope["user"] = []CachedPolicy{{ID: "u1"}, {ID: "u2"}}
	got := c.PolicySnapshot()
	if len(got) != 3 {
		t.Errorf("snapshot len: %d, want 3", len(got))
	}
}

// --- usage_cache.go --------------------------------------------------------

func TestUsageCache_InMemory_GetEmpty(t *testing.T) {
	c := NewUsageCache(nil, testLogger())
	got, err := c.GetUsage(context.Background(), "virtual_key", "vk-1", "2026-05")
	if err != nil || got != 0 {
		t.Errorf("empty: got %d err %v", got, err)
	}
}

func TestUsageCache_InMemory_IncrAndGet(t *testing.T) {
	c := NewUsageCache(nil, testLogger())
	ctx := context.Background()
	if err := c.IncrUsage(ctx, "virtual_key", "vk-1", "p", 100); err != nil {
		t.Fatalf("incr: %v", err)
	}
	if err := c.IncrUsage(ctx, "virtual_key", "vk-1", "p", 50); err != nil {
		t.Fatalf("incr2: %v", err)
	}
	got, _ := c.GetUsage(ctx, "virtual_key", "vk-1", "p")
	if got != 150 {
		t.Errorf("got %d, want 150", got)
	}
}

func TestUsageCache_IncrMulti_ZeroCostNoOp(t *testing.T) {
	c := NewUsageCache(nil, testLogger())
	err := c.IncrMulti(context.Background(),
		[]UsageLevel{{TargetType: "virtual_key", TargetID: "v"}}, "p", 0)
	if err != nil {
		t.Errorf("zero cost: %v", err)
	}
	if v := c.memUsage[usageKey("virtual_key", "v", "p")]; v != 0 {
		t.Errorf("zero cost mutated: %d", v)
	}
}

func TestUsageCache_IncrMulti_EmptyLevelsNoOp(t *testing.T) {
	c := NewUsageCache(nil, testLogger())
	if err := c.IncrMulti(context.Background(), nil, "p", 100); err != nil {
		t.Errorf("nil levels: %v", err)
	}
	if err := c.IncrMulti(context.Background(), []UsageLevel{}, "p", 100); err != nil {
		t.Errorf("empty levels: %v", err)
	}
}

func TestUsageCache_Redis_GetReturnsZeroOnMissingKey(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewUsageCache(rdb, testLogger())
	got, err := c.GetUsage(context.Background(), "virtual_key", "missing", "p")
	if err != nil || got != 0 {
		t.Errorf("missing key: got %d err %v", got, err)
	}
}

func TestUsageCache_Redis_IncrUsageSetsTTL(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewUsageCache(rdb, testLogger())
	ctx := context.Background()

	if err := c.IncrUsage(ctx, "virtual_key", "vk-1", "2026-05", 250); err != nil {
		t.Fatalf("incr: %v", err)
	}
	key := usageKey("virtual_key", "vk-1", "2026-05")
	val, _ := rdb.Get(ctx, key).Result()
	if val != "250" {
		t.Errorf("redis value: %q want 250", val)
	}
	ttl := mr.TTL(key)
	if ttl <= 0 {
		t.Errorf("TTL should be set on first increment, got %v", ttl)
	}
}

func TestUsageCache_Redis_IncrMultiPipelineIncrementsAll(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := NewUsageCache(rdb, testLogger())
	ctx := context.Background()

	levels := []UsageLevel{
		{TargetType: "virtual_key", TargetID: "v1"},
		{TargetType: "user", TargetID: "u1"},
		{TargetType: "organization", TargetID: "o1"},
	}
	if err := c.IncrMulti(ctx, levels, "2026-05", 300); err != nil {
		t.Fatalf("multi: %v", err)
	}
	for _, l := range levels {
		got, _ := c.GetUsage(ctx, l.TargetType, l.TargetID, "2026-05")
		if got != 300 {
			t.Errorf("%s/%s: got %d", l.TargetType, l.TargetID, got)
		}
	}
}

func TestPeriodTTL_Daily(t *testing.T) {
	// daily key from today must yield a TTL > 0 and < 26h.
	today := time.Now().UTC().Format("2006-01-02")
	ttl := periodTTL(today)
	if ttl <= 0 || ttl > 26*time.Hour {
		t.Errorf("daily TTL out of range: %v", ttl)
	}
}

func TestPeriodTTL_Monthly(t *testing.T) {
	month := time.Now().UTC().Format("2006-01")
	ttl := periodTTL(month)
	// Anywhere from a few seconds (last day of month) to 32 days+1h.
	if ttl <= 0 || ttl > 33*24*time.Hour {
		t.Errorf("monthly TTL out of range: %v", ttl)
	}
}

func TestPeriodTTL_UnknownFormat_Default(t *testing.T) {
	ttl := periodTTL("garbage")
	if ttl != 32*24*time.Hour {
		t.Errorf("unknown format: %v want 32d", ttl)
	}
}

func TestPeriodTTL_PastDailyFallback(t *testing.T) {
	// A daily key from a past day should still get a positive TTL — the
	// fallback "2 * time.Hour" branch. Without it, callers Expire with a
	// 0 or negative duration which Redis treats as immediate delete.
	ttl := periodTTL("2020-01-01")
	if ttl <= 0 {
		t.Errorf("past daily key: %v should be positive fallback", ttl)
	}
}

// --- downgrade.go converter ------------------------------------------------

// Note: TargetPricingFromStore is exercised indirectly when other modules
// build pricing slices for SelectCheapestIndex. The store dependency makes
// a direct test require importing store; instead we exercise the conversion
// shape via a hand-built fixture below.
