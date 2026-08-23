package specutil_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// The union of the two per-codec suites this replaces, plus the case that
// decided which of their two semantics survived. Every row states what a codec
// would do with the verdict, because ok=false is not a detail — it is the
// difference between refusing a caller's body with a reason and shipping bytes
// the upstream will reject.
func TestParseDataURL(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("hello"))

	for _, tc := range []struct {
		name      string
		in        string
		wantOK    bool
		wantMedia string
		wantB64   string
		why       string
	}{
		{
			name:   "a well-formed data URL yields its media type and payload",
			in:     "data:image/png;base64," + valid,
			wantOK: true, wantMedia: "image/png", wantB64: valid,
			why: "the ordinary path every inline image and file takes",
		},
		{
			name:   "an absent media type defaults rather than emptying",
			in:     "data:;base64," + valid,
			wantOK: true, wantMedia: "application/octet-stream", wantB64: valid,
			why: "the data: scheme's own default; an empty media_type would be rejected by the wire",
		},
		{
			name: "a remote URL is not a data URL",
			in:   "https://example.com/image.png",
			why:  "the caller sent a reference, not bytes; the codec takes its URL branch instead",
		},
		{
			name: "no comma means no payload boundary",
			in:   "data:image/png;base64",
			why:  "unparseable, and guessing where the payload starts would invent bytes",
		},
		{
			name: "a meta section without ;base64 is a form we do not decode",
			in:   "data:image/png," + valid,
			why:  "percent-encoded and plain data: URLs exist; accepting one as base64 would corrupt it",
		},
		{
			name: "a trailing comma carries no payload",
			in:   "data:image/png;base64,",
			why:  "an empty payload is a file with no bytes",
		},
		{
			name: "an undecodable payload is refused",
			in:   "data:image/png;base64,not valid base64!!!",
			why:  "the wire would reject it; refusing here names the field instead",
		},
		{
			name:   "padding-stripped base64 is normalized rather than refused",
			in:     "data:image/png;base64," + strings.TrimRight(valid, "="),
			wantOK: true, wantMedia: "image/png", wantB64: valid,
			why: "the case the two former copies disagreed on, and neither answer was right. " +
				"api.anthropic.com answers 400 for this body while generativelanguage.googleapis.com " +
				"answers 200 AND reads the image, so refusing imposes one vendor's rule on the " +
				"other. Re-emitting it padded is accepted by both",
		},
		{
			name:   "the URL-safe alphabet is normalized to the standard one",
			in:     "data:image/png;base64," + base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xbf}),
			wantOK: true, wantMedia: "image/png",
			wantB64: base64.StdEncoding.EncodeToString([]byte{0xfb, 0xff, 0xbf}),
			why:     "bytes that differ between the two alphabets; Gemini decodes either, Anthropic's enum does not",
		},
		{
			name:   "a line-wrapped payload is unwrapped rather than forwarded",
			in:     "data:image/png;base64," + valid[:4] + "\n" + valid[4:],
			wantOK: true, wantMedia: "image/png", wantB64: valid,
			why: "Go's decoder skips \\r and \\n so this passed validation and was forwarded " +
				"verbatim; generativelanguage.googleapis.com answers 400 \"Invalid value\" for it",
		},
		{
			name:   "media-type parameters and space are trimmed off",
			in:     "data: image/png ;base64," + valid,
			wantOK: true, wantMedia: "image/png", wantB64: valid,
			why: "vendors whose media_type is a closed enum reject \" image/png \" and say nothing useful",
		},
		{
			name:   "a charset parameter does not become part of the type",
			in:     "data:text/plain;charset=utf-8;base64," + valid,
			wantOK: true, wantMedia: "text/plain", wantB64: valid,
			why: "the same enum problem, in the form a browser actually emits",
		},
		{
			name:   "an uppercase media type is folded to the spelling the wire accepts",
			in:     "data:IMAGE/PNG;base64," + valid,
			wantOK: true, wantMedia: "image/png", wantB64: valid,
			why: "RFC 2045 makes a media type case-INSENSITIVE, so this is the same type — and " +
				"api.anthropic.com answers 400 for it while accepting image/png, so a caller " +
				"using a legal spelling got an undiagnosable refusal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media, b64, ok := specutil.ParseDataURL(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v — %s", ok, tc.wantOK, tc.why)
			}
			if !tc.wantOK {
				if media != "" || b64 != "" {
					t.Errorf("a refused URL returned media=%q b64=%q; a caller that ignores ok "+
						"must not find usable-looking values", media, b64)
				}
				return
			}
			if media != tc.wantMedia {
				t.Errorf("media = %q, want %q", media, tc.wantMedia)
			}
			if b64 != tc.wantB64 {
				t.Errorf("b64 = %q, want %q", b64, tc.wantB64)
			}
		})
	}
}
