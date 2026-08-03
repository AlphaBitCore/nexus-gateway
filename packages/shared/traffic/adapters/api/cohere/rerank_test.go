package cohere

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestRerank_IsRerankBody pins the schema discriminator: a rerank body is
// query(string)+documents(array) with NO messages. A chat body (has messages)
// or a body missing either rerank field is not rerank. Getting this wrong
// either routes chat through the wrong extractor or — the SEC-1 failure —
// lets a real rerank body fall through and be scanned by nothing.
func TestRerank_IsRerankBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"messages body is not rerank", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"query+documents no messages is rerank", `{"query":"find X","documents":["a","b"]}`, true},
		{"neither is not rerank", `{"model":"command-r"}`, false},
		{"query but no documents is not rerank", `{"query":"find X"}`, false},
		{"documents but no query is not rerank", `{"documents":["a","b"]}`, false},
		{"query non-string is not rerank", `{"query":42,"documents":["a"]}`, false},
		{"documents non-array is not rerank", `{"query":"find X","documents":"a"}`, false},
		{"messages present wins even with query+documents", `{"messages":[],"query":"q","documents":["a"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRerankBody([]byte(tc.body)); got != tc.want {
				t.Errorf("isRerankBody(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestRerank_ExtractRequest_ScansEveryDocument is the SEC-1 regression guard.
// A rerank body must yield the query FIRST, then EVERY string document in
// order, as scannable segments — the documents are the caller's retrieved
// corpus and the bulk-PII carrier. If any document were dropped from
// Segments the hook pipeline would forward that document's PII unscanned.
func TestRerank_ExtractRequest_ScansEveryDocument(t *testing.T) {
	body := []byte(`{"model":"rerank-english-v3.0","query":"find X","documents":["doc a","doc b"]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/rerank")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"find X", "doc a", "doc b"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments=%v want %v", nc.Segments, want)
	}
	for i, w := range want {
		if nc.Segments[i] != w {
			t.Errorf("Segments[%d]=%q want %q (ordering: query then every document)", i, nc.Segments[i], w)
		}
	}
	if nc.Metadata["model"] != "rerank-english-v3.0" {
		t.Errorf("Metadata[model]=%q want rerank-english-v3.0", nc.Metadata["model"])
	}
}

