package routing

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The virtual key's model-access invariant has three implementations that look
// inconsistent unless you know why, so these read production source and fail
// when one of them drifts toward the wrong reading.
//
// The invariant: a key may be SERVED only models on its allow list; what a
// caller may ASK for is restricted only when the asked-for model is the served
// one. Written out in matcher/vkaccess.go.
//
// Structural rather than behavioural on purpose. Each leg already has
// behavioural tests; what they cannot catch is a future change that makes leg 2
// "consistent" with leg 1 — which reads like a fix and would delete auto
// routing, because "auto" is not a catalog model and can never be on an allow
// list.

// Leg 1 must test the RESOLVED model, not the requested string.
//
// This is the exact misreading that prompted the test: the refusal message
// names `requestedModel` while the predicate operates on `model`, so the check
// scans as a requested-model gate and is a served-model gate. Someone
// propagating the apparent rule to the rule path would break auto.
func TestVKAccess_PassthroughChecksTheResolvedModelNotTheRequestString(t *testing.T) {
	const file = "../ingress/proxy/stage_routing_passthrough.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var args []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ModelMatchesAllowedRefs" {
			return true
		}
		for _, a := range call.Args {
			var b strings.Builder
			_ = printer.Fprint(&b, fset, a)
			args = append(args, b.String())
		}
		return true
	})

	if len(args) == 0 {
		t.Fatal("the passthrough no longer checks the VK allow list at all — leg 1 of the invariant is gone")
	}
	joined := strings.Join(args, ", ")
	if !strings.Contains(joined, "model.ID") {
		t.Errorf("leg 1 must test the RESOLVED catalog model; got args (%s)", joined)
	}
	if strings.Contains(joined, "requestedModel") {
		t.Errorf("leg 1 is testing the raw requested string (%s). The invariant gates what is SERVED; "+
			"gating the request breaks `model: \"auto\"`, which can never appear on an allow list.", joined)
	}
}

// Leg 3 must narrow the candidate set BEFORE the judge prompt is built.
//
// Filtering after the pick would pay for a router LLM call and then throw the
// answer away, and could leave zero targets from a judge that chose correctly.
func TestVKAccess_SmartFiltersCandidatesBeforeTheJudgeSeesThem(t *testing.T) {
	const file = "strategies/strategy_smart.go"
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(raw)
	filterAt := strings.Index(src, "ModelMatchesAllowedRefs")
	if filterAt < 0 {
		t.Fatal("smart routing no longer filters candidates by the VK allow list — leg 3 of the invariant is gone")
	}
	// The judge is only worth paying for once the candidate set is final.
	for _, marker := range []string{"buildRouterPrompt", "routerPrompt", "callRouter", "judge"} {
		if at := strings.Index(strings.ToLower(src), strings.ToLower(marker)); at >= 0 && at < filterAt {
			t.Errorf("the VK candidate filter runs AFTER %q (offset %d vs %d): the router would rank models "+
				"the key cannot use, and its answer would then be discarded", marker, filterAt, at)
			return
		}
	}
}
