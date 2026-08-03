package proxy

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExtractEndUserID pins the single-source contract: the tag comes from
// X-Nexus-End-User-Id and from nowhere else, so a caller's traffic is filed
// under the identifier they declared to Nexus rather than one inferred from
// whichever wire shape they happen to speak.
func TestExtractEndUserID(t *testing.T) {
	h := func(v string) http.Header {
		hdr := http.Header{}
		if v != "" {
			hdr.Set("X-Nexus-End-User-Id", v)
		}
		return hdr
	}

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"header declared", "cust-1", "cust-1"},
		{"header whitespace trimmed", "  cust-2  ", "cust-2"},
		{"header absent", "", ""},
		{"header present but blank", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractEndUserID(h(tc.header)); got != tc.want {
				t.Errorf("extractEndUserID = %q, want %q", got, tc.want)
			}
		})
	}
}

// A provider's native end-user field is NOT a source. Each body below
// carries the field that identifies the caller's end user to the upstream
// provider; none of them may reach traffic_event.end_user_id, because that
// column answers a different question — who this traffic belongs to across
// the Nexus product family — and nobody chose those values for it.
func TestExtractEndUserID_IgnoresProviderNativeFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-4o","user":"openai-end-user"}`,
		`{"model":"gpt-4o","safety_identifier":"openai-end-user"}`,
		`{"model":"claude","metadata":{"user_id":"anthropic-end-user"}}`,
	} {
		req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if got := extractEndUserID(req.Header); got != "" {
			t.Errorf("body %s attributed %q; the header is the only source", body, got)
		}
	}
}

// TestExtractEndUserID_CapsLength pins the storage bound: an oversized
// value is cut to endUserMaxBytes without leaving a torn UTF-8 rune, so
// an abusive caller cannot inflate traffic_event rows and the stored
// prefix remains valid text.
func TestExtractEndUserID_CapsLength(t *testing.T) {
	long := strings.Repeat("a", 300)
	hdrLong := http.Header{}
	hdrLong.Set("X-Nexus-End-User-Id", long)
	got := extractEndUserID(hdrLong)
	if len(got) != endUserMaxBytes {
		t.Errorf("len = %d, want %d", len(got), endUserMaxBytes)
	}

	// Multi-byte value cut mid-rune must shrink to the last whole rune.
	cn := strings.Repeat("用", 100) // 3 bytes each → 300 bytes
	hdr := http.Header{}
	hdr.Set("X-Nexus-End-User-Id", cn)
	got = extractEndUserID(hdr)
	if !utf8.ValidString(got) {
		t.Fatalf("capped value is not valid UTF-8")
	}
	if len(got) > endUserMaxBytes {
		t.Errorf("len = %d, want <= %d", len(got), endUserMaxBytes)
	}
	if len(got) != 255 { // 256 does not divide by 3; last torn rune dropped
		t.Errorf("len = %d, want 255 (85 whole runes)", len(got))
	}
}

// TestExtractSessionID pins the session tag's header-only contract: value
// trimmed, capped, and no body field is ever consulted — a session is an
// application-level concept the caller names explicitly.
func TestExtractSessionID(t *testing.T) {
	hdr := http.Header{}
	if got := extractSessionID(hdr); got != "" {
		t.Errorf("absent header: got %q, want empty", got)
	}
	hdr.Set("X-Nexus-Session-Id", "  conv-42  ")
	if got := extractSessionID(hdr); got != "conv-42" {
		t.Errorf("got %q, want trimmed conv-42", got)
	}
	hdr.Set("X-Nexus-Session-Id", strings.Repeat("s", 300))
	if got := extractSessionID(hdr); len(got) != endUserMaxBytes {
		t.Errorf("len = %d, want capped at %d", len(got), endUserMaxBytes)
	}
}
