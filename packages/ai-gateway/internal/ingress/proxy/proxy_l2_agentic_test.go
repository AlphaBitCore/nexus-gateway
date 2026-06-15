package proxy

import (
	"testing"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestTryL2Lookup_AgenticBypass pins the production fix for the semantic-cache
// agent-loop hazard: a request that declares tools, or whose transcript
// carries tool-role messages, must never consult the semantic reader (a
// similarity hit replays the previous tool call — observed live as 10
// identical failing freeze calls in 6 seconds), and the skip must be stamped
// with its own named reason.
func TestTryL2Lookup_AgenticBypass(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*l2ReadParams)
	}{
		{"tools declared (round 1)", func(p *l2ReadParams) { p.hasTools = true }},
		{"tool-role message in transcript", func(p *l2ReadParams) {
			p.canonicalMsgs = append(p.canonicalMsgs, normcore.Message{Role: normcore.RoleTool})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdr := &stubSemanticReader{}
			h := &Handler{deps: &Deps{SemanticReader: rdr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}
			p := makeTryParams(t)
			tc.mut(&p)
			if h.tryL2Lookup(p) {
				t.Error("agentic request must miss, never semantic-hit")
			}
			if rdr.called.Load() != 0 {
				t.Error("the semantic reader must not be consulted for an agentic request")
			}
			if p.rec.GatewayCacheSkipReason != audit.GatewayCacheSkipReasonAgenticToolUse {
				t.Errorf("skip reason = %q, want agentic_tool_use", p.rec.GatewayCacheSkipReason)
			}
		})
	}

	// A plain conversation is unaffected: it proceeds past the agentic guard
	// (here to the fleet gate; the reader-not-called assertion belongs to the
	// disabled-fleet test).
	plain := makeTryParams(t)
	h := &Handler{deps: &Deps{SemanticReader: &stubSemanticReader{}, SemanticConfigCache: semantic.NewConfigCache()}}
	if h.tryL2Lookup(plain) {
		t.Error("plain request with disabled fleet must miss")
	}
	if plain.rec.GatewayCacheSkipReason == audit.GatewayCacheSkipReasonAgenticToolUse {
		t.Error("a plain conversation must not be classified agentic")
	}
}

// TestScheduleL2Write_AgenticBypass: the write side is symmetric — an agent
// loop's turn never poisons the semantic index, whether flagged by the
// lookup-side stamp (round 1) or detected from tool-role messages.
func TestScheduleL2Write_AgenticBypass(t *testing.T) {
	wtr := newStubWriter()
	h := &Handler{deps: &Deps{SemanticWriter: wtr, SemanticConfigCache: enabledFleetCache(), CredManager: &stubCredManager{}}}

	recStamped := &audit.Record{GatewayCacheSkipReason: audit.GatewayCacheSkipReasonAgenticToolUse}
	h.scheduleL2Write(recStamped, routingcore.RoutingTarget{}, sampleMsgs(), []byte(`{"ok":true}`), nil, false, Ingress{}, noopLogger())

	msgs := append(sampleMsgs(), normcore.Message{Role: normcore.RoleTool})
	h.scheduleL2Write(&audit.Record{}, routingcore.RoutingTarget{}, msgs, []byte(`{"ok":true}`), nil, false, Ingress{}, noopLogger())

	if n := wtr.called.Load(); n != 0 {
		t.Fatalf("agentic turns must never reach the semantic writer, got %d writes", n)
	}
}
