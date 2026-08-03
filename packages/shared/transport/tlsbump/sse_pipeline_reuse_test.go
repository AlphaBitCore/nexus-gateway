package tlsbump

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// The SSE path builds ONE response pipeline per request (finding C-19). The scope
// routing builds it to decide the streaming mode, and live, buffer and Model A all
// reuse that instance instead of rebuilding it with identical arguments.
//
// These tests pin the two properties that made the deduplication safe, because
// neither was covered before: the invariant the live branch now relies on, and the
// fact that a reused pipeline still executes.

// countingHook records how many times the pipeline executed it, so a test can tell
// "the pipeline ran" from "the pipeline was wired to nil and silently did nothing" —
// which is exactly how finding C-17 hid for as long as it did.
type countingHook struct {
	runs *atomic.Int64
}

func (h countingHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	h.runs.Add(1)
	return &core.HookResult{
		HookID:   "h-count",
		HookName: "counting-hook",
		Decision: core.Approve,
	}, nil
}

func (h countingHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (h countingHook) SupportsModality(core.Modality) bool     { return true }

// countingResolver builds a resolver with one observe-only response hook. It carries
// no blocking or redacting capability, so MayBlock and MayRedact are both false and
// the scope routing leaves a "live" admin mode alone — the non-enforcing live path,
// which is the one that reuses the probe.
func countingResolver(t *testing.T, runs *atomic.Int64) *compliance.PolicyResolver {
	t.Helper()
	reg := core.NewHookRegistry()
	reg.Register("counting", func(_ *core.HookConfig) (core.Hook, error) {
		return countingHook{runs: runs}, nil
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-count",
			ImplementationID:  "counting",
			Name:              "counting-hook",
			Stage:             "response",
			Enabled:           true,
			FailBehavior:      "fail-open",
			ApplicableIngress: []string{"ALL"},
		},
	}, reg, discardSlog())
}

// TestNonStrictBuildPipeline_NeverErrors_FailOpenByConstruction pins the property
// the single-build hoist relies on. sse.go now builds one response pipeline at SSE
// entry and refuses with 451 on error; the per-mode build-failure fallbacks are gone
// because a non-strict caller cannot produce that error. Every error return inside
// BuildPipeline is gated on strictFailClosed AND FailBehavior=fail-closed, which is
// what makes the agent's host-packet path fail-open by construction rather than by a
// fallback branch (CLAUDE.md NE safety rule).
//
// If a future error return is added without the strict gate, the agent would start
// receiving 451s on intercepted SSE — a fail-CLOSED regression on the path where
// that is forbidden. This test fails first.
func TestNonStrictBuildPipeline_NeverErrors_FailOpenByConstruction(t *testing.T) {
	resolver := unbuildableFailClosedResolver(t, "response")

	pipeline, err := resolver.BuildPipeline(
		"response", "COMPLIANCE_PROXY",
		"", nil,
		time.Second, time.Second, false,
		false, // the agent's non-strict host-packet posture
		discardSlog(),
	)
	if err != nil {
		t.Fatalf("non-strict BuildPipeline must never error — the agent SSE path has no "+
			"build-failure fallback and would refuse host traffic with 451. got: %v", err)
	}
	if pipeline != nil {
		t.Fatalf("the only hook is an unbuildable fail-closed one, so a non-strict build "+
			"must degrade to no pipeline, got %p", pipeline)
	}

	// The same resolver under the appliance's strict posture DOES error — that is the
	// asymmetry the single build depends on, so both halves are asserted together.
	if _, strictErr := resolver.BuildPipeline(
		"response", "COMPLIANCE_PROXY",
		"", nil,
		time.Second, time.Second, false,
		true, // the compliance-proxy appliance
		discardSlog(),
	); strictErr == nil {
		t.Fatal("strict BuildPipeline must error on an unbuildable fail-closed hook: " +
			"the SSE entry guard's 451 depends on it")
	}
}

// TestSSE_LiveMode_ReusedPipelineStillExecutes is the equivalence proof for the C-19
// deduplication. The live branch no longer calls BuildPipeline; it uses the instance
// the scope routing built. If that wiring were wrong the pipeline would be nil, the
// stream would still be relayed byte-for-byte, and nothing would look broken from
// the client's side — the hook would simply never run. That is precisely the failure
// mode of finding C-17, so it is asserted on the hook's own execution count.
func TestSSE_LiveMode_ReusedPipelineStillExecutes(t *testing.T) {
	var runs atomic.Int64
	writer := &recordingAuditWriter{}

	store := streampolicy.NewStore(streampolicy.Policy{
		Mode:           streampolicy.ModeChunkedAsync, // "live"
		ChunkBytes:     1024,
		HookTimeoutMs:  1000,
		MaxBufferBytes: 1 << 20,
		FailBehavior:   streampolicy.FailOpen,
	})
	bo := &bumpOptions{
		policyResolver:       countingResolver(t, &runs),
		streamingPolicyStore: store,
		auditEmitter:         compliance.NewAuditEmitter(writer, discardSlog()),
	}

	rec := httptest.NewRecorder()
	respInput := &core.HookInput{Stage: "response", TargetHost: "api.example.com", IngressType: "COMPLIANCE_PROXY"}
	auditInfo := &compliance.AuditInfo{TransactionID: "tx-sse-reuse"}
	audCtx := &requestAuditCtx{input: respInput, info: *auditInfo}

	handleSSEResponse(context.Background(), rec, sseUpstreamResponse(), audCtx, respInput, auditInfo, bo, discardSlog(), time.Now())

	if got := runs.Load(); got == 0 {
		t.Fatal("the response hook never executed on the live path: the pipeline the scope " +
			"routing built was not reused, so checkpoint evaluation had nothing to run. " +
			"The stream still relays, which is why this is asserted on the hook and not on " +
			"the response body.")
	}
	// The stream must still reach the client: reusing the pipeline is a cost change,
	// not a delivery change.
	if body := rec.Body.String(); body == "" {
		t.Fatal("live mode relayed nothing to the client")
	}
}
