package contract

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

// Example holds one canonical contract entry.
type Example struct {
	Name             string
	Config           core.HookConfig
	Input            *core.HookInput
	ExpectedDecision core.Decision
}

// Examples returns the full contract matrix in a stable order so test
// failures are easy to correlate across services. Add one entry per
// built-in hook × documented config shape.
func Examples() []Example {
	out := []Example{
		// --- pii-detector ---
		{
			Name: "pii_detector_email_block",
			Config: core.HookConfig{
				ID: "cp-pii-1", ImplementationID: "pii-detector",
				Name: "PII Email", Stage: "request", Priority: 10, Enabled: true,
				FailBehavior: "fail-open", TimeoutMs: 5000,
				Config: map[string]any{
					"action": "block",
					"patternDefinitions": []any{
						map[string]any{
							"id":    "email",
							"regex": `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
							"flags": "i",
						},
					},
				},
			},
			Input: &core.HookInput{
				Stage:       "request",
				IngressType: "AI_GATEWAY",
				Normalized:  core.PayloadFromTextSegments([]string{"contact me at user@example.com"}),
			},
			ExpectedDecision: core.RejectHard,
		},
		{
			Name: "pii_detector_clean_approve",
			Config: core.HookConfig{
				ID: "cp-pii-2", ImplementationID: "pii-detector",
				Name: "PII Email Clean", Stage: "request", Priority: 10, Enabled: true,
				FailBehavior: "fail-open", TimeoutMs: 5000,
				Config: map[string]any{
					"action": "block",
					"patternDefinitions": []any{
						map[string]any{
							"id":    "email",
							"regex": `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
							"flags": "i",
						},
					},
				},
			},
			Input: &core.HookInput{
				Stage:       "request",
				IngressType: "AI_GATEWAY",
				Normalized:  core.PayloadFromTextSegments([]string{"hello world no pii here"}),
			},
			ExpectedDecision: core.Approve,
		},

		// --- keyword-filter ---
		{
			Name: "keyword_filter_reject_hard",
			Config: core.HookConfig{
				ID: "cp-kw-1", ImplementationID: "keyword-filter",
				Name: "Block forbidden", Stage: "request", Priority: 20, Enabled: true,
				FailBehavior: "fail-open",
				Config: map[string]any{
					"caseSensitive": false,
					"patterns": []any{
						map[string]any{
							"pattern":  `forbidden-phrase`,
							"category": "blocked",
							"severity": "hard",
						},
					},
				},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
				Normalized: core.PayloadFromTextSegments([]string{"this contains a FORBIDDEN-PHRASE somewhere"}),
			},
			ExpectedDecision: core.RejectHard,
		},

		// --- content-safety ---
		{
			Name: "content_safety_violence_block",
			Config: core.HookConfig{
				ID: "cp-cs-1", ImplementationID: "content-safety",
				Name: "Violence", Stage: "request", Priority: 30, Enabled: true,
				FailBehavior: "fail-open",
				Config: map[string]any{
					"categories": map[string]any{"violence": true},
					"action":     "reject_hard",
				},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
				Normalized: core.PayloadFromTextSegments([]string{"the plot involves kill and murder."}),
			},
			ExpectedDecision: core.RejectHard,
		},

		// --- rate-limiter ---
		{
			Name: "rate_limiter_under_limit_approve",
			Config: core.HookConfig{
				ID: "cp-rl-1", ImplementationID: "rate-limiter",
				Name: "RL", Stage: "request", Priority: 40, Enabled: true,
				FailBehavior: "fail-open",
				Config: map[string]any{
					"maxRequests":   100,
					"windowSeconds": 60,
					"keyType":       "source_ip",
				},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY", SourceIP: "10.0.0.1",
			},
			ExpectedDecision: core.Approve,
		},

		// --- request-size-validator ---
		{
			Name: "request_size_under_limit_approve",
			Config: core.HookConfig{
				ID: "cp-rs-1", ImplementationID: "request-size-validator",
				Name: "Size", Stage: "request", Priority: 50, Enabled: true,
				FailBehavior: "fail-open",
				Config:       map[string]any{"maxSizeBytes": 1024 * 1024},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
				BodySize: 1024,
			},
			ExpectedDecision: core.Approve,
		},

		// --- ip-access-filter ---
		{
			Name: "ip_access_allowlist_match_approve",
			Config: core.HookConfig{
				ID: "cp-ip-1", ImplementationID: "ip-access-filter",
				Name: "IP ACL", Stage: "request", Priority: 60, Enabled: true,
				FailBehavior: "fail-open",
				Config: map[string]any{
					"mode":      "allowlist",
					"allowlist": []any{"10.0.0.0/8"},
				},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY", SourceIP: "10.5.5.5",
			},
			ExpectedDecision: core.Approve,
		},

		// --- data-residency ---
		{
			Name: "data_residency_no_upstream_tag_approve",
			Config: core.HookConfig{
				ID: "cp-dr-1", ImplementationID: "data-residency",
				Name: "DR", Stage: "request", Priority: 70, Enabled: true,
				FailBehavior: "fail-open",
				Config: map[string]any{
					"policies": []any{
						map[string]any{
							"classification": "CONFIDENTIAL",
							"allowedRegions": []any{"eu-west-1"},
						},
					},
				},
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
				// No UpstreamTags → approve path
			},
			ExpectedDecision: core.Approve,
		},

		// --- noop ---
		{
			Name: "noop_always_approve",
			Config: core.HookConfig{
				ID: "cp-noop-1", ImplementationID: "noop",
				Name: "Noop", Stage: "request", Priority: 99, Enabled: true,
				FailBehavior: "fail-open",
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
			},
			ExpectedDecision: core.Approve,
		},
	}
	out = append(out, aiGuardDecisions()...)
	return out
}

// aiGuardDecisions returns drift-guard fixtures that keep every consumer's
// test binary sensitive to renames/removals of core.Decision constants.
// If any Decision constant is renamed, every service fails to build on
// this file.
func aiGuardDecisions() []Example {
	return []Example{
		{
			Name: "ai_guard_decision_reject_hard_shape",
			Config: core.HookConfig{
				ID: "cp-aiguard-shape-1", ImplementationID: "noop",
				Name: "aiguard-shape", Stage: "request", Priority: 999, Enabled: true,
				FailBehavior: "fail-open",
			},
			Input: &core.HookInput{
				Stage: "request", IngressType: "AI_GATEWAY",
			},
			// noop always approves; the drift-guard shape is merely the
			// presence of this fixture in every consumer's test binary.
			// When core.Approve / RejectHard are renamed, every service
			// fails to build on this file.
			ExpectedDecision: core.Approve,
		},
	}
}
