package redact

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// The three branches of the provider-error extractor. This is NOT the whole
// field — see TestClassifyComplianceError_EveryArmIsBounded, which is the
// test that actually pins the claim "the bound belongs to the field". This
// one passing while two other producers wrote unbounded text is exactly how
// that claim survived being false.
func TestProviderErrorMessage_EveryBranchIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)

	cases := []struct {
		name string
		body string
	}{
		{"structured error.message", `{"error":{"message":"` + long + `"}}`},
		{"top-level message", `{"message":"` + long + `"}`},
		{"unstructured raw body", long},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProviderErrorMessage([]byte(tc.body), 500, true)
			if len(got) > MaxErrorReasonBytes+3 { // +3 for the ellipsis
				t.Errorf("%s produced %d bytes; a provider chooses this string and it "+
					"lands in traffic_event.error_reason unbounded", tc.name, len(got))
			}
			if !strings.HasSuffix(got, "...") {
				t.Errorf("a truncated reason must say so; got tail %q", tail(got))
			}
		})
	}
}

// Slicing mid-rune yields invalid UTF-8, which can fail a downstream JSON
// encode and turn a bounded error string into a DROPPED event — worse than
// the unbounded string it replaced.
func TestProviderErrorMessage_TruncatesOnARuneBoundary(t *testing.T) {
	// The single "x" is load-bearing. Every multi-byte width divides 300
	// exactly (300 % 2 == 300 % 3 == 300 % 4 == 0), so a run of one rune type
	// puts the naive cut ON a boundary and the test passes against the very
	// bug it exists to catch — the first draft did exactly that. The ASCII
	// prefix shifts the run to offset 1, where a 3-byte rune straddles 300.
	body := `{"error":{"message":"x` + strings.Repeat("→", 400) + `"}}`
	got := ProviderErrorMessage([]byte(body), 500, true)
	trimmed := strings.TrimSuffix(got, "...")
	if !utf8.ValidString(trimmed) {
		t.Errorf("truncation split a rune and produced invalid UTF-8: %q", trimmed)
	}
	// Non-vacuity: it must actually have truncated, or valid-UTF-8 proves
	// nothing about the boundary logic.
	if len(trimmed) == 0 || utf8.RuneCountInString(trimmed) >= 400 {
		t.Errorf("nothing was truncated (%d runes); this test cannot show a boundary bug",
			utf8.RuneCountInString(trimmed))
	}
}

func tail(s string) string {
	if len(s) <= 20 {
		return s
	}
	return s[len(s)-20:]
}

// A provider chooses these bytes, so they can simply not be UTF-8 — and an
// invalid sequence in a text column is not cosmetic: PostgreSQL rejects the
// INSERT and the audit consumer treats that rejection as permanent, dropping
// the event. Rune-aligned cutting alone does not cover it, because the input
// can already be invalid before any cut.
func TestBoundErrorReason_AlwaysReturnsValidUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"all continuation bytes, over the cap", bytes.Repeat([]byte{0x80}, 400)},
		{"valid prefix then continuation bytes", append([]byte("abcde"), bytes.Repeat([]byte{0x80}, 400)...)},
		{"leading continuation byte then ascii", append([]byte{0x80}, bytes.Repeat([]byte("a"), 400)...)},
		{"invalid and UNDER the cap", bytes.Repeat([]byte{0xff}, 300)},
		{"valid ascii then a lone lead byte, under the cap", append(bytes.Repeat([]byte("a"), 299), 0xe2)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BoundErrorReason(tc.in)
			if !utf8.ValidString(got) {
				t.Errorf("returned invalid UTF-8 (%d bytes): %q", len(got), got)
			}
			if len(got) > MaxErrorReasonBytes*4+3 {
				t.Errorf("repair blew the bound: %d bytes", len(got))
			}
			// Non-vacuity: a bare "..." means the message was annihilated
			// rather than bounded, which is what a walk-to-zero does.
			if strings.TrimSuffix(got, "...") == "" {
				t.Errorf("the whole message was thrown away; got %q", got)
			}
		})
	}
}

// The fallback branch is reached when the body is neither known error shape —
// an HTML error page from a CDN, WAF or nginx, which can be megabytes — and
// it runs on the request goroutine during a 4xx/5xx storm. Converting the
// body to a string before cutting allocated the whole thing to keep 300
// bytes. The assertion is on ALLOCATED BYTES, not on ns/op: the wall clock
// here is dominated by gjson scanning the body twice, which would hide a
// megabyte of garbage behind noise.
func BenchmarkProviderErrorMessage_LargeNonJSONBody(b *testing.B) {
	body := bytes.Repeat([]byte("<html><body>502 Bad Gateway</body></html>"), 30000)
	if len(body) < 1_000_000 {
		b.Fatalf("the fixture must be large enough for a full copy to show: %d bytes", len(body))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := ProviderErrorMessage(body, 502, true); len(got) != MaxErrorReasonBytes+3 {
			b.Fatalf("fixture stopped exercising the fallback: got %d bytes", len(got))
		}
	}
}

