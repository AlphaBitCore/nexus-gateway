// The canonical locus is fail-closed: the gateway is not in the host's outbound
// packet path, so a redaction the policy demanded and could not apply must never
// deliver the original.
//
// That rule was only half enforced. It covered "a redaction was DECIDED and the
// rewrite failed". It did not cover "the content never reached the hook at all",
// where no decision happens, so the fail-closed branch is unreachable by
// construction — the pipeline reads a nil payload, every content hook abstains,
// and the response goes out unscanned with an approve stamp on it.
//
// The trigger is wider than a missing registry. The canonical codec treats
// `model`, `choices` and `usage` as required, so an upstream that omits `usage`
// — a streamed response folded to non-stream, a cache replay, a provider that
// only sends usage when asked — decodes to nothing and takes the same path.
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// scanningHook is a content-scanning hook that finds nothing. What matters is
// that it IS one: the question the gateway has to answer is "was anything
// configured to look at this content", not "did anything match".
type scanningHook struct {
	goHooks.AnyEndpointAnyModality
}

func (scanningHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

func (scanningHook) ScansContent() bool { return true }

func redactWithoutCanonical(t *testing.T, body string) canonicalRedactOutcome {
	t.Helper()
	h := &Handler{deps: &Deps{
		// No NormalizeRegistry: the canonical decode cannot happen, which is the
		// same position the gateway is in when an upstream body the codec
		// declines reaches this stage.
		HookConfigCache: newResponseHookCache(t, scanningHook{}),
		TrafficAdapter:  &openai.Adapter{},
		Logger:          noopLogger(),
	}}
	ingress := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), ingress))
	return h.redactCanonicalBuffer(req, &audit.Record{RequestID: "failclosed"}, ingress,
		routingcore.RoutingTarget{ModelCode: "m", Region: "us-east-1"},
		[]byte(body), 0, "failclosed", noopLogger())
}

func TestUnscannableContentIsNotDelivered(t *testing.T) {
	const withPII = `{"id":"x","object":"chat.completion","model":"gpt-4o",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"card 4111 1111 1111 1111"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	out := redactWithoutCanonical(t, withPII)

	if !out.failClosed {
		t.Fatalf("a response no hook could scan was delivered: %s\n"+
			"Every content hook abstained because the payload was nil, so the audit records an "+
			"approve for a scan that never happened — indistinguishable from a clean response.",
			string(out.body))
	}
	// Nothing further is asserted about out.body on purpose, and the reason
	// belongs here rather than in a branch that returns either way. The outcome
	// struct keeps the ORIGINAL bytes on every fail-closed arm; the caller, on
	// seeing failClosed, writes the error instead of the body. Checking the
	// struct for the secret would be asserting a contract this type does not
	// have — and the previous version of that check returned in both branches,
	// so it read like a leak check while asserting nothing.
	if out.rewritten {
		t.Error("a response nothing could scan was marked rewritten; the redacted-copy guard " +
			"would then persist a body no redaction ever touched")
	}
}

// TestBodyWithNoScannableContentStillPasses is the control. Failing closed
// whenever the canonical is missing would reject responses that carry nothing to
// scan, which is a self-inflicted outage rather than a compliance win.
func TestBodyWithNoScannableContentStillPasses(t *testing.T) {
	const empty = `{"id":"x","object":"chat.completion","model":"gpt-4o",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`

	if out := redactWithoutCanonical(t, empty); out.failClosed {
		t.Errorf("a response carrying no scannable text was refused (status=%d msg=%q); there was "+
			"nothing for a hook to miss", out.errStatus, out.errMsg)
	}
}

// TestNoContentHookConfiguredStillPasses is the other control: with nothing
// configured to scan content, an unavailable canonical costs nothing.
func TestNoContentHookConfiguredStillPasses(t *testing.T) {
	const withPII = `{"id":"x","object":"chat.completion","model":"gpt-4o",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"card 4111 1111 1111 1111"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	h := &Handler{deps: &Deps{
		HookConfigCache: newResponseHookCache(t, metadataOnlyHook{}),
		TrafficAdapter:  &openai.Adapter{},
		Logger:          noopLogger(),
	}}
	ingress := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), ingress))
	out := h.redactCanonicalBuffer(req, &audit.Record{RequestID: "no-content-hook"}, ingress,
		routingcore.RoutingTarget{ModelCode: "m", Region: "us-east-1"},
		[]byte(withPII), 0, "no-content-hook", noopLogger())

	if out.failClosed {
		t.Errorf("a response was refused with no content-scanning hook configured (status=%d "+
			"msg=%q); nothing was going to look at it either way", out.errStatus, out.errMsg)
	}
}

// metadataOnlyHook declares itself content-independent, the way rate-limit and
// IP-access hooks do. That declaration is what the pipeline reads: a hook that
// does NOT implement RawContentPrescanner is conservatively counted as
// content-scanning, because nothing proves otherwise. The control above needs a
// hook that actually proves it.
type metadataOnlyHook struct {
	goHooks.AnyEndpointAnyModality
}

func (metadataOnlyHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

func (metadataOnlyHook) ScansContent() bool        { return false }
func (metadataOnlyHook) MayMatchRaw(_ []byte) bool { return false }
