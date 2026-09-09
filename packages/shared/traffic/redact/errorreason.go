package redact

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// MaxErrorReasonBytes bounds every string that becomes
// traffic_event.error_reason.
const MaxErrorReasonBytes = 300

// ProviderErrorMessage extracts a human-readable error message from a provider
// response body. It handles the JSON envelope OpenAI, Anthropic and Gemini all
// use (.error.message, or a top-level .message), optionally falls back to the
// raw body, and returns a generic "provider returned HTTP <N>" when there is
// nothing it may quote.
//
// It lives here rather than in either audit writer because both of them write
// the same column, and two copies drift exactly as you would expect: one gets
// fixed, the other keeps returning the provider's string unbounded and cutting
// the raw body mid-rune, on the higher-traffic service. One implementation is
// the point.
//
// allowRawQuote governs ONLY the last branch, and it is a parameter rather than
// a caller-side decision because that is the only way a future caller cannot
// forget it. The first two branches read a provider-authored, already-bounded
// diagnostic string — "You exceeded your current quota", "model_not_found" —
// which is what this column exists to carry and which no payload policy is
// about. The last branch copies up to 300 bytes of whatever the upstream sent:
// a WAF or CDN page, or a text/plain body echoing the caller's own request
// back. Those are response-body bytes, and an operator who turned payload
// capture off excluded them.
//
// So a caller holding bytes the storage gate already approved passes true; a
// caller holding raw upstream bytes passes whatever that gate says. Nilling the
// body instead — which is what the first attempt at this did — silences all
// three branches and leaves every failure reading "provider returned HTTP 400"
// on the default configuration, where StoreResponseBody is false.
func ProviderErrorMessage(body []byte, statusCode int, allowRawQuote bool) string {
	if len(body) == 0 {
		return fmt.Sprintf("provider returned HTTP %d", statusCode)
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return BoundErrorReason(msg)
	}
	if msg := gjson.GetBytes(body, "message").String(); msg != "" {
		return BoundErrorReason(msg)
	}
	if !allowRawQuote {
		// Neither structured shape matched, so anything left to say would be a
		// verbatim quote of the response body — which this caller may not keep.
		// The status line still tells an operator what happened.
		return fmt.Sprintf("provider returned HTTP %d", statusCode)
	}
	// The BYTES, not string(body). This branch is reached when the body is
	// neither shape — a CDN, WAF or nginx HTML error page, which can be
	// megabytes — and converting before cutting materialises the whole thing
	// to throw 99.97% of it away. Measured on a 1.2 MB body: 320 B/op against
	// 1,204,548 B/op, unbounded and linear in whatever the upstream sent. Both
	// callers run this on the request goroutine, and this branch is reached
	// exactly during a 4xx/5xx storm — the peak-rate moment.
	return BoundErrorReason(body)
}

// BoundErrorReason truncates v to MaxErrorReasonBytes on a RUNE boundary and
// guarantees the result is valid UTF-8.
//
// Generic over string and []byte so a caller never has to convert a large body
// just to bound it; see ProviderErrorMessage's fallback.
//
// Both properties are load-bearing. Invalid UTF-8 in a text column is not a
// cosmetic problem: PostgreSQL rejects the INSERT, and the audit consumer
// classifies that class of rejection as permanent and DROPS the event — a
// worse outcome than the unbounded string this replaced. Slicing mid-rune
// manufactures exactly that, and a provider can also just send it: the input
// is bytes chosen by someone else. So the cut is rune-aligned AND the result
// is repaired, rather than trusting either alone.
func BoundErrorReason[T ~string | ~[]byte](v T) string {
	if len(v) <= MaxErrorReasonBytes {
		return validUTF8(string(v))
	}
	cut := MaxErrorReasonBytes
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	if cut == 0 {
		// A run of continuation bytes (undecompressed gzip, protobuf) walks
		// the whole way down and would annihilate the message — 400 bytes of
		// 0x80 returned a bare "...". Keep the flat cut; validUTF8 repairs
		// the partial rune it leaves.
		cut = MaxErrorReasonBytes
	}
	return validUTF8(string(v[:cut])) + "..."
}

// validUTF8 replaces invalid byte sequences with U+FFFD, allocating only when
// there is something to repair. The scan is over at most MaxErrorReasonBytes,
// so the common (valid) path costs one pass and no allocation.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}
