package proxy

// proxy_l2_embedding_input.go — canonical-message → embedding-input string
// construction for the L2 semantic cache. Split out of proxy_l2.go (which
// owns L2 read/write orchestration) along the "build the text the embedding
// model sees" responsibility seam.

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// canonicalMsgsToInputStaging converts []normcore.Message (from the canonical
// NormalizedPayload) to []inputstaging.Message, joining all text content blocks
// into a single string per message.  Images, tool calls, and tool results are
// omitted — inputstaging only reasons over text content.
func canonicalMsgsToInputStaging(msgs []normcore.Message) []inputstaging.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]inputstaging.Message, 0, len(msgs))
	for _, m := range msgs {
		text := joinTextBlocksL2(m.Content)
		if text == "" {
			continue
		}
		out = append(out, inputstaging.Message{
			Role:    string(m.Role),
			Content: text,
		})
	}
	return out
}

// joinTextBlocksL2 concatenates all ContentText blocks in a ContentBlock slice,
// separated by a space.  Non-text blocks (images, tool calls, tool results)
// are omitted.
func joinTextBlocksL2(blocks []normcore.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == normcore.ContentText && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// buildEmbeddingInput runs inputstaging.Plan on the canonical messages and
// joins the result into a single string.  Returns ("", false) when the
// messages are empty or the plan produces no output.
func buildEmbeddingInput(msgs []normcore.Message, strategy inputstaging.Strategy, maxInputTokens int) (string, bool) {
	stagingMsgs := canonicalMsgsToInputStaging(msgs)
	if len(stagingMsgs) == 0 {
		return "", false
	}
	if !strategy.Valid() {
		strategy = inputstaging.StrategySystemPlusLastUser
	}
	// Truncate the embed input to the embedding model's real context window
	// (capabilityJson.embeddings.max_input_tokens, carried on the snapshot).
	// A large chat context is trimmed to fit so the embedding call never 400s
	// on a token-limit. Fall back to a conservative 8192 when the model
	// declares no limit (or the fleet singleton is not configured yet);
	// inputstaging.Plan hard-fails only on ModelContextLimit < 1.
	contextLimit := maxInputTokens
	if contextLimit < 1 {
		contextLimit = 8192
	}
	plan, planErr := inputstaging.Plan(inputstaging.PlanInput{
		Messages:          stagingMsgs,
		ModelContextLimit: contextLimit,
		Strategy:          strategy,
		// ReportOnly: the staged text is semantic-cache key material and an
		// empty plan must stay a skip-the-cache signal. Plan's default budget
		// enforcement (re-seed + in-message cut) would silently change
		// embedding inputs and therefore cache-hit patterns. The hard bound
		// for this path is the explicit TruncateToTokens below.
		ReportOnly: true,
	})
	if planErr != nil || len(plan.Messages) == 0 {
		return "", false
	}
	var sb strings.Builder
	for i, m := range plan.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.Content)
	}
	// Last-resort hard cut: Plan drops whole messages but never cuts within one,
	// so a single oversized message would still exceed the embedding model's
	// limit and 400. Keep the newest content (tail) within the model's window.
	return inputstaging.TruncateToTokens(sb.String(), contextLimit), true
}
