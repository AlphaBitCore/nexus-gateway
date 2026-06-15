package restable

import (
	"strings"
	"testing"
)

// TestSanitizeTerminal covers the terminal-injection defense (F-0293): control
// sequences a server could embed in a cell or error body must be stripped, while
// legitimate printable text plus tab/newline survive.
func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain printable preserved", "hello world", "hello world"},
		{"unicode preserved", "café — naïve 日本語", "café — naïve 日本語"},
		{"tab preserved", "a\tb", "a\tb"},
		{"newline preserved", "line1\nline2", "line1\nline2"},
		{"carriage return stripped", "over\rwrite", "overwrite"},
		{"bare ESC stripped", "a\x1bb", "ab"},
		{"empty stays empty", "", ""},
		{
			name: "SGR color sequence stripped, text kept",
			in:   "\x1b[31mRED\x1b[0m text",
			want: "RED text",
		},
		{
			name: "cursor-move CSI stripped",
			in:   "before\x1b[2Jafter",
			want: "beforeafter",
		},
		{
			name: "OSC 52 clipboard write stripped (BEL terminated)",
			in:   "x\x1b]52;c;ZXZpbA==\x07y",
			want: "xy",
		},
		{
			name: "OSC title-set stripped (BEL terminated)",
			in:   "name\x1b]0;pwned title\x07end",
			want: "nameend",
		},
		{
			name: "OSC terminated by ST (ESC backslash) stripped",
			in:   "p\x1b]52;c;ZA==\x1b\\q",
			want: "pq",
		},
		{
			name: "charset-select two-byte escape stripped",
			in:   "a\x1b(Bb",
			want: "ab",
		},
		{
			name: "C1 control byte stripped",
			in:   "a\x9bb",
			want: "ab",
		},
		{
			name: "DEL stripped",
			in:   "a\x7fb",
			want: "ab",
		},
		{
			name: "NUL and other C0 stripped",
			in:   "a\x00\x01\x02b",
			want: "ab",
		},
		{
			name: "trailing lone ESC dropped",
			in:   "tail\x1b",
			want: "tail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeTerminal(tc.in); got != tc.want {
				t.Errorf("SanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCellStringSanitizesServerString proves CellString runs the sanitizer on a
// string cell value (so a malicious record field cannot inject escapes), while
// non-string machine-formatted values are unaffected.
func TestCellStringSanitizesServerString(t *testing.T) {
	got := CellString("\x1b]52;c;ZXZpbA==\x07node-a")
	if strings.Contains(got, "\x1b") || strings.Contains(got, "52;c") {
		t.Errorf("CellString left an escape in the cell: %q", got)
	}
	if got != "node-a" {
		t.Errorf("CellString = %q, want %q", got, "node-a")
	}
}
