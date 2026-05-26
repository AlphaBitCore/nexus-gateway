package traffic

import (
	"testing"
	"time"
)

// TestFormatCSVTimestamp verifies the shape rendered for the `timestamp`
// column of the compliance CSV export. MatrixAuditRow.Timestamp is typed
// `any` because pgx can hand back either time.Time or a pre-formatted
// string depending on column type; the CSV must stay parseable in either
// case — Excel / Google Sheets only recognise RFC3339 as a date.
func TestFormatCSVTimestamp(t *testing.T) {
	fixed := time.Date(2026, 4, 22, 8, 14, 18, 784_000_000, time.UTC)

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"time.Time renders as RFC3339Nano UTC", fixed, "2026-04-22T08:14:18.784Z"},
		{"already-formatted string passes through", "2026-04-22T08:14:18.784Z", "2026-04-22T08:14:18.784Z"},
		{"nil renders as empty string", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatCSVTimestamp(tc.in); got != tc.want {
				t.Errorf("formatCSVTimestamp(%v) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDerefString pins the nil-safety contract relied on by every optional
// string column in the compliance CSV export.
func TestDerefString(t *testing.T) {
	s := "value"
	if got := derefString(&s); got != "value" {
		t.Errorf("derefString(&%q) = %q; want %q", s, got, "value")
	}
	if got := derefString(nil); got != "" {
		t.Errorf("derefString(nil) = %q; want empty", got)
	}
}

// TestFormatOptionalInt pins the nil-safety contract for optional numeric
// columns (statusCode, latencyMs) — empty cell beats "0" so consumers can
// distinguish "absent" from "zero".
func TestFormatOptionalInt(t *testing.T) {
	n := 200
	zero := 0
	if got := formatOptionalInt(&n); got != "200" {
		t.Errorf("formatOptionalInt(&200) = %q; want %q", got, "200")
	}
	if got := formatOptionalInt(&zero); got != "0" {
		t.Errorf("formatOptionalInt(&0) = %q; want %q", got, "0")
	}
	if got := formatOptionalInt(nil); got != "" {
		t.Errorf("formatOptionalInt(nil) = %q; want empty", got)
	}
}
