package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryEdgesAndTiers covers the error/edge branches and asserts the memory
// builtins are auto-tier (memory ops never need confirmation).
func TestMemoryEdgesAndTiers(t *testing.T) {
	base := t.TempDir()
	m := OpenMemoryStore(base, "") // empty env → the "default" scope dir (sanitizeEnv)

	// the index falls back to the body when a fact has no description.
	if err := m.Remember(MemoryFact{Name: "p", Type: MemBaseline, Body: "no-desc body line"}); err != nil {
		t.Fatal(err)
	}
	if idx, _ := m.Index(); !strings.Contains(idx, "no-desc body line") {
		t.Fatalf("index should fall back to the body when description is empty: %q", idx)
	}
	// a title that slugifies to empty is refused.
	if err := m.Remember(MemoryFact{Name: "!!!", Type: MemBaseline, Body: "x"}); err == nil {
		t.Fatal("a title that slugifies to empty must be refused")
	}
	// update: not-found, empty body, and secret are all refused.
	if err := m.Update("ghost", "x"); err == nil {
		t.Fatal("update of a missing fact must error")
	}
	if err := m.Update("p", "  "); err == nil {
		t.Fatal("update with an empty body must error")
	}
	if err := m.Update("p", "the key is sk-123abc"); err == nil {
		t.Fatal("update with a secret must be refused")
	}
	// forget of an empty name is a no-op, not an error.
	if removed, err := m.Forget(""); removed || err != nil {
		t.Fatalf("forget of an empty name should be a no-op, got removed=%v err=%v", removed, err)
	}
	// stray non-fact files in a scope dir are skipped by the index, and recall of a
	// corrupt fact errors.
	dir := filepath.Join(base, "default")
	_ = os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ignore me"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "corrupt.md"), []byte("no frontmatter"), 0o600)
	if idx, _ := m.Index(); strings.Contains(idx, "note") || strings.Contains(strings.ToLower(idx), "corrupt") {
		t.Fatalf("non-fact files must be skipped by the index: %q", idx)
	}
	if _, _, err := m.Recall("corrupt"); err == nil {
		t.Fatal("recall of a corrupt fact must error")
	}

	tools := []Tool{newRecallTool(m), newRememberTool(m), newUpdateMemoryTool(m), newForgetTool(m)}
	for _, tl := range tools {
		if tl.Tier() != TierAuto {
			t.Fatalf("%s must be TierAuto (memory ops need no confirmation)", tl.Name())
		}
		if r, _ := tl.Run(context.Background(), json.RawMessage(`{bad`)); !r.IsError {
			t.Fatalf("%s must return an error result on malformed input", tl.Name())
		}
	}
}

