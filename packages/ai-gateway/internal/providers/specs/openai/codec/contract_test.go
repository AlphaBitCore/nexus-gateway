package codec

// ruleSet mechanics: the two doors of the contract applier. Named failure
// modes: a due edit on a duplicated key must refuse the surgical door
// (sjson edits the FIRST occurrence, parsers take last-wins); a rename
// must never overwrite a caller-supplied target field; the fast paths
// must return the same slice (zero copy); rewrites are reported in rule
// order with the exact legacy label strings.

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func quirkGate(m string) bool { return strings.HasPrefix(m, "quirk-") }

func testRules() []FieldRule {
	return []FieldRule{
		{Applies: quirkGate, Field: "max_tokens", RenameTo: "max_completion_tokens"},
		{Applies: quirkGate, Field: "temperature"},
		{Applies: quirkGate, Field: "top_p"},
	}
}

func TestFieldRule_LabelDerivation(t *testing.T) {
	if got := (FieldRule{Field: "temperature"}).label(); got != "temperature→removed" {
		t.Fatalf("remove label: %q", got)
	}
	if got := (FieldRule{Field: "max_tokens", RenameTo: "max_completion_tokens"}).label(); got != "max_tokens→max_completion_tokens" {
		t.Fatalf("rename label: %q", got)
	}
	if got := (FieldRule{Field: "dimensions", Label: "custom"}).label(); got != "custom" {
		t.Fatalf("label override: %q", got)
	}
}

func TestRuleSet_ApplyBytes_NonMatchingModel_SameSliceZeroReads(t *testing.T) {
	s := newRuleSet(testRules())
	body := []byte(`{"model":"plain","temperature":0.7,"max_tokens":10}`)
	out, rw, ok := s.applyBytes(body, "plain-model")
	if !ok || rw != nil {
		t.Fatalf("non-matching model must be a no-op: ok=%v rw=%v", ok, rw)
	}
	if &out[0] != &body[0] {
		t.Fatal("fast path must return the same slice")
	}
}

func TestRuleSet_ApplyBytes_MatchingModelNothingPresent_SameSlice(t *testing.T) {
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","messages":[]}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok || rw != nil {
		t.Fatalf("nothing due must be a no-op: ok=%v rw=%v", ok, rw)
	}
	if &out[0] != &body[0] {
		t.Fatal("nothing-due path must return the same slice")
	}
}

func TestRuleSet_ApplyBytes_StripAndRename_ReportedInRuleOrder(t *testing.T) {
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","top_p":0.9,"max_tokens":128,"temperature":0}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("surgical door must handle a clean body")
	}
	want := []string{"max_tokens→max_completion_tokens", "temperature→removed", "top_p→removed"}
	if len(rw) != len(want) {
		t.Fatalf("rewrites %v, want %v", rw, want)
	}
	for i := range want {
		if rw[i] != want[i] {
			t.Fatalf("rewrites[%d]=%q, want %q (rule order fixes report order)", i, rw[i], want[i])
		}
	}
	for _, gone := range []string{"temperature", "top_p", "max_tokens"} {
		if gjson.GetBytes(out, gone).Exists() {
			t.Fatalf("%s must be gone: %s", gone, out)
		}
	}
	if gjson.GetBytes(out, "max_completion_tokens").Int() != 128 {
		t.Fatalf("rename must carry the value: %s", out)
	}
}

func TestRuleSet_ApplyBytes_RenamePreservesExactLiteral(t *testing.T) {
	// The rename moves the caller's raw JSON literal, not a re-marshalled
	// float — 0.30000000000000004-style drift on integer caps would be a
	// silent body change.
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","max_tokens":1e3}`)
	out, _, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("surgical door expected")
	}
	if got := gjson.GetBytes(out, "max_completion_tokens").Raw; got != "1e3" {
		t.Fatalf("raw literal must move verbatim, got %q in %s", got, out)
	}
}

func TestRuleSet_ApplyBytes_RenameTargetPresent_DeleteOnlyNoOverwrite(t *testing.T) {
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","max_tokens":128,"max_completion_tokens":512}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("surgical door expected")
	}
	if gjson.GetBytes(out, "max_completion_tokens").Int() != 512 {
		t.Fatalf("caller's target field must win: %s", out)
	}
	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("source field must still be removed: %s", out)
	}
	if len(rw) != 1 || rw[0] != "max_tokens→max_completion_tokens" {
		t.Fatalf("the rename is still reported (the upstream sees a different cap than sent): %v", rw)
	}
}

