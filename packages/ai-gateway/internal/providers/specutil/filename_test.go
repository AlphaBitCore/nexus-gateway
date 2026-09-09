package specutil

import "testing"

// The name this supplies travels upstream on a real request, so the two things
// that matter are that it is never empty and that it never claims a type the
// bytes are not.
func TestFilenameForMime(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"application/pdf", "attachment.pdf"},
		{"APPLICATION/PDF", "attachment.pdf"},
		{"  application/pdf  ", "attachment.pdf"},
		{"text/plain", "attachment.txt"},
		{"text/markdown", "attachment.md"},
		{"text/csv", "attachment.csv"},
		{"application/json", "attachment.json"},
		{"text/html", "attachment.html"},
		{"application/xml", "attachment.xml"},
		{"application/msword", "attachment.doc"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "attachment.docx"},
		{"application/vnd.ms-excel", "attachment.xls"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "attachment.xlsx"},

		// An unrecognised type gets no extension rather than a guessed one: a
		// reader trusts the extension, so a wrong one is worse than none.
		{"application/x-nexus-unknown", "attachment"},
		{"", "attachment"},
	}
	for _, tc := range cases {
		if got := FilenameForMime(tc.mime); got != tc.want {
			t.Errorf("FilenameForMime(%q) = %q, want %q", tc.mime, got, tc.want)
		}
	}
}

// TestFilenameForMimeIsNeverEmpty pins the property the whole fill exists for:
// an empty name is the 400 it was added to prevent.
func TestFilenameForMimeIsNeverEmpty(t *testing.T) {
	for _, mime := range []string{"", "application/pdf", "nonsense", "//", "text/"} {
		if FilenameForMime(mime) == "" {
			t.Errorf("FilenameForMime(%q) returned an empty name; OpenAI rejects file_data "+
				"without one, which is the failure this function exists to prevent", mime)
		}
	}
}

// TestFilenameForMimeIsStable pins determinism: the same request must encode
// identically on every attempt, or the cache key moves under it.
func TestFilenameForMimeIsStable(t *testing.T) {
	first := FilenameForMime("application/pdf")
	for range 100 {
		if got := FilenameForMime("application/pdf"); got != first {
			t.Fatalf("FilenameForMime is not deterministic: %q then %q", first, got)
		}
	}
}
