package adapters_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	trafficcohere "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/cohere"
	trafficgemini "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/gemini"
	trafficopenai "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	trafficvoyage "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/voyage"
)

// An embeddings request is the plainest possible carrier of user text: the
// caller hands the model a document to vectorise, verbatim, with no chat
// scaffolding. On the intercepting substrates — the agent's NE proxy and the
// compliance proxy — the adapter is chosen by the HOST the client dialled, so a
// tool calling Cohere or Gemini embeddings directly is read by that vendor's
// traffic adapter, not by OpenAI's.
//
// If that adapter has no embeddings branch, ExtractRequest answers
// ErrUnknownSchema, the hook pipeline receives no content, and the document goes
// upstream scanned by nothing. Nothing about the request looks different.
//
// The wire shapes below are the ones the normalize codecs document as
// authoritative for each vendor (cohere_embeddings.go, gemini_embeddings.go,
// openai_embeddings.go, voyage_embeddings.go) rather than shapes invented here.
func TestEveryEmbeddingsWireIsScannable(t *testing.T) {
	const secret = "employee SSN 123-45-6789"

	cases := []struct {
		name    string
		adapter traffic.Adapter
		path    string
		body    string
	}{
		{
			name:    "openai",
			adapter: &trafficopenai.Adapter{},
			path:    "/v1/embeddings",
			body:    `{"model":"text-embedding-3-small","input":["` + secret + `"]}`,
		},
		{
			name:    "cohere",
			adapter: &trafficcohere.Adapter{},
			path:    "/v2/embed",
			body:    `{"model":"embed-v4.0","input_type":"search_document","texts":["` + secret + `"]}`,
		},
		{
			name:    "gemini single",
			adapter: &trafficgemini.Adapter{},
			path:    "/v1beta/models/text-embedding-004:embedContent",
			body:    `{"model":"models/text-embedding-004","content":{"parts":[{"text":"` + secret + `"}]}}`,
		},
		{
			name:    "gemini batch",
			adapter: &trafficgemini.Adapter{},
			path:    "/v1beta/models/text-embedding-004:batchEmbedContents",
			body: `{"requests":[{"model":"models/text-embedding-004",` +
				`"content":{"parts":[{"text":"` + secret + `"}]}}]}`,
		},
		{
			name:    "voyage",
			adapter: &trafficvoyage.Adapter{},
			path:    "/v1/embeddings",
			body:    `{"model":"voyage-3","input":["` + secret + `"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := tc.adapter.ExtractRequest(context.Background(), []byte(tc.body), tc.path)
			if err != nil {
				t.Fatalf("ExtractRequest: %v — the hook pipeline gets no content for this wire, "+
					"so the document goes upstream scanned by nothing", err)
			}
			joined := strings.Join(nc.Segments, "")
			if !strings.Contains(joined, secret) {
				t.Fatalf("the embedded document did not reach Segments (got %q). A compliance "+
					"policy cannot match text the extractor never produced", joined)
			}
		})
	}
}

// Rerank is the other endpoint kind that reaches the hook pipeline through the
// format-aware extraction rather than the canonical waist, and it carries user
// text in two places: the query the caller asked, and every document they handed
// over to be ranked. Both are scannable, both must be maskable, and a walk that
// covers one and not the other is the shape this file exists to catch.
func TestRerankQueryAndDocumentsAreScannableAndRewritable(t *testing.T) {
	const (
		q  = "who is 111-22-3333"
		d0 = "doc one 444-55-6666"
		d1 = "doc two 777-88-9999"
	)
	adapter := &trafficcohere.Adapter{}
	path := "/v2/rerank"
	body := `{"model":"rerank-v3.5","query":"` + q + `","documents":["` + d0 + `","` + d1 + `"]}`

	nc, err := adapter.ExtractRequest(context.Background(), []byte(body), path)
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	joined := strings.Join(nc.Segments, "\x00")
	for _, want := range []string{q, d0, d1} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q did not reach Segments — a policy cannot match text the extractor never "+
				"produced (got %q)", want, nc.Segments)
		}
	}

	masked := make([]string, len(nc.Segments))
	for i := range nc.Segments {
		masked[i] = "[REDACTED-" + strconv.Itoa(i) + "]"
	}
	out, patched, err := adapter.RewriteRequestBody(context.Background(), []byte(body), path,
		traffic.NormalizedContent{Segments: masked})
	if err != nil {
		t.Fatalf("RewriteRequestBody: %v — a redaction decided here cannot be written back", err)
	}
	if patched != len(nc.Segments) {
		t.Errorf("patched=%d but the extractor reported %d slots — the two walks disagree, so a "+
			"replacement lands on the wrong document", patched, len(nc.Segments))
	}
	for _, gone := range []string{q, d0, d1} {
		if strings.Contains(string(out), gone) {
			t.Errorf("%q survives the rewrite: %s", gone, out)
		}
	}
}

// The companion property: what the extractor found, the rewriter must be able to
// mask. An extractor that reports text no rewrite can reach turns a redact policy
// into a refusal (or, on the fail-open substrate, a pass-through) on every
// request of that shape.
func TestEveryEmbeddingsWireIsRewritable(t *testing.T) {
	const (
		secret = "employee SSN 123-45-6789"
		masked = "employee SSN [REDACTED]"
	)

	cases := []struct {
		name    string
		adapter traffic.Adapter
		path    string
		body    string
	}{
		{
			name:    "openai",
			adapter: &trafficopenai.Adapter{},
			path:    "/v1/embeddings",
			body:    `{"model":"text-embedding-3-small","input":["` + secret + `"]}`,
		},
		{
			name:    "cohere",
			adapter: &trafficcohere.Adapter{},
			path:    "/v2/embed",
			body:    `{"model":"embed-v4.0","input_type":"search_document","texts":["` + secret + `"]}`,
		},
		{
			name:    "gemini single",
			adapter: &trafficgemini.Adapter{},
			path:    "/v1beta/models/text-embedding-004:embedContent",
			body:    `{"model":"models/text-embedding-004","content":{"parts":[{"text":"` + secret + `"}]}}`,
		},
		{
			name:    "voyage",
			adapter: &trafficvoyage.Adapter{},
			path:    "/v1/embeddings",
			body:    `{"model":"voyage-3","input":["` + secret + `"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, patched, err := tc.adapter.RewriteRequestBody(context.Background(), []byte(tc.body),
				tc.path, traffic.NormalizedContent{Segments: []string{masked}})
			if err != nil {
				t.Fatalf("RewriteRequestBody: %v — a redaction decided for this wire cannot be "+
					"written back", err)
			}
			if patched == 0 {
				t.Fatalf("the rewrite reported zero patched slots — a redaction for this wire " +
					"would report success while changing nothing")
			}
			if strings.Contains(string(out), secret) {
				t.Errorf("the original value survives the rewrite: %s", out)
			}
			if !strings.Contains(string(out), masked) {
				t.Errorf("the masked value is not in the rewritten body: %s", out)
			}
		})
	}
}
