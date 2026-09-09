package canonicalbridge

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// OpenAI rejects a file part that carries file_data without a filename:
//
//	Missing required parameter: messages[N].content[M].file.filename
//
// So a request that arrives valid must not come back from a translation hop
// missing it. The rich-content signature could not see this — it excluded the
// filename outright, with the reasoning that "only some wires carry one", which
// is true and is exactly why the loss was invisible.
//
// Two rules, because they are not the same rule:
//
//   - PRESENCE is universal. Whatever the hop, a file part with bytes must come
//     back with a name, even one the gateway supplied, or the next OpenAI call
//     is a 400.
//   - PRESERVATION holds only where the intermediate wire has somewhere to keep
//     it. Anthropic's document has `title`, and the ingress direction already
//     reads a filename back out of it, so that hop must round-trip exactly.
//     Gemini's inlineData has no slot at all; the name is genuinely lost there
//     and a supplied one is the honest outcome.

func filenamesIn(body string) []string {
	var out []string
	gjson.Get(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str != "file" {
				return true
			}
			if part.Get("file.file_data").Str == "" {
				return true // only file_data makes the filename mandatory
			}
			out = append(out, part.Get("file.filename").Str)
			return true
		})
		return true
	})
	return out
}

func TestFilePartAlwaysCarriesAFilename(t *testing.T) {
	// The control is PER CASE, and it took two attempts to get right.
	//
	// The first version skipped when the AFTER side carried no file part, so
	// "this round trip has no file part" and "the hop dropped the file part"
	// were the same observation and the defect reported clean. Renaming the
	// Gemini ingress key from file_data to file_url turned four of six subtests
	// into SKIPs with the package still green.
	//
	// A global "at least one case carried a file part" counter does not fix it
	// either: the same mutation leaves two cases intact, so the counter is
	// satisfied while four cases silently stop asserting. The BEFORE side of
	// each round trip is the only honest control — a case that started with a
	// file part must still have one at the end.
	for _, tc := range richContentRoundTrips(t) {
		t.Run(tc.name, func(t *testing.T) {
			before, names := filenamesIn(tc.before), filenamesIn(tc.after)
			if len(before) == 0 {
				return // this round trip never carried one
			}
			if len(names) == 0 {
				t.Fatalf("the round trip started with %d inline file part(s) and ended with "+
					"none — the hop dropped them, which is a larger failure than the missing "+
					"filename this test was written for.\nbody: %s", len(before), tc.after)
			}
			for i, n := range names {
				if strings.TrimSpace(n) == "" {
					t.Errorf("file part %d came back with no filename.\nbody: %s\n"+
						"OpenAI rejects file_data without one, so this hop turns a request that "+
						"was valid on arrival into a 400.", i, tc.after)
				}
			}
		})
	}
}

func TestFilenameSurvivesAWireThatCanCarryIt(t *testing.T) {
	for _, tc := range richContentRoundTrips(t) {
		if !tc.preservesFilename {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			before, after := filenamesIn(tc.before), filenamesIn(tc.after)
			if len(before) == 0 {
				return // this round trip never carried one
			}
			if len(after) == 0 {
				t.Fatalf("the file part itself did not survive the hop (%d before, none "+
					"after), so there is no filename left to compare", len(before))
			}
			if strings.Join(before, ",") != strings.Join(after, ",") {
				t.Errorf("filenames changed across a wire that can carry them: %v -> %v.\n"+
					"Anthropic keeps it on the document's title, and the ingress direction already "+
					"reads it back from there, so losing it here means only one half of that pair "+
					"was implemented.", before, after)
			}
		})
	}

}

// richContentRoundTrip is one A→B→A hop, with both canonical bodies, so a gate
// can compare a specific field rather than the whole signature.
type richContentRoundTrip struct {
	name          string
	before, after string
	// preservesFilename records whether the intermediate wire has anywhere to
	// keep a file's name. Anthropic does (document.title); Gemini's inlineData
	// does not, so the name is genuinely lost there and one is supplied on the
	// way back.
	preservesFilename bool
}

func richContentRoundTrips(t *testing.T) []richContentRoundTrip {
	t.Helper()
	b := testBridge(t)
	shapes := []provcore.Format{provcore.FormatOpenAI, provcore.FormatAnthropic, provcore.FormatGemini}

	var out []richContentRoundTrip
	for _, a := range shapes {
		for _, viaB := range shapes {
			if a == viaB || !b.ChatRoutable(a, viaB) || !b.ChatRoutable(viaB, a) {
				continue
			}
			bodyA := []byte(richNativeChatBody(a))
			wireB, _, err := b.IngressChatToWire(a, viaB, bodyA, dummyCallTarget(viaB), false)
			if err != nil {
				t.Fatalf("A→B (%s→%s): %v", a, viaB, err)
			}
			bodyA2, _, err := b.IngressChatToWire(viaB, a, wireB, dummyCallTarget(a), false)
			if err != nil {
				t.Fatalf("B→A (%s→%s): %v", viaB, a, err)
			}
			canonA, err := b.IngressChatToCanonical(a, bodyA, dummyCallTarget(a))
			if err != nil {
				t.Fatalf("canonicalize original %s: %v", a, err)
			}
			canonA2, err := b.IngressChatToCanonical(a, bodyA2, dummyCallTarget(a))
			if err != nil {
				t.Fatalf("canonicalize round-tripped %s: %v", a, err)
			}
			out = append(out, richContentRoundTrip{
				name:              string(a) + "_via_" + string(viaB) + "_back",
				before:            string(canonA),
				after:             string(canonA2),
				preservesFilename: viaB == provcore.FormatAnthropic,
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("no routable round trips; these gates would pass vacuously")
	}
	return out
}
