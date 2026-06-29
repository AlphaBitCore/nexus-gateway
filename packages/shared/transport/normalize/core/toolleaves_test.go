package core

import (
	"reflect"
	"testing"
)

func TestToolUseStringLeaves_DeterministicAcrossRepeatedCalls(t *testing.T) {
	// A map with several keys exercises Go's randomized iteration; the walk
	// must produce byte-identical ordering every call.
	input := map[string]any{
		"zeta":  "z-value",
		"alpha": "a-value",
		"mid":   "m-value",
		"beta":  "b-value",
		"gamma": "g-value",
	}
	first := ToolUseStringLeaves(input)
	want := []ToolLeaf{
		{Ordinal: 0, Value: "a-value"}, // alpha
		{Ordinal: 1, Value: "b-value"}, // beta
		{Ordinal: 2, Value: "g-value"}, // gamma
		{Ordinal: 3, Value: "m-value"}, // mid
		{Ordinal: 4, Value: "z-value"}, // zeta
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("sorted-key order wrong: got %+v want %+v", first, want)
	}
	for i := range 50 {
		got := ToolUseStringLeaves(input)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d differed from first: got %+v want %+v", i, got, first)
		}
	}
}

func TestToolUseStringLeaves_NestedObjectAndArray(t *testing.T) {
	input := map[string]any{
		"b_obj": map[string]any{
			"inner_z": "iz",
			"inner_a": "ia",
		},
		"a_arr": []any{"a0", "a1", "a2"},
		"c_str": "cs",
	}
	got := ToolUseStringLeaves(input)
	// Top keys sorted: a_arr, b_obj, c_str.
	// a_arr by index: a0,a1,a2 -> ordinals 0,1,2
	// b_obj keys sorted: inner_a, inner_z -> ia,iz -> ordinals 3,4
	// c_str -> cs -> ordinal 5
	want := []ToolLeaf{
		{Ordinal: 0, Value: "a0"},
		{Ordinal: 1, Value: "a1"},
		{Ordinal: 2, Value: "a2"},
		{Ordinal: 3, Value: "ia"},
		{Ordinal: 4, Value: "iz"},
		{Ordinal: 5, Value: "cs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested order wrong: got %+v want %+v", got, want)
	}
}

func TestToolUseStringLeaves_SkipsNonStringLeaves(t *testing.T) {
	input := map[string]any{
		"num":   float64(42),
		"flag":  true,
		"null":  nil,
		"text":  "keep-me",
		"arr":   []any{float64(1), "in-arr", true, nil},
		"empty": "",
	}
	got := ToolUseStringLeaves(input)
	// Sorted keys: arr, empty, flag, null, num, text.
	// arr -> only "in-arr" (number/bool/null skipped) -> ordinal 0
	// empty -> "" is still a string leaf -> ordinal 1
	// flag/null/num skipped
	// text -> "keep-me" -> ordinal 2
	want := []ToolLeaf{
		{Ordinal: 0, Value: "in-arr"},
		{Ordinal: 1, Value: ""},
		{Ordinal: 2, Value: "keep-me"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skip-non-string wrong: got %+v want %+v", got, want)
	}
}

func TestToolUseStringLeaves_EmptyAndNil(t *testing.T) {
	if got := ToolUseStringLeaves(nil); got != nil {
		t.Fatalf("nil input: got %+v want nil", got)
	}
	if got := ToolUseStringLeaves(map[string]any{}); got != nil {
		t.Fatalf("empty input: got %+v want nil", got)
	}
	// A map with only non-string leaves yields no leaves.
	if got := ToolUseStringLeaves(map[string]any{"n": float64(1), "b": false}); got != nil {
		t.Fatalf("non-string-only input: got %+v want nil", got)
	}
}
