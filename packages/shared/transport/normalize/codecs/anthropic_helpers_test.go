package codecs

import "testing"

func TestIntFromAny_AllTypes(t *testing.T) {
	// JSON decoders deliver numeric values as float64, but Go callers
	// may pass int / int64 directly. The helper centralizes the cast
	// so anthropic_messages.go (and downstream callers) handle every
	// shape uniformly — without this, a refactor that adds a new
	// numeric path could silently miss one of the existing types.
	cases := []struct {
		in   any
		want int
	}{
		{float64(42.0), 42},
		{float64(42.9), 42}, // truncates
		{int(7), 7},
		{int64(99), 99},
		{"42", 0},     // not a number
		{nil, 0},      // not a number
		{true, 0},     // not a number
		{int32(5), 0}, // int32 is NOT in the switch — pins this
	}
	for _, c := range cases {
		if got := intFromAny(c.in); got != c.want {
			t.Errorf("intFromAny(%T %v): got %d want %d", c.in, c.in, got, c.want)
		}
	}
}
