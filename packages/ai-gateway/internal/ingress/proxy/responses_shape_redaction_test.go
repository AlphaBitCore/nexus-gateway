package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A real /v1/responses body: `output[]`, not `choices[]`. This is what the
// native passthrough lane forwards verbatim when the upstream serves the
// Responses wire end to end.
const responsesShapeBody = `{"id":"resp_1","object":"response","model":"gpt-5",` +
	`"status":"completed","output":[{"type":"message","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"reach me at alice@example.com","annotations":[]}]}],` +
	`"usage":{"input_tokens":4,"output_tokens":7,"total_tokens":11}}`

// maskEmailHook redacts an email wherever it appears in a canonical text block,
// by span — the addressed form, not a positional segment list.
type maskEmailHook struct{ goHooks.Hook }

func (maskEmailHook) Name() string                                 { return "mask-email" }
func (maskEmailHook) SupportsEndpoint(_ goHooks.EndpointType) bool { return true }
func (maskEmailHook) SupportsModality(_ goHooks.Modality) bool     { return true }
func (maskEmailHook) Execute(_ context.Context, in *goHooks.HookInput) (*goHooks.HookResult, error) {
	res := &goHooks.HookResult{Decision: goHooks.Approve}
	if in.Normalized == nil {
		return res, nil
	}
	const secret = "alice@example.com"
	for mi, m := range in.Normalized.Messages {
		for bi, b := range m.Content {
			idx := strings.Index(b.Text, secret)
			if b.Type != normalize.ContentText || idx < 0 {
				continue
			}
			res.Decision = goHooks.Modify
			res.TransformSpans = append(res.TransformSpans, normalize.TransformSpan{
				Source:         normalize.SourceHook,
				SourceID:       "mask-email",
				Action:         normalize.ActionRedact,
				ContentAddress: contentAddr(mi, bi),
				Start:          idx,
				End:            idx + len(secret),
				Replacement:    "[REDACTED]",
			})
		}
	}
	return res, nil
}

func contentAddr(mi, bi int) string {
	return fmt.Sprintf("messages.%d.content.%d", mi, bi)
}

func responsesLaneHandler(t *testing.T, hook goHooks.Hook) *Handler {
	t.Helper()
	return &Handler{deps: &Deps{
		NormalizeRegistry: canonicalRegistry(),
		HookConfigCache:   newResponseHookCache(t, hook),
		CanonicalBridge:   canonicalbridge.New(provbuiltins.SchemaCodecs(nil)),
		TrafficAdapter:    &openai.Adapter{},
		Logger:            noopLogger(),
	}}
}

func runResponsesLane(t *testing.T, h *Handler, body string) canonicalRedactOutcome {
	t.Helper()
	ingress := Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(WithIngress(req.Context(), ingress))
	return h.redactCanonicalBuffer(req, &audit.Record{RequestID: "responses-lane"}, ingress,
		routingcore.RoutingTarget{ModelCode: "gpt-5"}, []byte(body), 0, "responses-lane", noopLogger())
}

// The /v1/responses lane could be SCANNED but not REDACTED. Its body is
// `output[]`, the rewriter walks `choices[]`, and rather than guess at the shape
// the apply step refused — so a block policy worked on this lane while a redact
// policy produced a 500. Decoding the shape to canonical chat before the hooks
// see it removes the second waist shape and gives the lane the redaction it
// never had.
func TestResponsesShapeIsRedactedAndReturnedOnItsOwnWire(t *testing.T) {
	out := runResponsesLane(t, responsesLaneHandler(t, maskEmailHook{}), responsesShapeBody)

	if out.failClosed {
		t.Fatalf("the responses lane refused a redaction it can now apply: %s", out.errMsg)
	}
	if !out.rewritten {
		t.Fatal("no rewrite was recorded — the hook matched but nothing reached the body")
	}
	if bytes.Contains(out.body, []byte("alice@example.com")) {
		t.Errorf("the secret survived into the delivered body:\n%s", out.body)
	}
	if !bytes.Contains(out.body, []byte("[REDACTED]")) {
		t.Errorf("the mask is absent from the delivered body:\n%s", out.body)
	}
	// And it must go back on the CALLER's wire. Handing a /v1/responses reader a
	// chat-shaped body would break every client on that lane.
	if !gjson.GetBytes(out.body, "output").Exists() || gjson.GetBytes(out.body, "choices").Exists() {
		t.Errorf("the delivered body is not the Responses shape:\n%s", out.body)
	}
	if got := gjson.GetBytes(out.body, "usage.total_tokens").Int(); got != 11 {
		t.Errorf("usage.total_tokens = %d, want 11 — the envelope must survive the round trip", got)
	}
}

