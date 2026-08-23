package locator

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const pngBytes = "\x89PNG\r\n\x1a\n"

var pngB64 = base64.StdEncoding.EncodeToString([]byte(pngBytes))

func TestResolveEachContainerReturnsTheExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name, locator string
		body          string
		wantMime      string
	}{
		{
			"whole body", Body, pngBytes, "",
		},
		{
			"plain base64 at a path", JSON("data.0.b64_json"),
			`{"data":[{"b64_json":"` + pngB64 + `"}]}`, "",
		},
		{
			"data uri at a path", DataURI("messages.0.content.0.image_url.url"),
			`{"messages":[{"content":[{"image_url":{"url":"data:image/png;base64,` + pngB64 + `"}}]}]}`,
			"image/png",
		},
		{
			"data uri spelled with an uppercase scheme", DataURI("u"),
			`{"u":"DATA:image/png;base64,` + pngB64 + `"}`, "image/png",
		},
		{
			"data uri padded with a zero-width space", DataURI("u"),
			`{"u":"\u200bdata:image/png;base64,` + pngB64 + `"}`, "image/png",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve([]byte(tc.body), tc.locator)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if string(got.Bytes) != pngBytes {
				t.Fatalf("bytes = %q, want the stored artifact", got.Bytes)
			}
			if got.Mime != tc.wantMime {
				t.Fatalf("mime = %q, want %q", got.Mime, tc.wantMime)
			}
		})
	}
}

// The frame index a codec emits and the index this package counts must be the
// same number, or a streamed artifact serves the wrong frame's bytes while
// every other assertion still passes.
func TestResolveSSECountsDataBearingFramesInArrivalOrder(t *testing.T) {
	body := "data: " + `{"n":0}` + "\n\n" +
		": a comment frame carries no data\n\n" +
		"data: " + `{"n":1,"img":"` + pngB64 + `"}` + "\n\n" +
		"\n\n" + // an empty block is not a frame
		"data: [DONE]\n\n"

	frames := DataFrames([]byte(body))
	if len(frames) != 2 {
		t.Fatalf("want 2 data-bearing frames, got %d: %q", len(frames), frames)
	}

	got, err := Resolve([]byte(body), SSE(1, "img"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got.Bytes) != pngBytes {
		t.Fatalf("frame 1 served the wrong bytes: %q", got.Bytes)
	}
	// One past the end names a frame that is not there, rather than
	// silently serving the last one.
	if _, err := Resolve([]byte(body), SSE(2, "img")); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("frame 2 reason = %q, want %q", ReasonOf(err), ReasonNotFound)
	}
}

