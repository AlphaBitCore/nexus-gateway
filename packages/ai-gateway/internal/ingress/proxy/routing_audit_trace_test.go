package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestRecordWalk_ReconstructsTheChain is the observability gate for the
// dispatch walk.
//
// Selection follows the failure now, not position, so a chain that jumped over
// entries is either a deliberate move — an overflow reaching for the largest
// window, a rate limit stepping off a provider — or a bug. From the plan alone
// those look the same. The invariant that used to police this asked whether
// every passed-over target had a named reason, and it asked it positionally, so
// it stopped meaning anything the moment position stopped deciding.
//
// What an operator has to be able to do at 3am is replay the walk: what was
// tried, in what order, why that order, and what each one cost.
func TestRecordWalk_ReconstructsTheChain(t *testing.T) {
	rec := &audit.Record{RoutingTrace: &routingAuditTrace{}}

	tgt := func(provider, model string) routingcore.RoutingTarget {
		return routingcore.RoutingTarget{ProviderName: provider, ModelCode: model}
	}
	recordWalk(rec, []executor.Attempt{
		{Target: tgt("openai", "gpt-4o"), Dispatched: true, StatusCode: 429,
			Code: "rate_limited", SelectionReason: "next-in-list", LatencyMs: 120},
		{Target: tgt("anthropic", "claude"), Dispatched: true, StatusCode: 400,
			Code: "context_overflow", SelectionReason: "different-provider", LatencyMs: 90},
		{Target: tgt("google", "gemini-pro"), Dispatched: true, StatusCode: 200,
			SelectionReason: "largest-window", LatencyMs: 300},
		{Target: tgt("cohere", "command"), SelectionReason: "next-in-list",
			Error: "skipped: upstream call budget spent"},
	})

	trace := rec.RoutingTrace.(*routingAuditTrace)
	if len(trace.Attempts) != 4 {
		t.Fatalf("recorded %d attempts, want 4 — the target passed over belongs in the "+
			"record too; it is the one an operator is most often accounting for", len(trace.Attempts))
	}

	// Order is the chain, so the sequence numbers have to say so.
	for i, a := range trace.Attempts {
		if a.Seq != i+1 {
			t.Errorf("attempt %d carries seq %d", i, a.Seq)
		}
	}

	// The jump that needs explaining: third in the chain, chosen for its
	// window after the second overflowed.
	if got := trace.Attempts[2].SelectionReason; got != "largest-window" {
		t.Errorf("selection reason %q — without it this hop is indistinguishable from a bug", got)
	}
	if trace.Attempts[1].Code != "context_overflow" {
		t.Errorf("the failure that caused the jump was not recorded: %q", trace.Attempts[1].Code)
	}

	// A skipped target must not read as an upstream call.
	if trace.Attempts[3].Dispatched {
		t.Error("a target that was passed over is recorded as having been dispatched")
	}

	// It must survive the trip to the column it lives in.
	blob, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{"largest-window", "different-provider", "context_overflow", "gemini-pro"} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("%q did not reach the persisted trace", want)
		}
	}
}

// TestRecordWalk_CarriesTheErrorClassToTheRow.
//
// Status and code do not cover what happened. A transport failure has neither
// — status 0, no provider envelope — and so does a provider error whose code
// the classifier did not recognise, so on the row those two look identical
// while needing opposite investigations. The class is what the walk actually
// branched on, and until it is projected here it lives only inside the
// executor.
func TestRecordWalk_CarriesTheErrorClassToTheRow(t *testing.T) {
	rec := &audit.Record{RoutingTrace: &routingAuditTrace{}}
	recordWalk(rec, []executor.Attempt{
		{Target: routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"},
			Dispatched: true, ErrorClass: "network", SelectionReason: "next-in-list"},
		{Target: routingcore.RoutingTarget{ProviderName: "anthropic", ModelCode: "claude"},
			Dispatched: true, ErrorClass: "unclassified", SelectionReason: "different-provider"},
	})

	trace := rec.RoutingTrace.(*routingAuditTrace)
	if trace.Attempts[0].ErrorClass != "network" || trace.Attempts[1].ErrorClass != "unclassified" {
		t.Fatalf("classes on the row = %q, %q — neither attempt carries a status or a code, so "+
			"dropping the class leaves the two indistinguishable",
			trace.Attempts[0].ErrorClass, trace.Attempts[1].ErrorClass)
	}

	blob, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"errorClass":"network"`, `"errorClass":"unclassified"`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("%s did not reach the persisted trace: %s", want, blob)
		}
	}
}

// TestRecordWalk_CarriesWhatWeRewroteToTheRow.
//
// A coercion is the gateway changing the caller's request — renaming
// max_tokens for a reasoning model, coercing a size to an aspect ratio. The
// only place that said so was a response header, which reaches a caller who
// discards it; the operator asking "what did we change" hours later has this
// row and nothing else, and the adapter contract says a field we coerced is a
// field we own.
//
// Per-attempt, because a coercion is per-target: the same request translated
// for two wires is rewritten differently, so one flat list per request would
// attribute the third target's rewrite to the first.
func TestRecordWalk_CarriesWhatWeRewroteToTheRow(t *testing.T) {
	rec := &audit.Record{RoutingTrace: &routingAuditTrace{}}
	recordWalk(rec, []executor.Attempt{
		{Target: routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-5"},
			Dispatched: true, StatusCode: 400, Code: "invalid_request",
			Coerced: []string{"max_tokens→max_completion_tokens"}},
		{Target: routingcore.RoutingTarget{ProviderName: "anthropic", ModelCode: "claude"},
			Dispatched: true, StatusCode: 200},
	})

	trace := rec.RoutingTrace.(*routingAuditTrace)
	if len(trace.Attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(trace.Attempts))
	}
	got := trace.Attempts[0].Coerced
	if len(got) != 1 || got[0] != "max_tokens→max_completion_tokens" {
		t.Errorf("the row records %v for the attempt we rewrote — the rewrite that preceded "+
			"this 400 then exists only in a header the caller has already thrown away", got)
	}
	if len(trace.Attempts[1].Coerced) != 0 {
		t.Errorf("the second attempt records %v; nothing was rewritten for it, and saying "+
			"otherwise attributes the first target's translation to a different wire",
			trace.Attempts[1].Coerced)
	}
}
