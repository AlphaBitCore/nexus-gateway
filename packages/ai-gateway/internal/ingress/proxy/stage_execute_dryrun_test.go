package proxy

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// A dry-run rule MEASURES; it does not edit. The audit row describes what
// happened to this request, so its strip counts must be zero — the body went
// upstream untouched.
//
// Recording the would-have-stripped figure instead put a number nothing removed
// in front of four readers that all treat it as an edit: the traffic audit
// drawer shows it to an admin, cache ROI sums it into a savings figure, the 5m
// rollups aggregate it, and the Hub's cache-quality monitor counts the row as
// "normaliser-modified" — the last of which is why that job, after flipping
// every rule to dry-run, kept measuring the same population and could not
// observe whether its own remediation worked.
func TestServeProxy_DryRunRule_StampsZeroStripCounts(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})

	prod := &captureProducer{}
	auditWriter := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = auditWriter

	// The cch rule enabled but in dry-run: the engine reports what it WOULD
	// have removed and forwards the body untouched.
	on := true
	eng := wirerewrite.New(slog.Default())
	eng.Reload(wirerewrite.Config{Rules: map[string]map[string]wirerewrite.RuleOverride{
		wirerewrite.AdapterAnthropic: {
			wirerewrite.RuleAnthropicCchStrip: {Enabled: &on, DryRunAlways: &on},
		},
	}})
	deps.Normaliser = eng
	// Route to an ANTHROPIC target, or NormalizeUpstream is asked for the
	// openai rule set, finds nothing, and reports zero strips no matter what
	// the dry-run flag says — a green test proving nothing.
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID:      "p-anthropic",
		ProviderName:    "anthropic",
		ProviderModelID: "claude-x",
		ModelID:         "claude-x",
		ModelName:       "Claude X",
		ModelCode:       "claude-x",
		AdapterType:     "anthropic",
	}}}

	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	// A body carrying the rule's target, so a rule that DID edit would be
	// visible in the counts.
	body := `{"model":"claude-x","messages":[{"role":"system","content":"cch=deadbeef; and a stable operator playbook"},{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	auditWriter.Close()
	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) == 0 {
		t.Fatal("no audit message captured")
	}
	var evt mq.TrafficEventMessage
	if err := json.Unmarshal(msgs[0], &evt); err != nil {
		t.Fatalf("unmarshal audit envelope: %v", err)
	}
	if evt.NormalizedStripCount != nil && *evt.NormalizedStripCount != 0 {
		t.Errorf("a dry-run rule removed nothing, so the row must report 0 strips; got %d", *evt.NormalizedStripCount)
	}
	if evt.NormalizedStripBytes != nil && *evt.NormalizedStripBytes != 0 {
		t.Errorf("a dry-run rule removed nothing, so the row must report 0 stripped bytes; got %d", *evt.NormalizedStripBytes)
	}
}