// A multi-line SSE frame concatenates its data lines, and the optional
// single space after the colon is grammar rather than payload.
func TestResolveSSEJoinsMultiLineFramesWithoutEatingPayload(t *testing.T) {
	half := len(pngB64) / 2
	body := "data: {\"img\":\"" + pngB64[:half] + "\n" +
		"data:" + pngB64[half:] + "\"}\n\n"

	got, err := Resolve([]byte(body), SSE(0, "img"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got.Bytes) != pngBytes {
		t.Fatalf("multi-line frame lost bytes: %q", got.Bytes)
	}
}

func TestResolveMultipartFindsThePartByName(t *testing.T) {
	body := "--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		"whisper-1\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n" +
		"Content-Type: audio/wav\r\n\r\n" +
		pngBytes + "\r\n" +
		"--BOUNDARY--\r\n"

	got, err := Resolve([]byte(body), Multipart("file"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got.Bytes) != pngBytes {
		t.Fatalf("part bytes = %q", got.Bytes)
	}
	if got.Mime != "audio/wav" {
		t.Fatalf("the part's declared type must be carried: %q", got.Mime)
	}
	// Addressed by field name, so a part that is not there is named as
	// missing rather than answered with whichever part came first.
	if _, err := Resolve([]byte(body), Multipart("image")); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("missing part reason = %q, want %q", ReasonOf(err), ReasonNotFound)
	}
}

// Every failure carries a reason a caller can branch on. Matching on
// message text is how a 404 becomes a 500 after a wording change.
func TestResolveFailuresCarryTheirReason(t *testing.T) {
	doc := []byte(`{"a":"not base64!!","n":7,"u":"data:image/png;base64,` + pngB64 + `"}`)
	for _, tc := range []struct{ name, locator, want string }{
		{"empty locator", "", ReasonMalformed},
		{"unknown container", "wat:a", ReasonUnsupported},
		{"empty path", JSON(""), ReasonMalformed},
		{"missing path", "json:nope", ReasonNotFound},
		{"non-string value", "json:n", ReasonUndecodable},
		{"not base64", "json:a", ReasonUndecodable},
		{"not a data uri", "datauri:a", ReasonUndecodable},
		{"sse without a path", "sse:0", ReasonMalformed},
		{"sse frame not a number", "sse:x:a", ReasonMalformed},
		{"multipart with no part", "multipart:", ReasonMalformed},
		{"multipart on a json body", Multipart("file"), ReasonUndecodable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(doc, tc.locator); ReasonOf(err) != tc.want {
				t.Fatalf("reason = %q, want %q (err=%v)", ReasonOf(err), tc.want, err)
			}
		})
	}
	if _, err := Resolve(nil, Body); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("an empty stored body is not-found, got %q", ReasonOf(err))
	}
}

// ValidBase64 decides whether a codec may promise bytes are recoverable, and
// Resolve decodes with StdEncoding. If those two ever disagree the promise
// is a Download that throws, so the agreement is asserted directly rather
// than assumed from the comment that claims it.
func TestValidBase64AgreesWithTheDecoderItDescribes(t *testing.T) {
	cases := []string{
		"", "QQ==", "QUJD", "QUJDQQ", "QUJD=", "QQ", "Q===", "QU-D", "QU_D",
		"QUJDQUJD", "////", "++++", "A+/=", "!!!!", "QUJ D",
		// Line-wrapped, which StdEncoding ignores and this predicate must
		// too — it was stricter than its own decoder until measured.
		"QUJD\n", "QU\r\nJD", "QUJD\nQUJD", "QUJ\n",
		// Not line breaks, and not ignored by the decoder either.
		"QUJD\u200b", "QUJD ", "QUJD\t",
	}
	for _, s := range cases {
		claimed := ValidBase64(s)
		_, err := base64.StdEncoding.DecodeString(s)
		decodes := err == nil && s != ""
		if claimed != decodes {
			t.Fatalf("ValidBase64(%q)=%v but StdEncoding decodes=%v — the promise and the decoder disagree",
				s, claimed, decodes)
		}
	}
}

// DecodedSize answers by arithmetic. Sizing a 20 MB payload must not
// allocate 15 MB to report a number.
func TestDecodedSizeMatchesTheRealDecodeLength(t *testing.T) {
	for n := range 64 {
		raw := strings.Repeat("x", n)
		b64 := base64.StdEncoding.EncodeToString([]byte(raw))
		if got := DecodedSize(b64); got != int64(n) {
			t.Fatalf("DecodedSize(%q) = %d, want %d", b64, got, n)
		}
	}
}

// An empty base propagates to an empty locator, which is how a codec says
// "these bytes exist but I cannot address them". Emitting a locator anyway
// would promise a download nothing could serve.
func TestEmptyBasePropagatesToAnEmptyLocator(t *testing.T) {
	if got := JoinPath("", 0); got != "" {
		t.Fatalf("JoinPath = %q", got)
	}
	if got := JoinSuffix("", "x"); got != "" {
		t.Fatalf("JoinSuffix = %q", got)
	}
	for name, got := range map[string]string{
		"json":      JSON(""),
		"datauri":   DataURI(""),
		"sse":       SSE(0, ""),
		"multipart": Multipart(""),
	} {
		if got != "" {
			t.Fatalf("%s from an empty path = %q, want empty", name, got)
		}
	}
	if got := SSE(-1, "a"); got != "" {
		t.Fatalf("a negative frame must not produce a locator, got %q", got)
	}
}