func TestRuleSet_ApplyBytes_DuplicatedEditedKey_RefusesSurgicalDoor(t *testing.T) {
	// sjson edits the FIRST occurrence; parsers take last-wins. A dup on a
	// field about to be edited must force the decode door.
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","temperature":0.1,"temperature":0.9}`)
	if _, _, ok := s.applyBytes(body, "quirk-1"); ok {
		t.Fatal("duplicated edited key must refuse the surgical door")
	}
}

func TestRuleSet_ApplyBytes_DuplicateOnUntouchedField_StaysSurgical(t *testing.T) {
	// A dup on a field no rule edits (here: model) is not this door's
	// problem — the stamp helper has its own precondition.
	s := newRuleSet(testRules())
	body := []byte(`{"model":"quirk-1","model":"quirk-2","temperature":0}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatalf("dup on an unedited field must not force the decode door")
	}
	if gjson.GetBytes(out, "temperature").Exists() || len(rw) != 1 {
		t.Fatalf("edit must still land: %s %v", out, rw)
	}
}

func TestRuleSet_ApplyMap_RenameNoOverwriteAndOrder(t *testing.T) {
	s := newRuleSet(testRules())
	payload := map[string]any{
		"max_tokens":            float64(128),
		"max_completion_tokens": float64(512),
		"temperature":           0.5,
	}
	rw := s.applyMap(payload, "quirk-1")
	if payload["max_completion_tokens"] != float64(512) {
		t.Fatalf("map door must not overwrite the caller's target: %v", payload)
	}
	if _, has := payload["max_tokens"]; has {
		t.Fatal("map door must remove the source field")
	}
	if _, has := payload["temperature"]; has {
		t.Fatal("map door must strip temperature")
	}
	want := []string{"max_tokens→max_completion_tokens", "temperature→removed"}
	if len(rw) != 2 || rw[0] != want[0] || rw[1] != want[1] {
		t.Fatalf("rewrites %v, want %v", rw, want)
	}
}

func TestRuleSet_ApplyMap_NonMatchingModel_Nil(t *testing.T) {
	s := newRuleSet(testRules())
	payload := map[string]any{"temperature": 0.5}
	if rw := s.applyMap(payload, "plain"); rw != nil {
		t.Fatalf("non-matching model: %v", rw)
	}
	if _, has := payload["temperature"]; !has {
		t.Fatal("non-matching model must not be edited")
	}
}

func TestRuleSet_AnyFieldPresent(t *testing.T) {
	s := newRuleSet(testRules())
	if s.anyFieldPresent([]byte(`{"temperature":0}`), "plain") {
		t.Fatal("gate must run before the probe")
	}
	if s.anyFieldPresent([]byte(`{"messages":[]}`), "quirk-1") {
		t.Fatal("no rule field present")
	}
	if !s.anyFieldPresent([]byte(`{"top_p":0.9}`), "quirk-1") {
		t.Fatal("present rule field missed")
	}
}

func TestNewRuleSet_TooManyRules_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on >64 rules")
		}
	}()
	rules := make([]FieldRule, 65)
	for i := range rules {
		rules[i] = FieldRule{Applies: quirkGate, Field: "f"}
	}
	newRuleSet(rules)
}

// SetRaw + WhenPresentNonEmpty mechanics: the forced-value rule class
// (an explicit value is the only accepted state; absence is rejected by
// the upstream too, so removal cannot serve).
func forcedRules() []FieldRule {
	return []FieldRule{{
		Applies:             quirkGate,
		Field:               "reasoning_effort",
		SetRaw:              `"none"`,
		WhenPresentNonEmpty: "tools",
		Label:               "reasoning_effort→none (test)",
	}}
}

func TestRuleSet_SetRaw_ConditionAbsent_SameSlice(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","messages":[],"reasoning_effort":"high"}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok || rw != nil {
		t.Fatalf("without the condition field nothing is due: ok=%v rw=%v", ok, rw)
	}
	if &out[0] != &body[0] {
		t.Fatal("nothing-due must return the same slice")
	}
}

func TestRuleSet_SetRaw_EmptyConditionArray_NotDue(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":[],"reasoning_effort":"high"}`)
	if _, rw, ok := s.applyBytes(body, "quirk-1"); !ok || rw != nil {
		t.Fatalf("an empty condition array must not fire the rule (probed: vendor accepts it): ok=%v rw=%v", ok, rw)
	}
}

