package event

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestBuildDetails(t *testing.T) {
	e := &Event{
		DestIP:        "10.0.0.1",
		DestPort:      443,
		BytesIn:       1024,
		BytesOut:      2048,
		PolicyRuleID:  "rule-1",
		OSUser:        "alice",
		SourceProcess: "/usr/bin/curl",
	}
	e.BuildDetails()

	if e.Details == nil {
		t.Fatal("Details is nil")
	}

	var d map[string]any
	if err := json.Unmarshal(e.Details, &d); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}

	if d["destIp"] != "10.0.0.1" {
		t.Fatalf("destIp = %v, want 10.0.0.1", d["destIp"])
	}
	if d["policyRuleId"] != "rule-1" {
		t.Fatalf("policyRuleId = %v, want rule-1", d["policyRuleId"])
	}
}

func TestBuildDetails_Empty(t *testing.T) {
	e := &Event{ID: "test-empty"}
	e.BuildDetails()
	if e.Details != nil {
		t.Fatal("Details should be nil for empty event")
	}
}

func TestEvent_E18Signals_JSONRoundTrip(t *testing.T) {
	in := &Event{
		ID:                "evt-1",
		ProviderName:      "anthropic",
		ModelName:         "claude-3-5-sonnet-20241022",
		ApiKeyClass:       "sk-ant-",
		ApiKeyFingerprint: "deadbeefdeadbeef",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Event
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ProviderName != in.ProviderName {
		t.Fatalf("ProviderName = %q, want %q", out.ProviderName, in.ProviderName)
	}
	if out.ModelName != in.ModelName {
		t.Fatalf("ModelName = %q, want %q", out.ModelName, in.ModelName)
	}
	if out.ApiKeyClass != in.ApiKeyClass {
		t.Fatalf("ApiKeyClass = %q, want %q", out.ApiKeyClass, in.ApiKeyClass)
	}
	if out.ApiKeyFingerprint != in.ApiKeyFingerprint {
		t.Fatalf("ApiKeyFingerprint = %q, want %q", out.ApiKeyFingerprint, in.ApiKeyFingerprint)
	}
}

func TestEvent_E18Signals_OmitEmpty(t *testing.T) {
	e := &Event{ID: "evt-empty"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"providerName", "modelName", "apiKeyClass", "apiKeyFingerprint"} {
		if _, present := m[k]; present {
			t.Fatalf("expected %q to be omitted on empty event, got presence", k)
		}
	}
}
