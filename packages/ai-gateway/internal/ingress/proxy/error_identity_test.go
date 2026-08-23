package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A failed request has to say what failed, in a form something other than a
// human can read. Production carried rows this could not do for:
//
//	/v1/responses               400  error_code null, error_reason null
//	/v1/audio/transcriptions    400  error_code null, error_reason null
//	/v1/rerank                  502  error_code null, reason only an English sentence
//	/v1/embeddings              400  error_code null, reason only an English sentence
//
// Nothing could group them, alert on them, or count them, and the two with no
// reason at all left no trace of what happened. The cause was structural rather
// than an oversight per site: the four writers below build their own envelopes
// instead of going through writeIngressError, and each set only StatusCode and
// HookReasonCode.
//
// This drives every one of them and asserts the identity, which is the property
// an operator actually depends on.
func TestGatewayRejections_EveryFailureNamesItself(t *testing.T) {
	h := &Handler{deps: &Deps{}}

	for _, tc := range []struct {
		name     string
		call     func(w http.ResponseWriter, rec *audit.Record)
		wantCode string
		// ownDialect marks a case driven with an ingress whose error envelope is
		// its own — anthropic's carries no `code` field, because the real
		// api.anthropic.com envelope does not. For those the caller-visible code
		// cannot be asserted, so the identity is checked on the ROW, which is
		// what an operator groups by. Shape wins over the code field here for
		// the same reason it wins everywhere else on this surface: an Anthropic
		// SDK handed the OpenAI shape does not lose a field, it fails to parse,
		// and the message never reaches the user at all.
		ownDialect bool
		// wireType is the envelope's own `type`, a shipped field SDK consumers
		// dispatch on. It must NOT change when the audit code is added.
	}{
		{
			name: "no compatible capability",
			call: func(w http.ResponseWriter, rec *audit.Record) {
				h.writeNoCompatibleCapability(w, rec, &routingcore.NoCompatibleProviderError{
					Available: []routingcore.CandidateCapability{
						{Provider: "openai", Model: "text-embedding-3-small", MaxBatchSize: 2048},
					},
				})
			},
			wantCode: "NO_COMPATIBLE_CAPABILITY",
		},
		{
			name: "responses feature needs a native target",
			call: func(w http.ResponseWriter, rec *audit.Record) {
				h.writeResponsesFeatureRejection(w, rec, &ResponsesCrossFormatRejection{
					Param:   "previous_response_id",
					Message: "previous_response_id requires a target that natively serves responses",
				})
			},
			wantCode: "FEATURE_REQUIRES_NATIVE_RESPONSES_TARGET",
		},
		{
			name: "no compatible provider",
			call: func(w http.ResponseWriter, rec *audit.Record) {
				h.writeNoCompatibleProvider(w, rec, provcore.FormatAnthropic, "cohere",
					typology.EndpointKindChat)
			},
			wantCode:   "NO_COMPATIBLE_PROVIDER",
			ownDialect: true,
		},
		{
			// The same writer on an ingress that HAS a code slot. Without this
			// twin the anthropic case alone would let the code quietly vanish
			// from every caller, since its envelope never shows one.
			name: "no compatible provider, OpenAI ingress",
			call: func(w http.ResponseWriter, rec *audit.Record) {
				h.writeNoCompatibleProvider(w, rec, provcore.FormatOpenAI, "cohere",
					typology.EndpointKindChat)
			},
			wantCode: "NO_COMPATIBLE_PROVIDER",
		},
		{
			name: "cross-format streaming unsupported, OpenAI ingress",
			call: func(w http.ResponseWriter, rec *audit.Record) {
				h.writeCrossFormatStreamUnsupported(w, rec, "openai", "cohere")
			},
			wantCode: "CROSS_FORMAT_STREAM_UNSUPPORTED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &audit.Record{}
			w := httptest.NewRecorder()
			tc.call(w, rec)

			if rec.StatusCode != http.StatusBadRequest || w.Code != http.StatusBadRequest {
				t.Errorf("status rec=%d wire=%d, want 400", rec.StatusCode, w.Code)
			}
			if rec.ErrorCode != tc.wantCode {
				t.Errorf("error_code = %q, want %q — a rejection nothing can group by is the "+
					"row this test exists to prevent", rec.ErrorCode, tc.wantCode)
			}
			if rec.ErrorReason == "" {
				t.Error("error_reason is empty; the row records that something failed and " +
					"nothing about what")
			}
			if len(rec.ResponseBody) == 0 {
				t.Error("the emitted envelope was not stamped onto the row, so Traffic-drawer " +
					"triage has nothing to show — a gateway error body carries no user content " +
					"and is the most useful thing to see when a request fails")
			}
			// error.type comes from the status, on OpenAI's vocabulary. Each of
			// these writers used to invent its own value there — "unsupported_feature",
			// "cross_format_stream_unsupported" — none of which any SDK models,
			// which is the same defect AP-3 fixed for "proxy_error" on the
			// writers next door. The identity of the failure belongs in
			// error.code, which is where the rest of the surface puts it.
			if got := gjson.GetBytes(w.Body.Bytes(), "error.type").String(); got != "invalid_request_error" {
				t.Errorf("error.type = %q, want the status-derived invalid_request_error", got)
			}
			// The identity an operator groups by, asserted for EVERY case
			// including the ones whose envelope has nowhere to show it. This is
			// the half that must never weaken: a failure the row cannot name is
			// a failure nothing can count.
			if rec.ErrorCode != tc.wantCode {
				t.Errorf("row error_code = %q, want %q — a failure the row cannot name is a "+
					"failure nothing can group, alert on, or count", rec.ErrorCode, tc.wantCode)
			}
			if tc.ownDialect {
				// An ingress with its own envelope must GET its own envelope.
				// Handing an Anthropic SDK the OpenAI shape does not cost it a
				// field, it costs it the whole response: the parse fails and the
				// message never reaches the user.
				if gjson.GetBytes(w.Body.Bytes(), "type").String() != "error" {
					t.Errorf("an ingress with its own dialect got the wrong envelope: %s", w.Body.Bytes())
				}
			} else if got := gjson.GetBytes(w.Body.Bytes(), "error.code").String(); got != tc.wantCode {
				// Where the envelope HAS a code slot, the caller is told the
				// same machine code the operator will group by.
				t.Errorf("error.code = %q, want %q — the caller cannot branch on a failure the row can name", got, tc.wantCode)
			}
			if gjson.GetBytes(w.Body.Bytes(), "error.message").String() == "" {
				t.Error("the client received an envelope with no message")
			}
			// What the row says and what the client was told must agree.
			if wireMsg := gjson.GetBytes(w.Body.Bytes(), "error.message").String(); wireMsg != rec.ErrorReason {
				t.Errorf("row reason %q and client message %q disagree; an operator reading the "+
					"row would be looking at a different failure than the caller reported",
					rec.ErrorReason, wireMsg)
			}
		})
	}
}