func TestRuleSet_SetRaw_FieldAbsent_Injected(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":[{"type":"function"}]}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("surgical door expected")
	}
	if gjson.GetBytes(out, "reasoning_effort").String() != "none" {
		t.Fatalf("absent field must be injected (absence itself is rejected upstream): %s", out)
	}
	if len(rw) != 1 || rw[0] != "reasoning_effort→none (test)" {
		t.Fatalf("the injection is reported: %v", rw)
	}
}

func TestRuleSet_SetRaw_WrongValue_Overwritten(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":[{"type":"function"}],"reasoning_effort":"high"}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok {
		t.Fatal("surgical door expected")
	}
	if gjson.GetBytes(out, "reasoning_effort").String() != "none" {
		t.Fatalf("any other value must be forced to the accepted one: %s", out)
	}
	if len(rw) != 1 {
		t.Fatalf("reported once: %v", rw)
	}
}

func TestRuleSet_SetRaw_AlreadyCorrect_SameSliceNoReport(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":[{"type":"function"}],"reasoning_effort":"none"}`)
	out, rw, ok := s.applyBytes(body, "quirk-1")
	if !ok || rw != nil {
		t.Fatalf("the accepted value is not a rewrite: ok=%v rw=%v", ok, rw)
	}
	if &out[0] != &body[0] {
		t.Fatal("conformant body must return the same slice")
	}
}

func TestRuleSet_SetRaw_DupField_DecodeDoor(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":[{"type":"function"}],"reasoning_effort":"high","reasoning_effort":"low"}`)
	if _, _, ok := s.applyBytes(body, "quirk-1"); ok {
		t.Fatal("a duplicated field about to be set must take the decode door")
	}
	payload := map[string]any{"tools": []any{map[string]any{}}, "reasoning_effort": "high"}
	rw := s.applyMap(payload, "quirk-1")
	if payload["reasoning_effort"] != "none" || len(rw) != 1 {
		t.Fatalf("map door must force the value too: %v %v", payload, rw)
	}
}

func TestRuleSet_SetRaw_MapDoor_Boundaries(t *testing.T) {
	s := newRuleSet(forcedRules())
	for name, payload := range map[string]map[string]any{
		"condition absent":      {"reasoning_effort": "high"},
		"empty condition array": {"tools": []any{}, "reasoning_effort": "high"},
		"already correct":       {"tools": []any{map[string]any{}}, "reasoning_effort": "none"},
	} {
		if rw := s.applyMap(payload, "quirk-1"); rw != nil {
			t.Errorf("%s: nothing due, got %v", name, rw)
		}
	}
	if rw := s.applyMap(map[string]any{"tools": []any{map[string]any{}}, "reasoning_effort": "high"}, "plain"); rw != nil {
		t.Errorf("gate must run first: %v", rw)
	}
}

func TestNewRuleSet_RenameAndSetRawTogether_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on a rule declaring both RenameTo and SetRaw")
		}
	}()
	newRuleSet([]FieldRule{{Applies: quirkGate, Field: "f", RenameTo: "g", SetRaw: `"x"`}})
}

// Construction-time guards: the rule mechanism's invariants are enforced
// where a bad rule is a programming error, not discovered as a request-
// path panic or a silent door divergence.
func TestNewRuleSet_ConstructionGuards(t *testing.T) {
	expectPanic := func(name string, rules []FieldRule) {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected construction panic")
				}
			}()
			newRuleSet(rules)
		})
	}
	expectPanic("SetRaw container literal", []FieldRule{
		{Applies: quirkGate, Field: "reasoning", SetRaw: `{"effort":"none"}`},
	})
	expectPanic("SetRaw invalid JSON", []FieldRule{
		{Applies: quirkGate, Field: "f", SetRaw: `none`},
	})
	expectPanic("two rules on one field", []FieldRule{
		{Applies: quirkGate, Field: "temperature"},
		{Applies: quirkGate, Field: "temperature", SetRaw: `1`},
	})
	expectPanic("field is another rule's rename target", []FieldRule{
		{Applies: quirkGate, Field: "max_tokens", RenameTo: "max_completion_tokens"},
		{Applies: quirkGate, Field: "max_completion_tokens"},
	})
	expectPanic("field is another rule's condition", []FieldRule{
		{Applies: quirkGate, Field: "tools"},
		{Applies: quirkGate, Field: "reasoning_effort", SetRaw: `"none"`, WhenPresentNonEmpty: "tools"},
	})
	expectPanic("rename target is another rule's condition", []FieldRule{
		{Applies: quirkGate, Field: "functions", RenameTo: "tools"},
		{Applies: quirkGate, Field: "reasoning_effort", SetRaw: `"none"`, WhenPresentNonEmpty: "tools"},
	})
	expectPanic("gjson path syntax in field", []FieldRule{
		{Applies: quirkGate, Field: "messages.0.content"},
	})
	expectPanic("gjson path syntax in condition", []FieldRule{
		{Applies: quirkGate, Field: "f", SetRaw: `1`, WhenPresentNonEmpty: "tools.#"},
	})
}

