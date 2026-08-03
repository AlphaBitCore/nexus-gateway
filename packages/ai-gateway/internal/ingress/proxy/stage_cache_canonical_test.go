package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// cacheNormalized contract: cache-stage consumers (L2 embedding input,
// L2 write-back, freshness detection) must read post-redaction content.
// When a request hook rewrote the wire body, the admission-time lazy
// canonical predates the rewrite, so the rewritten bytes are normalized
// once (memoized); a renormalize failure returns nil — skipping the
// cache semantics — rather than falling back to the pre-redaction
// canonical, which would leak redacted content to the embedding
// provider. Requests without a rewrite keep the admission canonical
// with zero added work.

// bodyEchoNormalize returns a payload derived from the body it was
// given and counts invocations, so tests can assert which bytes were
// normalized and how often.
type bodyEchoNormalize struct {
	id    string
	calls int
}

func (b *bodyEchoNormalize) ID() string { return b.id }

func (b *bodyEchoNormalize) Normalize(_ context.Context, body []byte, _ normalize.Meta) (normalize.NormalizedPayload, error) {
	b.calls++
	return normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: string(body)}}},
		},
	}, nil
}

func newCanonicalFixture(t *testing.T, stub *bodyEchoNormalize, originalBody []byte) *proxyState {
	t.Helper()
	reg := normalize.NewRegistry()
	reg.Register("openai", stub)
	h := &Handler{deps: &Deps{NormalizeRegistry: reg}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	rctx := h.buildRequestContext(req, nil, originalBody, provcore.FormatOpenAI, "gpt-4o", "chat")
	return &proxyState{
		h:        h,
		r:        req,
		rec:      &audit.Record{},
		resolved: Ingress{BodyFormat: provcore.FormatOpenAI},
		rctxFull: rctx,
		body:     originalBody,
		modelID:  "gpt-4o",
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func textOfFirstMessage(t *testing.T, np *normalize.NormalizedPayload) string {
	t.Helper()
	if np == nil || len(np.Messages) == 0 || len(np.Messages[0].Content) == 0 {
		t.Fatalf("payload has no message text: %+v", np)
	}
	return np.Messages[0].Content[0].Text
}

func TestCacheNormalized_NoRewrite_ReturnsAdmissionCanonical(t *testing.T) {
	stub := &bodyEchoNormalize{id: "openai"}
	s := newCanonicalFixture(t, stub, []byte(`raw content with ssn 123-45-6789`))
	callsAfterAdmission := stub.calls

	np := s.cacheNormalized()
	if got := textOfFirstMessage(t, np); got != `raw content with ssn 123-45-6789` {
		t.Errorf("no-rewrite canonical = %q, want admission content", got)
	}
	if stub.calls != callsAfterAdmission {
		t.Errorf("no-rewrite path must not renormalize (calls %d -> %d)", callsAfterAdmission, stub.calls)
	}
}

func TestCacheNormalized_Rewritten_RenormalizesRewrittenBody(t *testing.T) {
	stub := &bodyEchoNormalize{id: "openai"}
	s := newCanonicalFixture(t, stub, []byte(`raw content with ssn 123-45-6789`))
	// Simulate the hooks stage: redaction rewrote the wire body.
	s.rec.HookRewritten = true
	s.body = []byte(`raw content with ssn [REDACTED]`)

	np := s.cacheNormalized()
	got := textOfFirstMessage(t, np)
	if got != `raw content with ssn [REDACTED]` {
		t.Errorf("cache canonical = %q, want the REWRITTEN content", got)
	}
}

func TestCacheNormalized_Rewritten_MemoizedSingleRenormalize(t *testing.T) {
	stub := &bodyEchoNormalize{id: "openai"}
	s := newCanonicalFixture(t, stub, []byte(`original`))
	s.rec.HookRewritten = true
	s.body = []byte(`redacted`)
	callsAfterAdmission := stub.calls

	first := s.cacheNormalized()
	second := s.cacheNormalized()
	if stub.calls != callsAfterAdmission+1 {
		t.Errorf("expected exactly one renormalize, got %d extra", stub.calls-callsAfterAdmission)
	}
	if first != second {
		t.Errorf("memoized result must be stable across calls")
	}
}

func TestCacheNormalized_Rewritten_NoRegistry_ReturnsNilNotStale(t *testing.T) {
	stub := &bodyEchoNormalize{id: "openai"}
	s := newCanonicalFixture(t, stub, []byte(`raw content with ssn 123-45-6789`))
	s.rec.HookRewritten = true
	s.body = []byte(`redacted`)
	s.h.deps.NormalizeRegistry = nil

	if np := s.cacheNormalized(); np != nil {
		t.Errorf("renormalize unavailable must yield nil (skip cache semantics), got %+v — returning the stale pre-redaction canonical would leak", np)
	}
}
