package core

import "testing"

func TestContentBlock(t *testing.T) {
	cb := ContentBlock{Role: "user", Type: "text", Text: "hello"}
	if cb.Text != "hello" {
		t.Fatalf("expected 'hello', got %q", cb.Text)
	}
}

func TestHookInput_Fields(t *testing.T) {
	input := &HookInput{
		Stage:       "request",
		Normalized:  PayloadFromTextSegments([]string{"test"}),
		SourceIP:    "1.2.3.4",
		TargetHost:  "api.openai.com",
		IngressType: "AI_GATEWAY",
	}
	if input.Stage != "request" {
		t.Fatalf("expected request, got %q", input.Stage)
	}
	segs := input.TextSegments()
	if len(segs) != 1 || segs[0] != "test" {
		t.Fatalf("unexpected text segments: %+v", segs)
	}
}

func TestPayloadFromTextSegments(t *testing.T) {
	p := PayloadFromTextSegments([]string{"hello", "world"})
	if p == nil {
		t.Fatal("nil payload")
	}
	segs := p.TextProjection()
	if len(segs) != 2 || segs[0] != "hello" || segs[1] != "world" {
		t.Fatalf("unexpected projection: %+v", segs)
	}
}

func TestHookResult_Tags_Roundtrip(t *testing.T) {
	r := HookResult{
		HookID: "h-1",
		Tags:   []string{"compliance:pii", "severity:confidential"},
	}
	if got := r.Tags; len(got) != 2 || got[0] != "compliance:pii" {
		t.Fatalf("unexpected tags: %+v", got)
	}
}

func TestHookInput_UpstreamTags_Roundtrip(t *testing.T) {
	in := HookInput{UpstreamTags: []string{"region:eu-only"}}
	if len(in.UpstreamTags) != 1 || in.UpstreamTags[0] != "region:eu-only" {
		t.Fatalf("unexpected upstream tags: %+v", in.UpstreamTags)
	}
}
