package validators

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// QualityChecker detects quality anomalies in responses: short responses,
// unexpected finish reasons, and refusal patterns.
// Applies to all text-carrying endpoints, text modality only, via the
// embedded TextOnlyContentScanning helper.
type QualityChecker struct {
	core.TextOnlyContentScanning
	minResponseLength     int
	expectedFinishReasons []string
	detectRefusals        bool
	refusalPatterns       []string
	onMatch               core.OnMatchConfig
}

// NewQualityChecker creates a quality-checker hook from config.
//
// Config shape:
//
//	{
//	  "minResponseLength":     10,
//	  "expectedFinishReasons": ["stop","end_turn"],
//	  "detectRefusals":        true,
//	  "refusalPatterns":       ["i can't help with", ...],
//	  "onMatch": {"inflightAction":"approve|block-soft", "storageAction":"keep"}
//	}
//
// onMatch.inflightAction = "approve" → log-only (anomaly tagged, request
// flows through). "block-soft" → reject with HTTP 246. Other inflight values
// (block-hard, redact) are valid but uncommon; they are honored when set.
func NewQualityChecker(cfg *core.HookConfig) (core.Hook, error) {
	qc := &QualityChecker{
		minResponseLength:     10,
		expectedFinishReasons: []string{"stop", "end_turn"},
		detectRefusals:        true,
		refusalPatterns: []string{
			"i can't help with",
			"i cannot help with",
			"i'm not able to",
			"i am not able to",
			"as an ai",
			"i don't have the ability",
		},
	}

	if v, ok := cfg.Config["minResponseLength"].(float64); ok {
		qc.minResponseLength = int(v)
	}
	if v, ok := cfg.Config["expectedFinishReasons"].([]any); ok {
		qc.expectedFinishReasons = nil
		for _, item := range v {
			if s, ok := item.(string); ok {
				qc.expectedFinishReasons = append(qc.expectedFinishReasons, s)
			}
		}
	}
	if v, ok := cfg.Config["detectRefusals"].(bool); ok {
		qc.detectRefusals = v
	}
	if v, ok := cfg.Config["refusalPatterns"].([]any); ok {
		qc.refusalPatterns = nil
		for _, item := range v {
			if s, ok := item.(string); ok {
				qc.refusalPatterns = append(qc.refusalPatterns, s)
			}
		}
	}

	// Quality checker defaults to approve (log-only) when onMatch is absent,
	// unlike other content hooks that default to block-hard.
	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("quality-checker: %w", err)
	}
	if _, explicit := cfg.Config["onMatch"]; !explicit {
		onMatch.InflightAction = core.InflightApprove
	}
	qc.onMatch = onMatch

	return qc, nil
}

func (qc *QualityChecker) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	// Extract response text. Prefer assistant messages in the normalized
	// payload; fall back to any text content; finally to the HookInput's
	// metadata FinishReason when no body was captured.
	finishReason := input.FinishReason
	var text string
	if input.Normalized != nil {
		// Prefer assistant-role messages.
		for _, m := range input.Normalized.Messages {
			if m.Role == "assistant" {
				for _, b := range m.Content {
					if b.Type == "text" && b.Text != "" {
						text = b.Text
						break
					}
				}
				if m.FinishReason != "" && finishReason == "" {
					finishReason = m.FinishReason
				}
			}
			if text != "" {
				break
			}
		}
		// Fallback: any text segment from the projection.
		if text == "" {
			if segs := input.Normalized.TextProjection(); len(segs) > 0 {
				text = segs[0]
			}
		}
		if finishReason == "" {
			finishReason = input.Normalized.FinishReason
		}
	}

	if text == "" && finishReason == "" {
		return &core.HookResult{Decision: core.Approve}, nil
	}

	var signals []string

	// Check minimum response length.
	if len(text) < qc.minResponseLength && text != "" {
		signals = append(signals, "short_response")
	}

	// Check finish reason.
	if finishReason != "" && !qc.isExpectedFinishReason(finishReason) {
		signals = append(signals, "unexpected_finish_reason:"+finishReason)
	}

	// Check refusal patterns.
	if qc.detectRefusals {
		lower := strings.ToLower(text)
		for _, pattern := range qc.refusalPatterns {
			if strings.Contains(lower, pattern) {
				signals = append(signals, "refusal_detected")
				break
			}
		}
	}

	if len(signals) == 0 {
		return &core.HookResult{Decision: core.Approve}, nil
	}

	reason := "quality signals: " + strings.Join(signals, ", ")
	decision := core.DecisionForInflight(qc.onMatch.InflightAction)
	reasonCode := "QUALITY_ANOMALY"
	if decision == core.Approve {
		reasonCode = "QUALITY_SIGNAL"
	}
	return &core.HookResult{
		Decision:   decision,
		Reason:     reason,
		ReasonCode: reasonCode,
	}, nil
}

func (qc *QualityChecker) isExpectedFinishReason(reason string) bool {
	for _, expected := range qc.expectedFinishReasons {
		if expected == reason {
			return true
		}
	}
	return false
}
