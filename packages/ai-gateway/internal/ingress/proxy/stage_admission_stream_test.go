package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// `stream: true` honoured on any kind a force-non-stream DENYLIST
// does not name is the defect. Such a list names image generation and TTS;
// embeddings and rerank are not on it, so `{"input":"…","stream":true}` sets Stream
// on the upstream request and takes the SSE responder — for upstreams that answer
// with one JSON object and have no event stream to parse.
//
// readBody is the seam where the client's flag becomes the gateway's decision,
// so these arms assert it there rather than trusting the typology predicate
// alone: the predicate being right and the call site not using it is exactly the
// shape this defect takes.
func TestReadBody_StreamHonouredOnlyWhereTheKindHasAStream(t *testing.T) {
	cases := []struct {
		name      string
		shape     typology.WireShape
		format    provcore.Format
		path      string
		pathModel string // Gemini carries the model in the route, not the body
		body      string
		fromPath  bool
		want      bool
	}{
		{
			name:   "embeddings cannot opt into streaming",
			shape:  typology.WireShapeOpenAIEmbeddings,
			format: provcore.FormatOpenAI,
			path:   "/v1/embeddings",
			body:   `{"model":"text-embedding-3-small","input":"hello","stream":true}`,
			want:   false,
		},
		{
			name:   "rerank cannot opt into streaming",
			shape:  typology.WireShapeCohereRerank,
			format: provcore.FormatCohere, // the format the real rerank ingress uses
			path:   "/v1/rerank",
			body:   `{"model":"rerank-v3.5","query":"q","documents":["a"],"stream":true}`,
			want:   false,
		},
		{
			// The sibling. Without it, "force non-stream everywhere" would pass
			// the two arms above while breaking the product.
			name:   "chat still streams when the client asks",
			shape:  typology.WireShapeOpenAIChat,
			format: provcore.FormatOpenAI,
			path:   "/v1/chat/completions",
			body:   `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want:   true,
		},
		{
			// The ONLY registered ingress whose stream flag comes from the ROUTE
			// rather than the body: Gemini's :streamGenerateContent. It is the
			// arm the new gate could most plausibly have silenced, and the
			// existing StreamFromPath test exercises a layer BELOW the gate, so
			// it cannot observe this.
			name:      "gemini streamGenerateContent still streams from the path",
			shape:     typology.WireShapeGeminiGenerateContent,
			format:    provcore.FormatGemini,
			path:      "/v1beta/models/gemini-2.5-flash:streamGenerateContent",
			pathModel: "gemini-2.5-flash:streamGenerateContent",
			body:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			fromPath:  true,
			want:      true,
		},
		{
			name:   "chat still does not stream when the client does not ask",
			shape:  typology.WireShapeOpenAIChat,
			format: provcore.FormatOpenAI,
			path:   "/v1/chat/completions",
			body:   `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{deps: &Deps{}}
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
			if tc.pathModel != "" {
				req.SetPathValue("model", tc.pathModel)
			}
			_, _, _, isStream, err := h.readBody(req, Ingress{
				WireShape:      tc.shape,
				BodyFormat:     tc.format,
				StreamFromPath: tc.fromPath,
			})
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if isStream != tc.want {
				t.Errorf("isStream=%v want %v for %s; body was %s",
					isStream, tc.want, tc.shape, tc.body)
			}
		})
	}
}
