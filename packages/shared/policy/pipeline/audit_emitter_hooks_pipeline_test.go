package pipeline

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// request_hooks_pipeline / response_hooks_pipeline must not be
// json.Marshal(r.HookResults): that carries ModifiedContent — the full
// message text with only matched spans substituted — and TransformSpans,
// whose Replacement field carries text too. The storage gate governs the body
// columns and sees neither.
//
// The pair matters: the first alone would pass against a column that had been
// blanked, and blanking it would break the only UI that reads it.
func hookResultWithContent(text string) HookResult {
	return HookResult{
		Order:      0,
		HookID:     "h1",
		HookName:   "pii-redactor",
		Decision:   Modify,
		Action:     core.ActionRedact,
		Reason:     "matched rule email",
		ReasonCode: "PII_EMAIL",
		LatencyUs:  120,
		ModifiedContent: []ContentBlock{
			{Role: "user", Type: "text", Text: text},
		},
		TransformSpans: []normcore.TransformSpan{
			{SourceID: "email", Start: 8, End: 8 + len(text), Replacement: text},
		},
	}
}

func TestBuildEvent_HooksPipelineCarriesNoContent(t *testing.T) {
	text := "contact " + emailMarker + " now"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)

	// Redact with no rewritten wire copy: the gate withholds the body outright.
	result := &CompliancePipelineResult{
		Decision:    Modify,
		Action:      core.ActionRedact,
		HookResults: []HookResult{hookResultWithContent(text)},
	}
	evt := emitAndCapture(t, AuditInfo{TransactionID: "tx-hooks-gated"}, result, nil, raw, nil)

	// Precondition: without this the test says nothing about the gate.
	if evt.RequestBody.Kind != audit.BodyAbsent {
		t.Fatalf("the gate did not withhold the body, so there is nothing to compare the "+
			"pipeline column against: kind=%q", evt.RequestBody.Kind)
	}

	pipeline := string(evt.RequestHooksPipeline)
	if strings.Contains(pipeline, emailMarker) {
		t.Errorf("request_hooks_pipeline carries the content the storage gate just withheld "+
			"from the body column: %s", pipeline)
	}
	for _, field := range []string{"modifiedContent", "transformSpans"} {
		if strings.Contains(pipeline, field) {
			t.Errorf("%s is in-flight working state with no reader on the persisted row; "+
				"it must not be serialised. column=%s", field, pipeline)
		}
	}
}

func TestBuildEvent_HooksPipelineStillIdentifiesTheHook(t *testing.T) {
	// Approve, so nothing is withheld and the only reason the column could be
	// empty is that the projection dropped too much. The control-plane traffic
	// drawer renders exactly these fields.
	text := "nothing sensitive here"
	raw := []byte(`{"messages":[{"content":"` + text + `"}]}`)
	result := &CompliancePipelineResult{
		Decision:    Approve,
		Action:      core.ActionApprove,
		HookResults: []HookResult{hookResultWithContent(text)},
	}
	evt := emitAndCapture(t, AuditInfo{TransactionID: "tx-hooks-approved"}, result, nil, raw, nil)

	pipeline := string(evt.RequestHooksPipeline)
	for _, want := range []string{"pii-redactor", "h1", "PII_EMAIL", "matched rule email", "latencyUs"} {
		if !strings.Contains(pipeline, want) {
			t.Errorf("the column must still answer \"which hook did what\": %q missing from %s",
				want, pipeline)
		}
	}
}

// A hook's Error must reach the trace WHOLE, and specifically with its tail
// intact. Go's wrapping puts the cause last, and Hub's
// proxy_hook_timeout_rate aggregator decides by substring-matching
// "deadline exceeded" against this exact field — so a head-preserving
// truncation makes the timeout alert stop counting past a long enough
// endpoint URL, which is when a slow webhook most needs alerting on.
//
// The URL padding is load-bearing: at 200 characters the marker survives any
// 300-byte cut and the test would pass against the very bug it exists to
// catch. 400 puts the cause well past the cap.
func TestBuildEvent_HookErrorKeepsItsCause(t *testing.T) {
	longURL := "https://hooks.example.com/" + strings.Repeat("p", 400)
	hookErr := `Post "` + longURL + `": context deadline exceeded`

	result := &CompliancePipelineResult{
		Decision: Approve,
		Action:   core.ActionApprove,
		HookResults: []HookResult{{
			Order: 0, HookID: "h1", HookName: "aiguard", Decision: Approve,
			Error: hookErr,
		}},
	}
	evt := emitAndCapture(t, AuditInfo{TransactionID: "tx-hook-err"}, result, nil,
		[]byte(`{"messages":[]}`), nil)

	pipeline := string(evt.RequestHooksPipeline)
	if !strings.Contains(pipeline, "deadline exceeded") {
		t.Errorf("the hook error lost its cause, so Hub's timeout aggregator — which "+
			"substring-matches this field — stops counting the timeout: %s", pipeline)
	}
	if len(hookErr) < 300 {
		t.Fatalf("fixture is only %d bytes; a 300-byte cut would not reach the cause and "+
			"this test could not show the bug", len(hookErr))
	}
}
