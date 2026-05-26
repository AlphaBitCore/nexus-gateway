package iam

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

// TestEngineEvaluate_AWSShapeParity proves the engine treats two
// representations of the same policy identically. Each test row builds
// the same effective grant twice — once with `"Action": "x"` /
// `"Resource": "y"` (single-string AWS form) and once with the array
// form — and asserts the engine's Evaluate decision matches for an
// arbitrary action/resource probe.
//
// This is the runtime guarantee that pasting a vendor-supplied AWS
// JSON policy into the Control Plane (where the AWS pattern of using
// single strings is common) doesn't change effective permissions
// relative to authoring the same policy through the form editor
// (which historically emitted arrays).
func TestEngineEvaluate_AWSShapeParity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cases := []struct {
		name         string
		singleJSON   string
		arrayJSON    string
		action       string
		resource     string
		wantDecision string
	}{
		{
			name: "wildcard everything: Action=*, Resource=*",
			singleJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":"*","Resource":"*"}
			]}`,
			arrayJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":["*"],"Resource":["*"]}
			]}`,
			action:       "admin:provider.read",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Allow",
		},
		{
			name: "single-action grant",
			singleJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":"admin:virtual-key.read","Resource":"nrn:nexus:gateway:*:virtual-key/*"}
			]}`,
			arrayJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":["admin:virtual-key.read"],"Resource":["nrn:nexus:gateway:*:virtual-key/*"]}
			]}`,
			action:       "admin:virtual-key.read",
			resource:     "nrn:nexus:gateway:*:virtual-key/vk-1",
			wantDecision: "Allow",
		},
		{
			name: "single-action grant — mismatched probe denies",
			singleJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":"admin:virtual-key.read","Resource":"*"}
			]}`,
			arrayJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":["admin:virtual-key.read"],"Resource":["*"]}
			]}`,
			action:       "admin:provider.delete",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Deny",
		},
		{
			name: "verb wildcard (AWS aiops:Get* pattern)",
			singleJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":"admin:provider.*","Resource":"*"}
			]}`,
			arrayJSON: `{"Version":"2026-03-28","Statement":[
				{"Effect":"Allow","Action":["admin:provider.*"],"Resource":["*"]}
			]}`,
			action:       "admin:provider.create",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Allow",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var singleDoc, arrayDoc PolicyDocument
			if err := json.Unmarshal([]byte(c.singleJSON), &singleDoc); err != nil {
				t.Fatalf("unmarshal single form: %v", err)
			}
			if err := json.Unmarshal([]byte(c.arrayJSON), &arrayDoc); err != nil {
				t.Fatalf("unmarshal array form: %v", err)
			}

			// Run both forms through the same engine with the same probe.
			for label, doc := range map[string]PolicyDocument{"single": singleDoc, "array": arrayDoc} {
				engine := NewEngine(&mockLoader{policies: []LoadedPolicy{{
					ID: "p1", Name: "p1", Source: "direct", Document: doc,
				}}}, logger)
				result, err := engine.Evaluate(context.Background(), "api_key", "test-key", c.action, c.resource, nil)
				if err != nil {
					t.Fatalf("[%s] Evaluate error: %v", label, err)
				}
				if result.Decision != c.wantDecision {
					t.Errorf("[%s] decision = %s; want %s (reason=%s)", label, result.Decision, c.wantDecision, result.Reason)
				}
			}
		})
	}
}