// TestMemoryCRUDAndScope covers the per-fact store end to end: remember writes a
// scoped file, the merged index lists both scopes, recall returns a body, update
// rewrites it in place, and forget deletes it.
func TestMemoryList(t *testing.T) {
	m := OpenMemoryStore(t.TempDir(), "prod")
	if facts := m.List(); len(facts) != 0 {
		t.Fatalf("empty store should List to 0 facts, got %d", len(facts))
	}
	// A preference (global) and a baseline (env) — List returns both scopes.
	if err := m.Remember(MemoryFact{Name: "Quiet mode", Type: MemPreference, Body: "terse"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remember(MemoryFact{Name: "latency baseline", Type: MemBaseline, Body: "p95 ~90ms"}); err != nil {
		t.Fatal(err)
	}
	facts := m.List()
	if len(facts) != 2 {
		t.Fatalf("List returned %d facts, want 2: %+v", len(facts), facts)
	}
	bodies := map[string]bool{}
	for _, f := range facts {
		bodies[f.Body] = true
	}
	if !bodies["terse"] || !bodies["p95 ~90ms"] {
		t.Fatalf("List missing facts across scopes: %+v", facts)
	}
}

func TestMemoryCRUDAndScope(t *testing.T) {
	m := OpenMemoryStore(t.TempDir(), "prod")

	// empty store → empty index.
	if idx, err := m.Index(); err != nil || idx != "" {
		t.Fatalf("empty store should index to empty, got %q err=%v", idx, err)
	}

	// a preference is global; a baseline is env-scoped.
	if err := m.Remember(MemoryFact{Name: "Quiet mode", Description: "prefers terse replies", Type: MemPreference, Body: "keep answers short"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remember(MemoryFact{Name: "latency baseline", Description: "p95 ~ 90ms normal", Type: MemBaseline, Body: "fleet p95 is normally about 90ms"}); err != nil {
		t.Fatal(err)
	}
	idx, _ := m.Index()
	for _, want := range []string{"### Global", "quiet-mode", "prefers terse replies", "### prod", "latency-baseline", "p95 ~ 90ms normal"} {
		if !strings.Contains(idx, want) {
			t.Fatalf("index missing %q:\n%s", want, idx)
		}
	}

	// recall returns the body (slug-insensitive to the original title casing/spacing).
	f, ok, err := m.Recall("Quiet Mode")
	if err != nil || !ok || f.Body != "keep answers short" {
		t.Fatalf("recall quiet-mode: ok=%v body=%q err=%v", ok, f.Body, err)
	}
	if !f.Global {
		t.Fatal("a preference must be stored in the global scope")
	}

	// update rewrites the body, keeps the rest.
	if err := m.Update("latency baseline", "fleet p95 is normally about 70ms now"); err != nil {
		t.Fatal(err)
	}
	if f, _, _ := m.Recall("latency baseline"); f.Body != "fleet p95 is normally about 70ms now" || f.Description != "p95 ~ 90ms normal" {
		t.Fatalf("update should change body, keep description: %+v", f)
	}

	// forget removes it; a second forget reports nothing removed.
	if removed, _ := m.Forget("latency baseline"); !removed {
		t.Fatal("forget should remove the fact")
	}
	if removed, _ := m.Forget("latency baseline"); removed {
		t.Fatal("forgetting a gone fact should report not-removed")
	}
	if _, ok, _ := m.Recall("latency baseline"); ok {
		t.Fatal("a forgotten fact must not recall")
	}
}

// TestMemoryDedupAndScopeIsolation: re-remembering the same title updates in place
// (no duplicate), and a different env's store does not see this env's baselines.
func TestMemoryDedupAndScopeIsolation(t *testing.T) {
	base := t.TempDir()
	prod := OpenMemoryStore(base, "prod")
	if err := prod.Remember(MemoryFact{Name: "openai entity", Description: "the prod openai provider", Type: MemEntity, Body: "id prov-openai-1"}); err != nil {
		t.Fatal(err)
	}
	// same slug, new content → update, not a second file.
	if err := prod.Remember(MemoryFact{Name: "OpenAI Entity", Description: "the prod openai provider", Type: MemEntity, Body: "id prov-openai-2"}); err != nil {
		t.Fatal(err)
	}
	if f, _, _ := prod.Recall("openai entity"); f.Body != "id prov-openai-2" {
		t.Fatalf("re-remember should update in place, got %q", f.Body)
	}
	idx, _ := prod.Index()
	if strings.Count(idx, "openai-entity") != 1 {
		t.Fatalf("dedup: the entity must appear once, got:\n%s", idx)
	}
	// a dev store shares global facts but NOT prod's env baselines/entities.
	dev := OpenMemoryStore(base, "dev")
	if _, ok, _ := dev.Recall("openai entity"); ok {
		t.Fatal("dev must not recall a prod-scoped entity")
	}
	if err := prod.Remember(MemoryFact{Name: "terse", Type: MemPreference, Body: "short replies"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := dev.Recall("terse"); !ok {
		t.Fatal("dev must recall a global preference written from prod")
	}
}

// TestMemoryRejectsSecretsAndBadType: secrets are refused and an unknown type errors.
func TestMemoryRejectsSecretsAndBadType(t *testing.T) {
	m := OpenMemoryStore(t.TempDir(), "local")
	if err := m.Remember(MemoryFact{Name: "leak", Type: MemEntity, Body: "the key is nvk_abc123def"}); err == nil {
		t.Fatal("a fact carrying a VK secret must be refused")
	}
	if err := m.Remember(MemoryFact{Name: "leak2", Type: MemPreference, Body: "password is hunter2"}); err == nil {
		t.Fatal("a fact carrying a password must be refused")
	}
	if err := m.Remember(MemoryFact{Name: "x", Type: "nonsense", Body: "y"}); err == nil {
		t.Fatal("an unknown memory type must be refused")
	}
	if err := m.Remember(MemoryFact{Name: "x", Type: MemPreference, Body: "  "}); err == nil {
		t.Fatal("an empty body must be refused")
	}
}

// TestMemoryTools exercises the recall/remember/update/forget builtins, including
// type-derived scope and the not-found paths.
func TestMemoryTools(t *testing.T) {
	m := OpenMemoryStore(t.TempDir(), "prod")
	rt, ut, ft, rc := newRememberTool(m), newUpdateMemoryTool(m), newForgetTool(m), newRecallTool(m)
	ctx := context.Background()

	// remember an entity — its type fixes the scope to the active env.
	if r, err := rt.Run(ctx, json.RawMessage(`{"type":"entity","title":"Edge VK","fact":"vk_id 42"}`)); err != nil || r.IsError {
		t.Fatalf("remember should succeed: %+v err=%v", r, err)
	}
	if f, _, _ := m.Recall("edge vk"); f.Global {
		t.Fatal("an entity must be stored in the env scope, not global")
	}
	// recall returns the body.
	if r, _ := rc.Run(ctx, json.RawMessage(`{"name":"Edge VK"}`)); r.IsError || !strings.Contains(r.Content, "vk_id 42") {
		t.Fatalf("recall tool should return the body, got %+v", r)
	}
	// recall a missing fact → error result (self-correcting).
	if r, _ := rc.Run(ctx, json.RawMessage(`{"name":"nope"}`)); !r.IsError {
		t.Fatal("recall of a missing fact should be an error result")
	}
	// update then forget.
	if r, _ := ut.Run(ctx, json.RawMessage(`{"name":"edge vk","fact":"vk_id 99"}`)); r.IsError {
		t.Fatalf("update should succeed: %+v", r)
	}
	if f, _, _ := m.Recall("edge vk"); f.Body != "vk_id 99" {
		t.Fatalf("update tool must rewrite the body, got %q", f.Body)
	}
	if r, _ := ft.Run(ctx, json.RawMessage(`{"name":"edge vk"}`)); r.IsError || !strings.Contains(r.Content, "forgot") {
		t.Fatalf("forget should succeed: %+v", r)
	}
	// forgetting a gone fact is a non-error "nothing to forget".
	if r, _ := ft.Run(ctx, json.RawMessage(`{"name":"edge vk"}`)); r.IsError {
		t.Fatalf("forgetting a gone fact is not an error, got %+v", r)
	}
	// a bad type through the tool is a recoverable error result.
	if r, _ := rt.Run(ctx, json.RawMessage(`{"type":"bogus","title":"t","fact":"f"}`)); !r.IsError {
		t.Fatal("remember with a bad type should be an error result")
	}
}

// TestRememberDerivesTitleWhenOmitted: the model sometimes calls remember without a
// title; rather than failing ("a memory needs a title"), the tool derives a stable
// title from the description (else the fact) so the save succeeds.
func TestRememberDerivesTitleWhenOmitted(t *testing.T) {
	m := OpenMemoryStore(t.TempDir(), "prod")
	rt := newRememberTool(m)
	ctx := context.Background()

	// No title, but a description → title derived from the description.
	r, err := rt.Run(ctx, json.RawMessage(`{"type":"preference","description":"prefers cost in USD","fact":"always show cost in USD"}`))
	if err != nil || r.IsError {
		t.Fatalf("remember without a title must still succeed by deriving one, got %+v err=%v", r, err)
	}
	if f, ok, _ := m.Recall("prefers cost in USD"); !ok || !strings.Contains(f.Body, "USD") {
		t.Fatalf("the fact must be saved under the derived title, ok=%v fact=%+v", ok, f)
	}

	// No title and no description → title derived from the first words of the fact.
	r2, err := rt.Run(ctx, json.RawMessage(`{"type":"preference","fact":"route anthropic through the eu region by default"}`))
	if err != nil || r2.IsError {
		t.Fatalf("remember with only a fact must still succeed, got %+v err=%v", r2, err)
	}
	if _, ok, _ := m.Recall("route anthropic through the eu region"); !ok {
		t.Fatal("the fact must be saved under a title derived from the first words of the fact")
	}
}

// TestMemoryParseRoundTrip: a rendered fact parses back to the same fields, and a
// file without frontmatter is skipped (not a crash).
func TestMemoryParseRoundTrip(t *testing.T) {
	f := MemoryFact{Name: "x-y", Description: "one line", Type: MemProcedure, Body: "do the thing\nthen the other"}
	got, ok := parseFact([]byte(renderFact(f)))
	if !ok || got.Name != "x-y" || got.Description != "one line" || got.Type != MemProcedure || got.Body != "do the thing\nthen the other" {
		t.Fatalf("round-trip mismatch: ok=%v got=%+v", ok, got)
	}
	if _, ok := parseFact([]byte("no frontmatter here")); ok {
		t.Fatal("a file without frontmatter must not parse")
	}
}
