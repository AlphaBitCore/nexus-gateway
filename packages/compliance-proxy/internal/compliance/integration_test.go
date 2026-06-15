package compliance_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestSharedGoHooksPipeline verifies that the compliance-proxy can build and
// execute a hook pipeline using types and implementations from shared.
func TestSharedGoHooksPipeline(t *testing.T) {
	logger := testLogger()

	// Build hook configs that reference shared built-in implementations.
	configs := []core.HookConfig{
		{
			ID:               "kw-1",
			ImplementationID: "keyword-filter",
			Name:             "Block Secrets",
			Priority:         100,
			Enabled:          true,
			Stage:            "request",
			FailBehavior:     "fail-open",
			TimeoutMs:        5000,
			Config: map[string]any{
				"patterns": []any{
					map[string]any{
						"pattern":  "secret-project",
						"category": "confidential",
						"severity": "hard",
					},
				},
				"caseSensitive": false,
			},
		},
	}

	// Create PolicyResolver using the shared builtins.Registry.
	resolver := pipeline.NewPolicyResolver(configs, builtins.Registry, logger)

	if !resolver.HasHooks("request") {
		t.Fatal("expected request hooks to be available")
	}

	// Build a HookInput with content that triggers the keyword filter.
	input := &core.HookInput{
		Stage:       "request",
		SourceIP:    "10.0.0.1",
		TargetHost:  "api.openai.com",
		Method:      "POST",
		Path:        "/v1/chat/completions",
		IngressType: "COMPLIANCE_PROXY",
		BodySize:    100,
		Normalized:  core.PayloadFromTextSegments([]string{"tell me about secret-project plans"}),
	}

	// Build and execute pipeline.
	pipeline, err := resolver.BuildPipeline(
		"request",
		"COMPLIANCE_PROXY",
		"", nil, // endpoint/modality unknown in integration test
		5*time.Second,
		15*time.Second,
		false,
		false,
		logger,
	)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}

	result := pipeline.Execute(context.Background(), input)
	if result.Decision != core.RejectHard {
		t.Errorf("expected REJECT_HARD, got %s (reason: %s)", result.Decision, result.Reason)
	}
	if len(result.HookResults) == 0 {
		t.Fatal("expected at least one hook result")
	}
	if result.HookResults[0].ReasonCode != "KEYWORD_BLOCKED" {
		t.Errorf("expected KEYWORD_BLOCKED, got %s", result.HookResults[0].ReasonCode)
	}
}

// TestSharedGoApprove verifies clean content passes through.
func TestSharedGoApprove(t *testing.T) {
	logger := testLogger()

	configs := []core.HookConfig{
		{
			ID:               "kw-1",
			ImplementationID: "keyword-filter",
			Name:             "Block Secrets",
			Priority:         100,
			Enabled:          true,
			Stage:            "request",
			FailBehavior:     "fail-open",
			TimeoutMs:        5000,
			Config: map[string]any{
				"patterns": []any{
					map[string]any{
						"pattern":  "forbidden-word",
						"category": "blocked",
						"severity": "hard",
					},
				},
			},
		},
	}

	resolver := pipeline.NewPolicyResolver(configs, builtins.Registry, logger)

	input := &core.HookInput{
		Stage:       "request",
		SourceIP:    "10.0.0.1",
		TargetHost:  "api.openai.com",
		Method:      "POST",
		Path:        "/v1/chat/completions",
		IngressType: "COMPLIANCE_PROXY",
		BodySize:    50,
		Normalized:  core.PayloadFromTextSegments([]string{"this is perfectly fine content"}),
	}

	pipeline, err := resolver.BuildPipeline("request", "COMPLIANCE_PROXY", "", nil, 5*time.Second, 15*time.Second, false, false, logger)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}

	result := pipeline.Execute(context.Background(), input)
	if result.Decision != core.Approve {
		t.Errorf("expected APPROVE, got %s (reason: %s)", result.Decision, result.Reason)
	}
}
