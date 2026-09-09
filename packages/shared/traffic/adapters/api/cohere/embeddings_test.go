package cohere

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// isEmbedBody discriminates the embed wire from the two other Cohere request
// shapes. Getting it wrong in either direction is a compliance outcome: a chat
// body taking this path is scanned by the wrong walk, and an embed body missing
// it is scanned by nothing.
func TestIsEmbedBodyDiscriminatesTheThreeCohereRequestShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"embed", `{"model":"embed-v4.0","texts":["a","b"]}`, true},
		{"chat", `{"model":"command-a","messages":[{"role":"user","content":"hi"}]}`, false},
		{"rerank", `{"model":"rerank-v3.5","query":"q","documents":["d"]}`, false},
		{"texts present but not an array", `{"texts":"a"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmbedBody([]byte(tc.body)); got != tc.want {
				t.Errorf("isEmbedBody = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractEmbedRequestReadsEveryDocument(t *testing.T) {
	body := []byte(`{"model":"embed-v4.0","input_type":"search_document",` +
		`"texts":["first doc","second doc"]}`)

	nc := extractEmbedRequest(body)
	if len(nc.Segments) != 2 || nc.Segments[0] != "first doc" || nc.Segments[1] != "second doc" {
		t.Fatalf("segments = %q, want both documents in order", nc.Segments)
	}
	if nc.Metadata["model"] != "embed-v4.0" {
		t.Errorf("model = %q, want embed-v4.0", nc.Metadata["model"])
	}
}

// The extractor skips non-string elements, so the rewriter must skip the same
// ones — otherwise a redaction lands on the wrong document.
func TestEmbedExtractAndRewriteAgreeOnSlots(t *testing.T) {
	body := []byte(`{"model":"embed-v4.0","texts":["alpha 111-22-3333",42,"beta 444-55-6666"]}`)

	nc := extractEmbedRequest(body)
	if len(nc.Segments) != 2 {
		t.Fatalf("segments = %q, want the two STRING documents only", nc.Segments)
	}

	out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{
		Segments: []string{"alpha [A]", "beta [B]"},
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 2 {
		t.Errorf("patched = %d, want 2", n)
	}
	got := string(out)
	if !strings.Contains(got, "alpha [A]") || !strings.Contains(got, "beta [B]") {
		t.Errorf("replacements did not land on their own slots: %s", got)
	}
	if strings.Contains(got, "111-22-3333") || strings.Contains(got, "444-55-6666") {
		t.Errorf("an original value survives: %s", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("the non-string element was disturbed: %s", got)
	}
}

func TestRewriteEmbedRequestWithNoSegmentsLeavesTheBodyAlone(t *testing.T) {
	body := []byte(`{"model":"embed-v4.0","texts":["untouched"]}`)
	out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("patched=%d body=%s — nothing was redacted, so nothing should change", n, out)
	}
}

// Fewer replacements than documents: the ones provided are applied and the rest
// are left as they were, rather than the walk running off the end.
func TestRewriteEmbedRequestStopsWhenSegmentsRunOut(t *testing.T) {
	body := []byte(`{"model":"embed-v4.0","texts":["one","two","three"]}`)
	out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{Segments: []string{"ONE"}})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 1 {
		t.Errorf("patched = %d, want 1", n)
	}
	got := string(out)
	if !strings.Contains(got, "ONE") || !strings.Contains(got, "two") || !strings.Contains(got, "three") {
		t.Errorf("unexpected body: %s", got)
	}
}
