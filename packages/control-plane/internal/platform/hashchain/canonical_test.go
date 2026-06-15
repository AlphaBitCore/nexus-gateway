package hashchain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalize_SortsKeysAtEveryLevel(t *testing.T) {
	in := `{"b":1,"a":{"z":2,"m":3},"c":[{"y":1,"x":2}]}`
	got, err := Canonicalize(json.RawMessage(in))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := `{"a":{"m":3,"z":2},"b":1,"c":[{"x":2,"y":1}]}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestCanonicalize_MapIterationIndependence(t *testing.T) {
	// The same logical document presented with different physical key orders
	// must canonicalize identically (the memo-identity property, FR-16).
	a := `{"one":1,"two":2,"three":3,"four":4,"five":5}`
	b := `{"five":5,"four":4,"three":3,"two":2,"one":1}`
	ca, err1 := Canonicalize(json.RawMessage(a))
	cb, err2 := Canonicalize(json.RawMessage(b))
	if err1 != nil || err2 != nil {
		t.Fatalf("errs: %v %v", err1, err2)
	}
	if string(ca) != string(cb) {
		t.Fatalf("key-order changed canonical bytes:\n%s\n%s", ca, cb)
	}
}

func TestCanonicalize_NumberStability(t *testing.T) {
	cases := []struct{ in, want string }{
		{`1`, `1`},
		{`1.0`, `1`},
		{`1e0`, `1`},
		{`1.5`, `1.5`},
		{`-0`, `0`},
		{`100`, `100`},
		{`1000000`, `1000000`},
		{`0.1`, `0.1`},
		{`2.5e3`, `2500`},
		{`1.25e-3`, `0.00125`},
		{`{"a":1.0,"b":2}`, `{"a":1,"b":2}`},
	}
	for _, tc := range cases {
		got, err := Canonicalize(json.RawMessage(tc.in))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("Canonicalize(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalize_EqualNumbersIdentical(t *testing.T) {
	// 1, 1.0, 1e0 are equal numbers → identical canonical bytes (so a node
	// that emits 1.0 on one run and 1 on the next does not look "changed" to
	// the FR-27 backpressure decision).
	forms := []string{`{"n":1}`, `{"n":1.0}`, `{"n":1e0}`}
	var first string
	for i, f := range forms {
		got, err := Canonicalize(json.RawMessage(f))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", f, err)
		}
		if i == 0 {
			first = string(got)
			continue
		}
		if string(got) != first {
			t.Fatalf("%s canonicalized to %s, want %s", f, got, first)
		}
	}
}

func TestCanonicalize_StringEscapingMinimalNoHTML(t *testing.T) {
	got, err := Canonicalize(json.RawMessage(`{"k":"a<b>&c\"d"}`))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := `{"k":"a<b>&c\"d"}`
	if string(got) != want {
		t.Fatalf("got %s, want %s (HTML chars must not be \\u-escaped)", got, want)
	}
}

func TestCanonicalize_Primitives(t *testing.T) {
	cases := map[string]string{
		`null`:         `null`,
		`true`:         `true`,
		`false`:        `false`,
		`"x"`:          `"x"`,
		`[]`:           `[]`,
		`{}`:           `{}`,
		`[1,"a",true]`: `[1,"a",true]`,
	}
	for in, want := range cases {
		got, err := Canonicalize(json.RawMessage(in))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", in, err)
		}
		if string(got) != want {
			t.Errorf("Canonicalize(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestCanonicalize_Errors(t *testing.T) {
	cases := []struct{ name, in string }{
		{"malformed", `{not json`},
		{"trailing-data", `{} {}`},
		{"empty", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonicalize(json.RawMessage(tc.in)); err == nil {
				t.Fatalf("Canonicalize(%q) succeeded, want error", tc.in)
			}
		})
	}
}

func TestCanonicalize_DeepNesting(t *testing.T) {
	deep := strings.Repeat(`{"a":`, maxDepth+5) + `1` + strings.Repeat(`}`, maxDepth+5)
	_, err := Canonicalize(json.RawMessage(deep))
	if !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("err = %v, want ErrNonCanonical for over-deep nesting", err)
	}
}

func TestCanonicalize_ErrorPropagationThroughContainers(t *testing.T) {
	// A non-finite number nested inside an object and inside an array must
	// propagate the error out, not get silently dropped.
	if _, err := Canonicalize(json.RawMessage(`{"a":{"b":1e400}}`)); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("nested-in-object: err = %v, want ErrNonCanonical", err)
	}
	if _, err := Canonicalize(json.RawMessage(`[1,[1e400]]`)); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("nested-in-array: err = %v, want ErrNonCanonical", err)
	}
}

func TestCanonicalize_DeepArrayNesting(t *testing.T) {
	deep := strings.Repeat(`[`, maxDepth+5) + `1` + strings.Repeat(`]`, maxDepth+5)
	if _, err := Canonicalize(json.RawMessage(deep)); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("err = %v, want ErrNonCanonical for over-deep array", err)
	}
}

func TestCanonicalize_BoolAndNullInStructure(t *testing.T) {
	got, err := Canonicalize(json.RawMessage(`{"t":true,"f":false,"n":null}`))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if string(got) != `{"f":false,"n":null,"t":true}` {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalize_NonFiniteNumberToken(t *testing.T) {
	// json.Decoder with UseNumber will accept "1e400" (parses to +Inf via
	// strconv) — must reject as non-canonical.
	_, err := Canonicalize(json.RawMessage(`1e400`))
	if !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("err = %v, want ErrNonCanonical for overflow number", err)
	}
}
