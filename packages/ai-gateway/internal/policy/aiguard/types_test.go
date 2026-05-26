package aiguard

import (
	"encoding/json"
	"testing"
)

func TestRequest_JSONShapeMatchesSpec(t *testing.T) {
	req := Request{
		DetectorType: "prompt_injection",
		Content:      "hi",
		Context: Context{
			Ingress:        "AI_GATEWAY",
			TargetProvider: "openai",
			TargetModel:    "gpt-4o-mini",
			UpstreamTags:   []string{"severity:confidential"},
			HookName:       "prompt-injection-v1",
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"detector_type", "content", "context"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
	ctx := m["context"].(map[string]any)
	for _, k := range []string{"ingress", "target_provider", "target_model", "upstream_tags", "hook_name"} {
		if _, ok := ctx[k]; !ok {
			t.Errorf("missing context.%s in %s", k, b)
		}
	}
}

func TestResponse_JSONShapeMatchesSpec(t *testing.T) {
	resp := Response{
		Decision:   "reject_hard",
		Confidence: 0.9,
		Reason:     "x",
		Labels:     []string{"prompt_injection"},
		Metadata: Metadata{
			JudgeModel:     "claude-haiku",
			JudgeLatencyMs: 100,
			CacheHit:       false,
			BackendMode:    "configured_provider",
		},
	}
	b, _ := json.Marshal(resp)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"decision", "confidence", "reason", "labels", "metadata"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
}
