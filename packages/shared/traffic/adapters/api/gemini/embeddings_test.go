package gemini

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// One character separates the embedding wire's `content` from generateContent's
// `contents`, so the discriminator has to be checked in both directions: a
// generateContent body taking the embedding path would be walked by the wrong
// extractor, and an embedding body missing it is scanned by nothing.
func TestIsEmbedBodyDiscriminatesContentFromContents(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"embedContent", `{"content":{"parts":[{"text":"a"}]}}`, true},
		{"batchEmbedContents", `{"requests":[{"content":{"parts":[{"text":"a"}]}}]}`, true},
		{"generateContent", `{"contents":[{"parts":[{"text":"a"}]}]}`, false},
		{"content present but not an object", `{"content":"a"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmbedBody([]byte(tc.body)); got != tc.want {
				t.Errorf("isEmbedBody = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractEmbedRequestReadsBothShapes(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		body := []byte(`{"model":"models/text-embedding-004",` +
			`"content":{"parts":[{"text":"first"},{"text":"second"}]}}`)
		nc := extractEmbedRequest(body)
		if len(nc.Segments) != 2 || nc.Segments[0] != "first" || nc.Segments[1] != "second" {
			t.Fatalf("segments = %q, want both parts in order", nc.Segments)
		}
		if nc.Metadata["model"] != "models/text-embedding-004" {
			t.Errorf("model = %q", nc.Metadata["model"])
		}
	})

	t.Run("batch", func(t *testing.T) {
		body := []byte(`{"requests":[` +
			`{"content":{"parts":[{"text":"a"}]}},` +
			`{"content":{"parts":[{"text":"b"},{"text":"c"}]}}]}`)
		nc := extractEmbedRequest(body)
		want := []string{"a", "b", "c"}
		if len(nc.Segments) != len(want) {
			t.Fatalf("segments = %q, want %q", nc.Segments, want)
		}
		for i := range want {
			if nc.Segments[i] != want[i] {
				t.Fatalf("segments = %q, want %q — batch order decides which document a "+
					"redaction lands on", nc.Segments, want)
			}
		}
	})
}

// The paths the extractor walks and the paths the rewriter writes are the same
// list, so a non-text part cannot shift the two apart.
func TestEmbedExtractAndRewriteAgreeAcrossBatchAndNonTextParts(t *testing.T) {
	body := []byte(`{"requests":[` +
		`{"content":{"parts":[{"text":"alpha 111-22-3333"},{"inlineData":{"mimeType":"image/png","data":"x"}}]}},` +
		`{"content":{"parts":[{"text":"beta 444-55-6666"}]}}]}`)

	nc := extractEmbedRequest(body)
	if len(nc.Segments) != 2 {
		t.Fatalf("segments = %q, want the two TEXT parts only", nc.Segments)
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
	if !strings.Contains(got, "inlineData") {
		t.Errorf("the non-text part was disturbed: %s", got)
	}
}

func TestRewriteEmbedRequestWithNothingToDo(t *testing.T) {
	t.Run("no segments", func(t *testing.T) {
		body := []byte(`{"content":{"parts":[{"text":"untouched"}]}}`)
		out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{})
		if err != nil || n != 0 || string(out) != string(body) {
			t.Errorf("n=%d err=%v body=%s — nothing was redacted, so nothing should change", n, err, out)
		}
	})

	t.Run("no text slots", func(t *testing.T) {
		body := []byte(`{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"x"}}]}}`)
		out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{Segments: []string{"x"}})
		if err != nil || n != 0 || string(out) != string(body) {
			t.Errorf("n=%d err=%v body=%s — there is no text slot to write into", n, err, out)
		}
	})

	t.Run("fewer segments than slots", func(t *testing.T) {
		body := []byte(`{"content":{"parts":[{"text":"one"},{"text":"two"}]}}`)
		out, n, err := rewriteEmbedRequest(body, traffic.NormalizedContent{Segments: []string{"ONE"}})
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if n != 1 {
			t.Errorf("patched = %d, want 1", n)
		}
		if !strings.Contains(string(out), "ONE") || !strings.Contains(string(out), "two") {
			t.Errorf("unexpected body: %s", out)
		}
	})
}