func TestTrimIgnorableCoversFormatCharactersNotJustWhitespace(t *testing.T) {
	for _, ch := range []string{" ", "\t", "\n", "\u0085", "\u00a0", "\u3000",
		"\ufeff", "\u200b", "\u200e", "\u00ad", "\u202a", "\u2060"} {
		if got := TrimIgnorable(ch + "data:x" + ch); got != "data:x" {
			t.Fatalf("TrimIgnorable did not strip %+q: %+q", ch, got)
		}
	}
	// And nothing meaningful is eaten.
	if got := TrimIgnorable("a b"); got != "a b" {
		t.Fatalf("interior content must survive: %q", got)
	}
}

func TestHasSchemeFoldReadsEveryLegalSpelling(t *testing.T) {
	for _, s := range []string{"data:x", "DATA:x", "Data:x", "dAtA:x"} {
		if !HasSchemeFold(s, "data:") {
			t.Fatalf("%q is a legal spelling of the scheme", s)
		}
	}
	for _, s := range []string{"", "dat", "data", "xdata:", "datax:"} {
		if HasSchemeFold(s, "data:") {
			t.Fatalf("%q is not the data scheme", s)
		}
	}
}

// The non-empty paths of the grammar constructors, and the string form of a
// failure — a caller that logs err.Error() should see the reason, not a
// pointer address.
func TestGrammarConstructorsAndErrorText(t *testing.T) {
	for name, got := range map[string]string{
		"JoinPath":   JoinPath("a", 3),
		"JoinSuffix": JoinSuffix("a", "b"),
		"JSON":       JSON("a.b"),
		"DataURI":    DataURI("a.b"),
		"SSE":        SSE(2, "a.b"),
		"Multipart":  Multipart("file"),
	} {
		if got == "" {
			t.Fatalf("%s produced an empty locator from a non-empty path", name)
		}
	}
	for want, got := range map[string]string{
		"a.3":            JoinPath("a", 3),
		"a.b":            JoinSuffix("a", "b"),
		"json:a.b":       JSON("a.b"),
		"datauri:a.b":    DataURI("a.b"),
		"sse:2:a.b":      SSE(2, "a.b"),
		"multipart:file": Multipart("file"),
	} {
		if got != want {
			t.Fatalf("locator = %q, want %q", got, want)
		}
	}

	err := failf(ReasonNotFound, "nothing at %q", "a.b")
	if s := err.Error(); s != `not-found: nothing at "a.b"` {
		t.Fatalf("error text = %q", s)
	}
	// A plain error is not a resolution failure and must not be reported as
	// one — a caller mapping reasons to status codes would send a 404 for an
	// unrelated database error.
	if got := ReasonOf(errors.New("boom")); got != "" {
		t.Fatalf("ReasonOf(plain) = %q, want empty", got)
	}
	if got := ReasonOf(nil); got != "" {
		t.Fatalf("ReasonOf(nil) = %q, want empty", got)
	}
}

// ParseDataURI's rejections, each for its own reason.
func TestParseDataURIRejectsWhatIsNotOne(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"wrong scheme", "http://x/a.png"},
		{"no comma", "data:image/png;base64"},
		{"not base64-encoded", "data:text/plain,hello"},
		{"scheme only", "data:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseDataURI(tc.in); ok {
				t.Fatalf("%q parsed as a base64 data URI", tc.in)
			}
		})
	}
	// A URI with no declared type is still a URI; the bytes decide what it
	// is, and the empty mime says "the wire did not tell us".
	mime, payload, ok := ParseDataURI("data:;base64,QUJD")
	if !ok || mime != "" || payload != "QUJD" {
		t.Fatalf("mime=%q payload=%q ok=%v", mime, payload, ok)
	}
}

