package extract

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestAccumulator_AddAtRoot(t *testing.T) {
	a := NewJSONPatchAccumulator()
	op := JSONPatchOp{Path: "", Op: "add", Val: json.RawMessage(`{"message":{"id":"abc"}}`)}
	if err := a.Apply(op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	state := a.State()
	if msg, ok := state["message"].(map[string]any); !ok || msg["id"] != "abc" {
		t.Fatalf("state: %+v", state)
	}
}

func TestAccumulator_AppendString(t *testing.T) {
	a := NewJSONPatchAccumulator()
	// Initial add sets a string at /message/content/parts/0.
	if err := a.Apply(JSONPatchOp{
		Path: "",
		Op:   "add",
		Val:  json.RawMessage(`{"message":{"content":{"parts":["initial"]}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	// Append more text.
	if err := a.Apply(JSONPatchOp{
		Path: "/message/content/parts/0",
		Op:   "append",
		Val:  json.RawMessage(`" extra"`),
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := a.ExtractByPointer("/message/content/parts/0")
	if !ok || got != "initial extra" {
		t.Fatalf("text: %q ok=%v", got, ok)
	}
}

func TestAccumulator_ShorthandAppendContinuesLastPath(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{
		Path: "",
		Op:   "add",
		Val:  json.RawMessage(`{"message":{"content":{"parts":[""]}}}`),
	})
	_ = a.Apply(JSONPatchOp{
		Path: "/message/content/parts/0",
		Op:   "append",
		Val:  json.RawMessage(`"A few that stand"`),
	})
	// Shorthand frame: no Path, no Op, just a string value.
	if err := a.Apply(JSONPatchOp{
		Path: "",
		Op:   "",
		Val:  json.RawMessage(`" out recently,"`),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.ExtractByPointer("/message/content/parts/0")
	if got != "A few that stand out recently," {
		t.Fatalf("text: %q", got)
	}
}

func TestAccumulator_NestedPatchOps(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{
		Path: "",
		Op:   "add",
		Val:  json.RawMessage(`{"message":{"content":{"parts":["start"]}}}`),
	})
	// Patch with nested ops — last frame of a ChatGPT stream.
	patchVal := json.RawMessage(`[
		{"p":"/message/content/parts/0","o":"append","v":" mid"},
		{"p":"/message/content/parts/0","o":"append","v":" end"}
	]`)
	if err := a.Apply(JSONPatchOp{Path: "", Op: "patch", Val: patchVal}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.ExtractByPointer("/message/content/parts/0")
	if got != "start mid end" {
		t.Fatalf("text: %q", got)
	}
}

func TestAccumulator_DefaultOpWithObjectValue(t *testing.T) {
	// No `o` field, value is an object → default "add" semantics.
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{
		Path: "",
		Op:   "",
		Val:  json.RawMessage(`{"foo":"bar"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if a.State()["foo"] != "bar" {
		t.Fatalf("state: %+v", a.State())
	}
}

func TestAccumulator_ApplyJSONDecodeFailure(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.ApplyJSON([]byte("not json")); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestAccumulator_Remove(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Path: "", Op: "add", Val: json.RawMessage(`{"a":1,"b":2}`)})
	_ = a.Apply(JSONPatchOp{Path: "/a", Op: "remove", Val: json.RawMessage(`null`)})
	if _, exists := a.State()["a"]; exists {
		t.Fatalf("a not removed: %+v", a.State())
	}
	if a.State()["b"] == nil {
		t.Fatalf("b lost: %+v", a.State())
	}
}

func TestAccumulator_ChatGPTLikeStream(t *testing.T) {
	// End-to-end: simulate the baa07c15 stream pattern and verify the
	// final accumulator state has the full assistant message text.
	a := NewJSONPatchAccumulator()
	frames := []string{
		// User message addition (whole tree).
		`{"p":"","o":"add","v":{"message":{"author":{"role":"user"},"content":{"parts":["hello"]}}}}`,
		// Assistant message replaces root (typical ChatGPT-web — each new turn overwrites).
		`{"p":"","o":"add","v":{"message":{"author":{"role":"assistant"},"content":{"parts":[""]}}}}`,
		// Streaming deltas.
		`{"p":"/message/content/parts/0","o":"append","v":"A few that stand"}`,
		`{"v":" out recently,"}`, // shorthand continuation
		`{"v":" depending on the kind of reading mood you're in."}`,
		// Final patch bundle.
		`{"p":"","o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":" Enjoy!"}]}`,
	}
	for i, f := range frames {
		if err := a.ApplyJSON([]byte(f)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	got, ok := a.ExtractByPointer("/message/content/parts/0")
	if !ok {
		t.Fatalf("text not at path: %+v", a.State())
	}
	want := "A few that stand out recently, depending on the kind of reading mood you're in. Enjoy!"
	if got != want {
		t.Fatalf("text: %q\nwant: %q", got, want)
	}
}