// The condition probe reads `<field>.0`, so a scalar condition value does
// not fire the rule on either door (aligned semantics: "has a first
// element").
func TestRuleSet_SetRaw_ScalarCondition_NotDue(t *testing.T) {
	s := newRuleSet(forcedRules())
	body := []byte(`{"model":"quirk-1","tools":"oops","reasoning_effort":"high"}`)
	if _, rw, ok := s.applyBytes(body, "quirk-1"); !ok || rw != nil {
		t.Fatalf("scalar condition must not fire (bytes door): ok=%v rw=%v", ok, rw)
	}
	payload := map[string]any{"tools": "oops", "reasoning_effort": "high"}
	if rw := s.applyMap(payload, "quirk-1"); rw != nil {
		t.Fatalf("scalar condition must not fire (map door): %v", rw)
	}
}

// Structural rules: gate-matching models route to the decode door from
// every chat entry point; non-matching models never pay the decode.
func structuralContract(calls *int) Contract {
	return Contract{
		Chat: testRules(),
		ChatStructural: []StructuralRule{{
			Applies: quirkGate,
			Apply: func(payload map[string]any, _ string) []string {
				*calls++
				if _, has := payload["marker"]; !has {
					payload["marker"] = true
					return []string{"marker→added"}
				}
				return nil
			},
		}},
	}
}

func TestStructuralRule_ChatDoors(t *testing.T) {
	calls := 0
	c := newIdentity(structuralContract(&calls))
	target := provcore.CallTarget{ProviderModelID: "quirk-1"}

	t.Run("non-stream native door routes to decode and reports", func(t *testing.T) {
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"quirk-1","messages":[],"temperature":0.2}`), target, false)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "marker").Bool() != true {
			t.Fatalf("structural edit must land: %s", res.Body)
		}
		if gjson.GetBytes(res.Body, "temperature").Exists() {
			t.Fatalf("field rules still run on the same decode pass: %s", res.Body)
		}
		found := false
		for _, r := range res.Rewrites {
			if r == "marker→added" {
				found = true
			}
		}
		if !found {
			t.Fatalf("structural rewrites must be reported: %v", res.Rewrites)
		}
	})

	t.Run("canonical door agrees", func(t *testing.T) {
		res, err := c.EncodeRequest(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"quirk-1","messages":[]}`), target)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "marker").Bool() != true {
			t.Fatalf("two doors, one structural rule: %s", res.Body)
		}
	})

	t.Run("streaming conformant body must not all-skip past the rule", func(t *testing.T) {
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"quirk-1","stream":true,"stream_options":{"include_usage":true},"messages":[]}`), target, true)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(res.Body, "marker").Bool() != true {
			t.Fatalf("streaming all-skip must yield to a gate-matching structural rule: %s", res.Body)
		}
	})

	t.Run("non-matching model never reaches the rule", func(t *testing.T) {
		before := calls
		body := []byte(`{"model":"plain","messages":[],"temperature":0.2}`)
		res, err := c.RewriteNative(typology.WireShapeOpenAIChat, body,
			provcore.CallTarget{ProviderModelID: "plain"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if calls != before {
			t.Fatal("gate must run before any decode")
		}
		if &res.Body[0] != &body[0] {
			t.Fatal("non-matching model keeps the verbatim fast path")
		}
	})

	t.Run("idempotent with no duplicate reporting", func(t *testing.T) {
		first, err := c.RewriteNative(typology.WireShapeOpenAIChat,
			[]byte(`{"model":"quirk-1","messages":[]}`), target, false)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.RewriteNative(typology.WireShapeOpenAIChat, first.Body, target, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Rewrites) != 0 {
			t.Fatalf("second pass must report nothing: %v", second.Rewrites)
		}
	})
}