// Resolution failures on shapes the happy-path tests never reach.
func TestResolveRemainingFailureShapes(t *testing.T) {
	// A data URI whose payload is present but not base64.
	if _, err := Resolve([]byte(`{"u":"data:image/png;base64,!!!!"}`), DataURI("u")); ReasonOf(err) != ReasonUndecodable {
		t.Fatalf("reason = %q", ReasonOf(err))
	}
	// A data URI with an empty payload resolves to nothing, and says so
	// rather than serving zero bytes as a file.
	if _, err := Resolve([]byte(`{"u":"data:image/png;base64,"}`), DataURI("u")); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("empty payload reason = %q", ReasonOf(err))
	}
	// A negative frame index is malformed, not a missing frame.
	if _, err := Resolve([]byte("data: {}\n\n"), "sse:-1:a"); ReasonOf(err) != ReasonMalformed {
		t.Fatalf("negative frame reason = %q", ReasonOf(err))
	}
	// An sse path that addresses nothing inside a frame that does exist.
	body := []byte("data: " + `{"a":1}` + "\n\n")
	if _, err := Resolve(body, SSE(0, "missing")); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("missing sse path reason = %q", ReasonOf(err))
	}
	// A multipart body whose opening delimiter names no boundary.
	if _, err := Resolve([]byte("--\r\nx"), Multipart("file")); ReasonOf(err) != ReasonUndecodable {
		t.Fatalf("empty boundary reason = %q", ReasonOf(err))
	}
	// A body that is only a delimiter line, with no parts at all.
	if _, err := Resolve([]byte("--B\r\n"), Multipart("file")); ReasonOf(err) != ReasonNotFound {
		t.Fatalf("no-parts reason = %q", ReasonOf(err))
	}
}

// A multipart part that declares no Content-Type carries no mime hint,
// rather than inventing one.
func TestResolveMultipartWithoutADeclaredType(t *testing.T) {
	body := "--B\r\n" +
		"Content-Disposition: form-data; name=\"file\"\r\n\r\n" +
		pngBytes + "\r\n--B--\r\n"
	got, err := Resolve([]byte(body), Multipart("file"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Mime != "" {
		t.Fatalf("mime = %q, want empty when the part declares none", got.Mime)
	}
	if string(got.Bytes) != pngBytes {
		t.Fatalf("bytes = %q", got.Bytes)
	}
}

// The EventSource grammar permits CRLF, and this walker split on a literal
// "\n\n" while the codec's own fold and the browser resolver both dispatched
// on a blank line. Two CRLF frames merged into one here, so a codec that had
// counted two emitted `sse:1:…` for a resolver that could only see frame 0:
// a captured ref whose Download 404s, on the one container whose index is
// COMPUTED rather than concatenated.
func TestDataFramesCountsCRLFFramesTheSameWayTheCodecDoes(t *testing.T) {
	for _, tc := range []struct{ name, sep string }{
		{"LF", "\n\n"},
		{"CRLF", "\r\n\r\n"},
		{"mixed", "\r\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: " + `{"n":0}` + tc.sep + "data: " + `{"n":1,"img":"` + pngB64 + `"}` + tc.sep
			frames := DataFrames([]byte(body))
			if len(frames) != 2 {
				t.Fatalf("want 2 frames, got %d: %q", len(frames), frames)
			}
			got, err := Resolve([]byte(body), SSE(1, "img"))
			if err != nil {
				t.Fatalf("frame 1 must resolve: %v", err)
			}
			if string(got.Bytes) != pngBytes {
				t.Fatalf("frame 1 served the wrong bytes")
			}
		})
	}

	// A CR-terminated data line must not carry the CR into the payload —
	// base64 with a trailing CR is still decodable, but the frame's JSON is
	// not, so this fails loudly rather than subtly.
	body := "data: " + `{"img":"` + pngB64 + `"}` + "\r\n\r\n"
	if _, err := Resolve([]byte(body), SSE(0, "img")); err != nil {
		t.Fatalf("a CR-terminated data line must parse: %v", err)
	}
}