// The branch a real provider almost always takes, and therefore the one that
// must not have paid for bounding the others. gjson already copies the token
// out of the body, so at or under the cap BoundErrorReason hands that same
// string straight back.
func BenchmarkProviderErrorMessage_TypicalStructuredError(b *testing.B) {
	body := []byte(`{"error":{"message":"model 'gpt-4o' does not exist or you do not have access to it","type":"invalid_request_error","code":"model_not_found"}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := ProviderErrorMessage(body, 404, true); len(got) == 0 {
			b.Fatal("fixture stopped exercising the structured branch")
		}
	}
}

// The benchmarks above are for humans; this is the gate. A benchmark cannot
// fail, so the regression it measures can be reintroduced against a green
// suite — which is how `BoundErrorReason(string(body))` shipped in the first
// place: every test stayed green while the fallback started copying the whole
// body to keep 300 bytes of it.
//
// The assertion is on BYTES, not on allocation COUNT: the regression was two
// allocations either way, one of them a megabyte. Count-based tools
// (testing.AllocsPerRun) are blind to it.
//
// TotalAlloc is exact accounting, not a timing measurement, so this does not
// belong to the class of load-dependent flakes — but the budget is set two
// orders of magnitude above the real figure so that an unrelated allocation
// nearby can never trip it, while a full-body copy always does.
func TestProviderErrorMessage_FallbackDoesNotCopyTheWholeBody(t *testing.T) {
	body := bytes.Repeat([]byte("<html><body>502 Bad Gateway</body></html>"), 30000)
	if len(body) < 1_000_000 {
		t.Fatalf("the fixture must dwarf the budget for this to prove anything: %d bytes", len(body))
	}

	const runs = 100
	const budgetPerOp = 64 << 10 // 64 KiB; measured ~640 B, a full copy is ~1.2 MB

	var sink string
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		sink = ProviderErrorMessage(body, 502, true)
	}
	runtime.ReadMemStats(&after)

	if len(sink) != MaxErrorReasonBytes+3 {
		t.Fatalf("fixture stopped exercising the fallback branch: got %d bytes", len(sink))
	}
	perOp := (after.TotalAlloc - before.TotalAlloc) / runs
	if perOp > budgetPerOp {
		t.Errorf("the fallback allocated %d bytes/op against a %d budget; the body is %d bytes, "+
			"so this is the whole thing being materialised to keep %d of it — on the request "+
			"goroutine, during a 4xx/5xx storm",
			perOp, budgetPerOp, len(body), MaxErrorReasonBytes)
	}
}

// The envelope shapes, asserted by exact value. They live with the
// implementation: beside one caller's copy of the function instead, a second
// copy in the ai-gateway goes untested.

func TestProviderErrorMessage_EmptyBodyFallsBackToStatus(t *testing.T) {
	got := ProviderErrorMessage(nil, 503, true)
	if got != "provider returned HTTP 503" {
		t.Errorf("got %q", got)
	}
}

func TestProviderErrorMessage_OpenAIShape(t *testing.T) {
	body := []byte(`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`)
	if got := ProviderErrorMessage(body, 429, true); got != "insufficient quota" {
		t.Errorf("got %q", got)
	}
}

func TestProviderErrorMessage_TopLevelMessage(t *testing.T) {
	body := []byte(`{"message":"bad request"}`)
	if got := ProviderErrorMessage(body, 400, true); got != "bad request" {
		t.Errorf("got %q", got)
	}
}

func TestProviderErrorMessage_FallsBackToRawBody(t *testing.T) {
	body := []byte(`<html>upstream is down</html>`)
	got := ProviderErrorMessage(body, 502, true)
	if got != "<html>upstream is down</html>" {
		t.Errorf("got %q", got)
	}
}

func TestProviderErrorMessage_LongBodyTruncated(t *testing.T) {
	body := make([]byte, 400)
	for i := range body {
		body[i] = 'a'
	}
	got := ProviderErrorMessage(body, 502, true)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long body should end with ellipsis: %q", got[len(got)-20:])
	}
	if len(got) != 303 {
		t.Errorf("truncated length: %d, want 303 (300 + '...')", len(got))
	}
}
