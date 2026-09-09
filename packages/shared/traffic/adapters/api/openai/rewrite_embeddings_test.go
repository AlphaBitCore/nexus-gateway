package openai

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// The refusal arms. Each says something different to the caller, and the caller
// acts on the difference: a malformed body is a client error, a missing `input`
// is a body this walk does not describe, and neither may be reported as a
// successful redaction of nothing.
func TestRewriteEmbeddingsInputRefusalArms(t *testing.T) {
	t.Run("malformed json", func(t *testing.T) {
		_, _, err := RewriteEmbeddingsInput([]byte(`{"input":`), traffic.NormalizedContent{
			Segments: []string{"x"},
		})
		if !errors.Is(err, traffic.ErrMalformed) {
			t.Errorf("err=%v want ErrMalformed", err)
		}
	})

	t.Run("no input field", func(t *testing.T) {
		_, _, err := RewriteEmbeddingsInput([]byte(`{"model":"m"}`), traffic.NormalizedContent{
			Segments: []string{"x"},
		})
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("err=%v want ErrUnknownSchema", err)
		}
	})

	t.Run("no segments leaves the body alone", func(t *testing.T) {
		body := []byte(`{"model":"m","input":"untouched"}`)
		out, n, err := RewriteEmbeddingsInput(body, traffic.NormalizedContent{})
		if err != nil || n != 0 || string(out) != string(body) {
			t.Errorf("n=%d err=%v body=%s — nothing was redacted, so nothing should change",
				n, err, out)
		}
	})
}

// `input` shapes the OpenAI embeddings wire allows that carry no addressable
// text. A walk that reported a patch here would be claiming to have masked
// something it never touched.
func TestRewriteEmbeddingsInputOnShapesWithNoTextSlot(t *testing.T) {
	t.Run("a bare token array", func(t *testing.T) {
		body := []byte(`{"model":"m","input":[[1,2,3]]}`)
		out, n, err := RewriteEmbeddingsInput(body, traffic.NormalizedContent{
			Segments: []string{"REDACTED"},
		})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if n != 0 {
			t.Errorf("patched=%d want 0 — a token array holds no text a policy can address", n)
		}
		if !strings.Contains(string(out), "[1,2,3]") {
			t.Errorf("the token array was disturbed: %s", out)
		}
	})

	t.Run("input is neither a string nor an array", func(t *testing.T) {
		body := []byte(`{"model":"m","input":42}`)
		out, n, err := RewriteEmbeddingsInput(body, traffic.NormalizedContent{
			Segments: []string{"REDACTED"},
		})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if n != 0 || string(out) != string(body) {
			t.Errorf("n=%d body=%s — the extractor produced no segment for this shape, so there "+
				"is no slot to write", n, out)
		}
	})
}

// More documents than replacements: the walk applies what it was given and stops,
// rather than reusing the last replacement or running past the end.
func TestRewriteEmbeddingsInputStopsWhenSegmentsRunOut(t *testing.T) {
	body := []byte(`{"model":"m","input":["one","two","three"]}`)
	out, n, err := RewriteEmbeddingsInput(body, traffic.NormalizedContent{
		Segments: []string{"ONE", "TWO"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("patched=%d want 2", n)
	}
	got := string(out)
	if !strings.Contains(got, "ONE") || !strings.Contains(got, "TWO") || !strings.Contains(got, "three") {
		t.Errorf("unexpected body: %s", got)
	}
}