// The control, and the reason the round trip is conditional. The passthrough
// lane exists to forward built-in tools and stateful fields verbatim; a
// decode/re-encode on every response would flatten them. A response nothing
// redacted must come back byte-identical.
func TestResponsesShapeIsBytePreservedWhenNothingIsRedacted(t *testing.T) {
	out := runResponsesLane(t, responsesLaneHandler(t, approveEverythingResponseHook{}), responsesShapeBody)

	if out.failClosed {
		t.Fatalf("an approved response was refused: %s", out.errMsg)
	}
	if out.rewritten {
		t.Error("a rewrite was recorded for a response no hook modified")
	}
	if string(out.body) != responsesShapeBody {
		t.Errorf("the passthrough body was re-encoded even though nothing was redacted.\n got: %s\nwant: %s",
			out.body, responsesShapeBody)
	}
}

type approveEverythingResponseHook struct{ goHooks.Hook }

func (approveEverythingResponseHook) Name() string                                 { return "approve" }
func (approveEverythingResponseHook) SupportsEndpoint(_ goHooks.EndpointType) bool { return true }
func (approveEverythingResponseHook) SupportsModality(_ goHooks.Modality) bool     { return true }
func (approveEverythingResponseHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

// The second CONTROL, and it is a control rather than evidence — mutation
// testing corrected an earlier claim here.
//
// The hooks could already see this lane's text before the decode was added: the
// normalize registry sniffs the body and the Responses codec reads `output[]`,
// so scanning — and therefore BLOCK decisions — worked. What did not work was
// REDACTION: the rewriter walks `choices[]` and the apply step refused the
// shape, so a redact policy on this lane produced a 500 instead of a masked
// response. Removing the decode leaves this test GREEN and reddens only the
// redaction one, which is exactly the shape of the gap.
//
// It stays because without it the redaction test could pass for the wrong
// reason: a hook that never matched would also leave the body unchanged, and
// the byte-preservation control would agree with it.
func TestResponsesShapeTextReachesTheHooks(t *testing.T) {
	var seen string
	h := responsesLaneHandler(t, spyHook{onInput: func(in *goHooks.HookInput) {
		if in.Normalized != nil {
			seen = strings.Join(in.Normalized.TextProjection(), "\n")
		}
	}})
	runResponsesLane(t, h, responsesShapeBody)
	if !strings.Contains(seen, "alice@example.com") {
		t.Errorf("the hook saw %q — the responses lane's text never reached the canonical payload, "+
			"so nothing on that lane is scanned", seen)
	}
}

type spyHook struct {
	goHooks.Hook
	onInput func(*goHooks.HookInput)
}

func (spyHook) Name() string                                 { return "spy" }
func (spyHook) SupportsEndpoint(_ goHooks.EndpointType) bool { return true }
func (spyHook) SupportsModality(_ goHooks.Modality) bool     { return true }
func (s spyHook) Execute(_ context.Context, in *goHooks.HookInput) (*goHooks.HookResult, error) {
	s.onInput(in)
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

// The fail-closed guard on this lane, driven through the check that actually
// remains.
//
// An earlier version of this test fed a body that the BRIDGE decoder refused
// (a Responses body reporting failure inside an HTTP 200). That decoder is no
// longer on this path — the redaction edits the Responses body in place — so
// that arm tested a failure mode the design removed. The property it protected
// is still real and is asserted here instead: a body the normalize registry
// cannot decode into either canonical response shape yields no payload, every
// content hook abstains, and the response must not go out with an approve stamp
// on a scan that did not happen.
//
// The presence check is the load-bearing half. It looks for `choices[]` on the
// chat shape, so asking it about an `output[]` body would answer "no content"
// and the guard would read the most dangerous case as the safest one.
func TestResponsesShapeUndecodableFailsClosedWhenAContentHookIsConfigured(t *testing.T) {
	// `output` with real text, but none of model / usage / status, so no
	// canonical response codec claims it and the sniff falls to generic JSON.
	const undecodable = `{"output":[{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"reach me at alice@example.com"}]}]}`

	out := runResponsesLane(t, responsesLaneHandler(t, maskEmailHook{}), undecodable)
	if !out.failClosed {
		t.Fatalf("a responses body no canonical codec could decode was delivered with a "+
			"content hook configured (body=%s)", out.body)
	}
}

// The lossy sibling, recorded rather than guarded. An `output` the decoder can
// PARSE but not interpret does not raise an error — it yields a canonical turn
// with `content: null`, and the gateway then scans an empty payload. There is no
// guard for it here because the input does not occur: these bodies come from the
// upstream API, not from a caller. Pinned so the behaviour is a known one rather
// than a surprise if that ever stops being true.
func TestResponsesShapeWithAnUninterpretableOutputDecodesToNothing(t *testing.T) {
	const odd = `{"id":"resp_1","object":"response","model":"gpt-5","output":"not-an-array"}`
	out := runResponsesLane(t, responsesLaneHandler(t, maskEmailHook{}), odd)
	if out.failClosed {
		t.Fatalf("refused a body the decoder accepted: %s", out.errMsg)
	}
	if out.rewritten {
		t.Error("a rewrite was recorded for a body that decoded to no content at all")
	}
}
