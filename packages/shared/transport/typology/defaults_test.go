package typology

import "testing"

// TestDefaults_EveryKindHasAtLeastOneRule prevents the failure mode
// where an EndpointKind constant is defined but no rule produces it —
// rules become unreachable and the constant rots. Adding a new
// EndpointKind requires at least one Rule entry referencing it.
//
// EndpointKindVideoGeneration and EndpointKindJob are exempt because
// they are reserved placeholders (no provider has shipped these
// endpoints in production yet; rules are added when the first provider
// lands).
func TestDefaults_EveryKindHasAtLeastOneRule(t *testing.T) {
	produced := map[EndpointKind]bool{}
	for _, r := range defaultRules {
		produced[r.Kind] = true
	}
	exempt := map[EndpointKind]bool{
		EndpointKindVideoGeneration: true,
		EndpointKindJob:             true,
		// Label-only refinement: the path table classifies /v1/responses as
		// chat-kind (defaults.go) for routing / cache / hook dispatch; the
		// traffic_event endpoint_type stamp overrides the label to "responses".
		// There is intentionally no dedicated path→responses rule.
		EndpointKindResponses: true,
	}
	for _, k := range AllEndpointKinds {
		if exempt[k] {
			continue
		}
		if !produced[k] {
			t.Errorf("EndpointKind %v has no rule in defaultRules; add one to defaults.go or mark exempt", k)
		}
	}
}

// TestDefaults_EveryShapeHasAtLeastOneRule mirrors the previous test
// for WireShape constants. Shapes without a rule are dead code.
//
// Bedrock shapes (WireShapeBedrockConverse, WireShapeBedrockInvoke) and
// Cohere chat / Voyage shapes are reserved for future intercepted
// paths and exempt until live rules land.
func TestDefaults_EveryShapeHasAtLeastOneRule(t *testing.T) {
	produced := map[WireShape]bool{}
	for _, r := range defaultRules {
		produced[r.Shape] = true
	}
	exempt := map[WireShape]bool{
		WireShapeBedrockConverse:   true, // AIGW dispatches via spec_adapter not via path rule
		WireShapeBedrockInvoke:     true, // same
		WireShapeBedrockEmbeddings: true, // same — Bedrock Titan/Cohere embedding via spec_adapter
		WireShapeCohereChat:        true, // chat path not yet observed in production interception
		WireShapeVoyageEmbeddings:  true, // Voyage path not yet observed in production interception
	}
	for _, w := range AllWireShapes {
		if exempt[w] {
			continue
		}
		if !produced[w] {
			t.Errorf("WireShape %v has no rule in defaultRules; add one to defaults.go or mark exempt", w)
		}
	}
}

// TestDefaults_RulesUseDefinedConstants asserts every Rule references
// a defined EndpointKind + WireShape constant (no typoed string
// literals leaking into the rule table).
func TestDefaults_RulesUseDefinedConstants(t *testing.T) {
	for i, r := range defaultRules {
		if !r.Kind.IsValid() {
			t.Errorf("defaultRules[%d] (%s %s) Kind = %q; not a defined EndpointKind constant",
				i, r.Method, r.PathPattern, r.Kind)
		}
		// WireShapeNone is valid for body-less endpoints (GET /v1/models).
		if r.Shape != WireShapeNone && !r.Shape.IsValid() {
			t.Errorf("defaultRules[%d] (%s %s) Shape = %q; not a defined WireShape constant",
				i, r.Method, r.PathPattern, r.Shape)
		}
	}
}

// TestDefaults_NoDuplicateExactPatterns guards against the easy mistake
// of registering the same (method, pattern) pair twice with different
// outcomes. The first-match-wins semantics would silently honor only
// the first; the duplicate is dead.
func TestDefaults_NoDuplicateExactPatterns(t *testing.T) {
	seen := map[string]int{}
	for i, r := range defaultRules {
		key := r.Method + " " + r.PathPattern
		if prev, dup := seen[key]; dup {
			t.Errorf("defaultRules[%d] duplicates [%d]: %s", i, prev, key)
		}
		seen[key] = i
	}
}

// TestDefaults_BodylessKindsHaveNoneShape asserts that EndpointKindModels
// (the only body-less endpoint kind today) consistently uses
// WireShapeNone in its rules. A rule with EndpointKindModels and a
// non-empty WireShape would be a coordination error: there is no body
// to parse.
func TestDefaults_BodylessKindsHaveNoneShape(t *testing.T) {
	for i, r := range defaultRules {
		if r.Kind == EndpointKindModels && r.Shape != WireShapeNone {
			t.Errorf("defaultRules[%d] (%s %s) Kind=models but Shape=%q; models is body-less, use WireShapeNone",
				i, r.Method, r.PathPattern, r.Shape)
		}
	}
}