// TestRerank_ExtractRequest_NoModel: a rerank body without a model must still
// extract its segments and simply omit the model metadata key (no empty-string
// stamp) — proves the model branch is guarded, not assumed.
func TestRerank_ExtractRequest_NoModel(t *testing.T) {
	body := []byte(`{"query":"q","documents":["d0"]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/rerank")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "q" || nc.Segments[1] != "d0" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("Metadata[model] present=%q want absent", nc.Metadata["model"])
	}
}

// TestRerank_ExtractRequest_ChatUnchanged: adding the rerank branch must NOT
// change chat extraction — a messages body still flows through the chat path
// and yields its message segments.
func TestRerank_ExtractRequest_ChatUnchanged(t *testing.T) {
	body := []byte(`{"model":"command-r","messages":[{"role":"user","content":"hello chat"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello chat" {
		t.Errorf("Segments=%v want [hello chat] (chat path unchanged)", nc.Segments)
	}
}

// TestRerank_ExtractRequest_NeitherUnknownSchema: a body that is neither chat
// (no messages) nor rerank (no query+documents) must surface ErrUnknownSchema
// so the dispatcher demotes to a generic spec instead of scanning nothing.
func TestRerank_ExtractRequest_NeitherUnknownSchema(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"command-r","top_n":3}`), "/v2/rerank")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestRerank_RewriteRoundTrip_RedactsDocumentPII is the KEY SEC-2 assertion.
// Flow: extract a rerank body carrying PII in a document, simulate the redact
// decision by replacing that document's segment with "[REDACTED]", feed the
// modified content back through RewriteRequestBody, and assert the OUTGOING
// upstream body has the redacted text written into documents.1 while the query
// and documents.0 are preserved. The provider never sees the original PII.
func TestRerank_RewriteRoundTrip_RedactsDocumentPII(t *testing.T) {
	body := []byte(`{"model":"rerank-english-v3.0","query":"find the customer","documents":["safe doc","contact jane@acme.com now"]}`)
	a := &Adapter{}

	nc, err := a.ExtractRequest(context.Background(), body, "/v2/rerank")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Sanity: the PII document was extracted for scanning (SEC-1).
	if len(nc.Segments) != 3 || nc.Segments[2] != "contact jane@acme.com now" {
		t.Fatalf("Segments=%v want the PII document scanned at index 2", nc.Segments)
	}
	// Simulate the redaction decision on the scanned segment.
	nc.Segments[2] = "[REDACTED]"

	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v2/rerank", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 3 {
		t.Errorf("written=%d want 3 (query + 2 documents)", n)
	}
	// THE round-trip proof: the outgoing body's documents.1 no longer carries
	// the PII — the provider receives the redacted text.
	if got := gjson.GetBytes(out, "documents.1").String(); got != "[REDACTED]" {
		t.Errorf("documents.1=%q want [REDACTED] — PII leaked to upstream body", got)
	}
	if gjson.GetBytes(out, "documents.1").String() == "contact jane@acme.com now" {
		t.Errorf("original PII still present in outgoing documents.1")
	}
	// Non-PII slots preserved.
	if got := gjson.GetBytes(out, "query").String(); got != "find the customer" {
		t.Errorf("query=%q mutated, want preserved", got)
	}
	if got := gjson.GetBytes(out, "documents.0").String(); got != "safe doc" {
		t.Errorf("documents.0=%q mutated, want preserved", got)
	}
	if gjson.GetBytes(out, "model").String() != "rerank-english-v3.0" {
		t.Errorf("model field mutated")
	}
}

// TestRerank_RewriteRoundTrip_QueryRedacted: PII can also live in the query;
// redacting segment 0 writes back into the outgoing `query` field.
func TestRerank_RewriteRoundTrip_QueryRedacted(t *testing.T) {
	body := []byte(`{"query":"ssn 123-45-6789","documents":["d0"]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/rerank")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	nc.Segments[0] = "ssn [REDACTED]"
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v2/rerank", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 2 {
		t.Errorf("written=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "query").String(); got != "ssn [REDACTED]" {
		t.Errorf("query=%q want redacted", got)
	}
	if got := gjson.GetBytes(out, "documents.0").String(); got != "d0" {
		t.Errorf("documents.0=%q want preserved", got)
	}
}

// TestRerank_Rewrite_FewerSegmentsStopsCleanly: when the pipeline returns
// fewer segments than there are slots, the rewriter must guard-and-continue —
// consume what it has, leave the rest untouched, and report the true written
// count — never panic on the missing indices.
func TestRerank_Rewrite_FewerSegmentsStopsCleanly(t *testing.T) {
	body := []byte(`{"query":"q","documents":["a","b","c"]}`)
	// Only query + first document supplied.
	content := traffic.NormalizedContent{Segments: []string{"q-red", "a-red"}}
	out, n, err := rewriteRerankRequest(body, content)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 2 {
		t.Errorf("written=%d want 2 (query + documents.0 only)", n)
	}
	if got := gjson.GetBytes(out, "query").String(); got != "q-red" {
		t.Errorf("query=%q", got)
	}
	if got := gjson.GetBytes(out, "documents.0").String(); got != "a-red" {
		t.Errorf("documents.0=%q", got)
	}
	// Untouched slots keep original text — not blanked, not panicked.
	if got := gjson.GetBytes(out, "documents.1").String(); got != "b" {
		t.Errorf("documents.1=%q want original b", got)
	}
	if got := gjson.GetBytes(out, "documents.2").String(); got != "c" {
		t.Errorf("documents.2=%q want original c", got)
	}
}

// TestRerank_ExtractRewrite_NonStringDocSkippedConsistently: a non-string
// document slot must be skipped identically by BOTH extract and rewrite so
// segment index i always maps to the same slot it was scanned from. Here
// documents.1 is a number: extract emits [query, doc0, doc2]; after redacting
// the doc2 segment, rewrite must write back into documents.2 (NOT documents.1)
// and leave the numeric slot intact.
func TestRerank_ExtractRewrite_NonStringDocSkippedConsistently(t *testing.T) {
	body := []byte(`{"query":"q","documents":["doc a",123,"pii doc c"]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v2/rerank")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Non-string slot skipped in extract → 3 segments, not 4.
	if len(nc.Segments) != 3 || nc.Segments[1] != "doc a" || nc.Segments[2] != "pii doc c" {
		t.Fatalf("Segments=%v want [q doc a pii doc c] (numeric slot skipped)", nc.Segments)
	}
	nc.Segments[2] = "[REDACTED]"
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v2/rerank", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != 3 {
		t.Errorf("written=%d want 3", n)
	}
	// Index alignment: the redaction landed on documents.2, not documents.1.
	if got := gjson.GetBytes(out, "documents.2").String(); got != "[REDACTED]" {
		t.Errorf("documents.2=%q want [REDACTED] (index alignment held)", got)
	}
	if got := gjson.GetBytes(out, "documents.0").String(); got != "doc a" {
		t.Errorf("documents.0=%q want preserved", got)
	}
	// The numeric slot is untouched — not overwritten by a string segment.
	if got := gjson.GetBytes(out, "documents.1"); got.Type != gjson.Number || got.Int() != 123 {
		t.Errorf("documents.1=%v want numeric 123 untouched", got.Raw)
	}
}

// TestRerank_Rewrite_MalformedBody: invalid JSON into the rewriter surfaces
// ErrMalformed rather than a partial/garbage write.
func TestRerank_Rewrite_MalformedBody(t *testing.T) {
	_, n, err := rewriteRerankRequest([]byte(`not json`), traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
	if n != 0 {
		t.Errorf("written=%d want 0 on malformed", n)
	}
}

// TestRerank_RewriteRequestBody_ChatUnsupported: RewriteRequestBody on a
// non-rerank (chat) body still returns ErrRewriteUnsupported — chat redaction
// runs via the OpenAI-canonical adapter, not here — and leaves the body
// unchanged.
func TestRerank_RewriteRequestBody_ChatUnsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"role":"user","content":"my email is a@b.com"}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v2/chat",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]"}})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("written=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("chat body mutated: %q", out)
	}
}
