package core

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// awaitStatus polls until a provider reaches the expected health status. The
// HealthTracker applies records asynchronously (single writer), so a read
// immediately after recording must wait for the samples to be published.
func awaitStatus(t *testing.T, tracker *store.HealthTracker, providerID string, want store.HealthStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if tracker.GetHealth(providerID).Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider %s did not reach %s in time; got %+v", providerID, want, tracker.GetHealth(providerID))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHealthRanker_Reorder(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// Record failures for provider B to make it unhealthy
	for range 20 {
		tracker.RecordFailure("provB", "provider-b", 100)
	}
	// Provider A is healthy (no records = healthy)
	awaitStatus(t, tracker, "provB", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{
		{ProviderID: "provB", ProviderName: "provider-b"},
		{ProviderID: "provA", ProviderName: "provider-a"},
	}

	reordered := ranker.Reorder(targets)

	// provA (healthy) should come first
	if reordered[0].ProviderID != "provA" {
		t.Errorf("expected provA first, got %s", reordered[0].ProviderID)
	}
}

func TestHealthRanker_NilTracker(t *testing.T) {
	ranker := NewHealthRanker(nil)
	targets := []RoutingTarget{{ProviderID: "a"}, {ProviderID: "b"}}
	result := ranker.Reorder(targets)
	if len(result) != 2 || result[0].ProviderID != "a" {
		t.Error("nil tracker should return targets unchanged")
	}
}

func TestHealthRanker_SingleTarget(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{{ProviderID: "a"}}
	result := ranker.Reorder(targets)
	if len(result) != 1 {
		t.Error("single target should pass through")
	}
}

// TestHealthRanker_DegradedProvider verifies that a degraded provider (error rate
// > 5% but ≤ 25%) is ranked after healthy but before unavailable.
func TestHealthRanker_DegradedProvider(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// 10 failures + 90 successes = 10% error rate → degraded (>5%, ≤25%)
	for range 10 {
		tracker.RecordFailure("provDegraded", "provider-degraded", 100)
	}
	for range 90 {
		tracker.RecordSuccess("provDegraded", "provider-degraded", 50)
	}
	// provHealthy: no records → healthy
	// provUnavailable: all failures → unavailable
	for range 20 {
		tracker.RecordFailure("provUnavailable", "provider-unavailable", 100)
	}
	awaitStatus(t, tracker, "provDegraded", store.HealthStatusDegraded)
	awaitStatus(t, tracker, "provUnavailable", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{
		{ProviderID: "provUnavailable", ProviderName: "provider-unavailable"},
		{ProviderID: "provDegraded", ProviderName: "provider-degraded"},
		{ProviderID: "provHealthy", ProviderName: "provider-healthy"},
	}

	result := ranker.Reorder(targets)

	// Expected order: healthy → degraded → unavailable
	if result[0].ProviderID != "provHealthy" {
		t.Errorf("expected provHealthy first, got %s", result[0].ProviderID)
	}
	if result[1].ProviderID != "provDegraded" {
		t.Errorf("expected provDegraded second, got %s", result[1].ProviderID)
	}
	if result[2].ProviderID != "provUnavailable" {
		t.Errorf("expected provUnavailable third, got %s", result[2].ProviderID)
	}
}

// The armed context-upgrade target must stay behind every ordinary target, even
// when its provider is the healthiest thing in the list.
//
// It is armed for one situation — the chosen model overflowing its context —
// and it was picked for window size, not for the ability to serve this request.
// Promoted to the front it is tried first, and the executor's gate deliberately
// lets a front-positioned upgrade target through rather than dropping it, so
// the ordering is the only thing standing between "armed for failover" and
// "silently became the primary".
//
// Measured on production before this: a model:auto request carrying a markdown
// document traced "selected cohere/command-r7b-12-2024" and answered with
// moonshot's refusal of the document.
func TestHealthRanker_UpgradeOnlyNeverOutranksARealTarget(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// The SELECTED provider is degraded; the armed upgrade's provider is not.
	// Health alone would put the upgrade first, which is the bug.
	for range 20 {
		tracker.RecordFailure("selected", "selected-provider", 100)
	}
	awaitStatus(t, tracker, "selected", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	got := ranker.Reorder([]RoutingTarget{
		{ProviderID: "selected", ProviderName: "selected-provider"},
		{ProviderID: "upgrade", ProviderName: "upgrade-provider", ContextUpgradeOnly: true},
	})
	if got[0].ContextUpgradeOnly {
		t.Fatalf("the armed upgrade target was ranked first; it is tried before the model "+
			"the capability filter selected. order = %v",
			[]string{got[0].ProviderID, got[1].ProviderID})
	}
	if got[0].ProviderID != "selected" {
		t.Errorf("order[0] = %s, want the selected target", got[0].ProviderID)
	}
}

// Health ordering among ORDINARY targets must be untouched — the fix must not
// buy correctness here by disabling the ranker's actual job.
func TestHealthRanker_HealthStillOrdersOrdinaryTargets(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	for range 20 {
		tracker.RecordFailure("sick", "sick-provider", 100)
	}
	awaitStatus(t, tracker, "sick", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	got := ranker.Reorder([]RoutingTarget{
		{ProviderID: "sick", ProviderName: "sick-provider"},
		{ProviderID: "well", ProviderName: "well-provider"},
		{ProviderID: "upgrade", ProviderName: "upgrade-provider", ContextUpgradeOnly: true},
	})
	order := []string{got[0].ProviderID, got[1].ProviderID, got[2].ProviderID}
	want := []string{"well", "sick", "upgrade"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestReorder_NeverMovesTheStrategysFirstChoice.
//
// Position zero is the strategy's answer, arrived at with information health
// does not have: an LLM router's read of the request, a conditional's branch, a
// weighted draw the admin configured. Health knows which providers have been
// failing. Those are different questions, and letting the second overrule the
// first is how a request gets served by a model chosen for its uptime rather
// than for its fitness.
//
// This ranker's own comment records what that cost: a document request whose
// selected model was displaced by reordering came back "no inline document
// part" from the model that replaced it. That was fixed by a sort key which has
// since gone, so the protection is restated as a rule — the tail is health's to
// arrange, the head is not.
//
// The rule is about PREFERENCE, so the case that tests it is a provider that is
// degraded — answering, just worse lately. A provider that is unavailable
// answers nothing, and moving past it is a fact rather than a preference; that
// case is covered separately, and the cleaner answer for it is to drop the
// target rather than sort it.
func TestReorder_NeverMovesTheStrategysFirstChoice(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)

	// The strategy's pick sits on a provider that has been failing some of the
	// time — degraded, not down. It can still serve; health simply prefers the
	// others. That preference must not win.
	for range 3 {
		tracker.RecordFailure("p-chosen", "chosen", 10)
	}
	for range 17 {
		tracker.RecordSuccess("p-chosen", "chosen", 10)
	}
	awaitStatus(t, tracker, "p-chosen", store.HealthStatusDegraded)

	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{
		{ProviderID: "p-chosen", ProviderName: "chosen", ModelID: "the-strategys-answer"},
		{ProviderID: "p-other", ProviderName: "other", ModelID: "healthy-1"},
		{ProviderID: "p-other", ProviderName: "other", ModelID: "healthy-2"},
	}

	got := ranker.Reorder(targets)

	if len(got) != len(targets) {
		t.Fatalf("reorder returned %d targets for %d", len(got), len(targets))
	}
	if got[0].ModelID != "the-strategys-answer" {
		t.Errorf("position zero became %q — health overruled the choice the strategy made "+
			"with information health does not have, and the request is served by whichever "+
			"model happened to be up", got[0].ModelID)
	}
}

// TestReorder_TwoTargetsIsTheOrdinaryCaseAndTheHeadStillHolds.
//
// One primary and one fallback is the shape most rules actually have, and it is
// the shape where losing position zero costs the most: there is no third target
// to absorb the mistake, so a swap means every request goes to the fallback and
// the primary the admin chose is never tried at all.
//
// Reorder short-circuits this length rather than sorting a one-element tail.
// That shortcut is only safe while the head is exempt, so it is asserted here
// rather than left to be inferred from the longer-list case.
func TestReorder_TwoTargetsIsTheOrdinaryCaseAndTheHeadStillHolds(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)

	// The chosen primary is degraded — answering, just worse lately. The
	// fallback has a clean record, so health prefers it. That preference must
	// not become the routing decision.
	for range 3 {
		tracker.RecordFailure("p-primary", "primary", 10)
	}
	for range 17 {
		tracker.RecordSuccess("p-primary", "primary", 10)
	}
	awaitStatus(t, tracker, "p-primary", store.HealthStatusDegraded)

	got := NewHealthRanker(tracker).Reorder([]RoutingTarget{
		{ProviderID: "p-primary", ModelID: "the-admins-primary"},
		{ProviderID: "p-spare", ModelID: "the-fallback"},
	})

	if got[0].ModelID != "the-admins-primary" {
		t.Errorf("order[0] = %q — with only two targets, demoting the head means the primary "+
			"is never tried at all and every request lands on the fallback", got[0].ModelID)
	}
}

// TestReorder_AnUnavailableHeadIsTheOneCaseHealthMayOverrule.
//
// The stated exception to "position zero is the strategy's". A DEGRADED
// provider still answers, so preferring another over the strategy's pick is a
// preference beating a preference. An UNAVAILABLE one answers nothing:
// dispatching to it spends an attempt and a full timeout to learn what the
// tracker already knows, and on a two-target rule that timeout is most of the
// client's latency budget before the fallback is even tried.
//
// This exception has to stay narrow. Widening it back to "degraded counts too"
// reinstates exactly what B2 exists to refuse, and the two tests are adjacent
// so that the difference between them is impossible to read as an accident.
func TestReorder_AnUnavailableHeadIsTheOneCaseHealthMayOverrule(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	for range 20 {
		tracker.RecordFailure("p-down", "down", 100)
	}
	awaitStatus(t, tracker, "p-down", store.HealthStatusUnavailable)

	got := NewHealthRanker(tracker).Reorder([]RoutingTarget{
		{ProviderID: "p-down", ModelID: "on-a-dead-provider"},
		{ProviderID: "p-up", ModelID: "reachable"},
	})

	if got[0].ModelID != "reachable" {
		t.Errorf("order[0] = %q — the walk opens by calling a provider known to be answering "+
			"nothing, and the client waits out that timeout before anything else is tried",
			got[0].ModelID)
	}
}
