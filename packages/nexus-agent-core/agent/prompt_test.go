package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPrompt(t *testing.T) {
	p := BuildSystemPrompt(PromptInput{
		Env:           "prod",
		IsProd:        true,
		DomainContext: "Cost model: per-1k-token pricing by provider.",
	})
	// Persona + role.
	if !strings.Contains(p, "Nexus") || !strings.Contains(strings.ToLower(p), "operator") {
		t.Fatal("prompt must establish the Nexus operator-agent persona")
	}
	// Safety: propose-then-confirm mitigations, never fabricate, cite data.
	for _, must := range []string{"confirm", "never", "cite"} {
		if !strings.Contains(strings.ToLower(p), must) {
			t.Fatalf("prompt must carry safety guidance %q", must)
		}
	}
	// Mutation dispatch: a write must be performed by CALLING the tool (which raises
	// the Allow/Deny gate), never by asking "is that correct?" in chat — the gate, not a
	// typed reply, is the authorization. Guards the confirm-gate-never-fired bug.
	for _, must := range []string{"CALL the write tool", "Allow/Deny gate", "gate IS the confirmation"} {
		if !strings.Contains(p, must) {
			t.Fatalf("prompt must route mutations through the gate, not a chat question: missing %q", must)
		}
	}
	// IoT terminology boundary: user-facing vocab present, internal vocab banned.
	if !strings.Contains(p, "node") || !strings.Contains(p, "config sync") {
		t.Fatal("prompt must teach the user-facing terminology")
	}
	for _, banned := range []string{"shadow", "desired state", "reported state"} {
		if strings.Contains(strings.ToLower(p), banned) {
			t.Fatalf("prompt must NOT leak internal IoT term %q", banned)
		}
	}
	// Domain context injected.
	if !strings.Contains(p, "Cost model") {
		t.Fatal("prompt must include injected domain context")
	}
	// Prod awareness.
	if !strings.Contains(strings.ToUpper(p), "PROD") {
		t.Fatal("prod prompt must flag the production environment")
	}
	// Capabilities-first dispatch: route to the curated tools + the search-first
	// resource catalog, and never dump/guess.
	for _, must := range []string{"observe_traffic_list", "resource_search", "SPECIALIZED"} {
		if !strings.Contains(p, must) {
			t.Fatalf("prompt must carry the dispatch guidance %q", must)
		}
	}
	// Time-scope discipline: the analytics window, its 7d default, and the "today" cue.
	for _, must := range []string{"window", "7d", "today"} {
		if !strings.Contains(p, must) {
			t.Fatalf("prompt must carry the time-scope guidance %q", must)
		}
	}
	// Data-questions-have-no-dead-ends: search-before-giving-up, reformulate, compose
	// across kinds, and filter-first.
	for _, must := range []string{"no dead ends", "resource_search before", "Reformulate", "Compose across kinds", "Filter first"} {
		if !strings.Contains(p, must) {
			t.Fatalf("prompt must carry the no-dead-ends guidance %q", must)
		}
	}
	// Memory discipline: recall, remember the right types, update-not-duplicate, never secrets.
	for _, must := range []string{"get smarter over time", "recall(name)", "type=preference", "Update, don't duplicate", "Never store"} {
		if !strings.Contains(p, must) {
			t.Fatalf("prompt must carry the memory guidance %q", must)
		}
	}
}

func TestBuildSystemPromptNonProdAndNoDomain(t *testing.T) {
	p := BuildSystemPrompt(PromptInput{Env: "local", IsProd: false})
	if strings.Contains(strings.ToUpper(p), "PRODUCTION ENVIRONMENT") {
		t.Fatal("local prompt must not claim production")
	}
	if strings.Contains(p, "Domain context") {
		t.Fatal("no domain context section when none injected")
	}
	if !strings.Contains(p, "local") {
		t.Fatal("non-prod prompt should name the environment")
	}
}

// TestBuildSystemPromptWebSurface is the FR-20 / AC-3 assertion: the web assistant's
// system prompt carries NONE of the CLI/TUI-flavored or internal-only terms
// ("cockpit", "file-backed", "drift"), nor the IoT internals — yet keeps the same
// capabilities, safety, and operator vocabulary. The CLI default keeps the terser
// engineering wording (verified below) so this is a surface re-word, not a rewrite.
func TestBuildSystemPromptWebSurface(t *testing.T) {
	web := BuildSystemPrompt(PromptInput{Env: "web", IsProd: true, Surface: "web"})
	for _, banned := range []string{"cockpit", "file-backed", "drift", "shadow", "Thing", "desired state", "reported state"} {
		if strings.Contains(web, banned) {
			t.Errorf("web prompt (FR-20/AC-3) must not contain %q", banned)
		}
	}
	// The web re-word still teaches the same capabilities, safety, and user vocab.
	for _, must := range []string{"Nexus", "resource_search", "Allow/Deny gate", "node", "config sync", "durable memory"} {
		if !strings.Contains(web, must) {
			t.Errorf("web prompt must keep %q (re-word only, not a rewrite)", must)
		}
	}

	// The CLI/TUI default (no Surface) keeps the terser engineering wording — proves
	// the change is surface-scoped, not a global edit that also altered the CLI face.
	cli := BuildSystemPrompt(PromptInput{Env: "local", IsProd: false})
	for _, kept := range []string{"cockpit", "file-backed", "drift"} {
		if !strings.Contains(cli, kept) {
			t.Errorf("CLI default prompt should still use %q (web re-word must not leak to CLI)", kept)
		}
	}
}
