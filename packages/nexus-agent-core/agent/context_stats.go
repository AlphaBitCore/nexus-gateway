package agent

// ContextStats reports how full the model's context is after a turn: the exact
// used / cached prompt tokens (from the gateway), plus a per-component estimate of
// what filled the context — the system prompt, the exposed tool schemas, the
// conversation history, and the per-turn context bundle — calibrated so the parts
// sum to Used. The gateway returns only a single prompt-token total per call, so
// the split is a local estimate; the totals are exact.
type ContextStats struct {
	Used   int // exact: prompt tokens of the turn's final model call
	Cached int // exact: cached prompt tokens

	System  int // estimated, calibrated
	Tools   int // estimated, calibrated
	History int // estimated, calibrated
	Bundle  int // estimated, calibrated

	Messages      int // persisted transcript message count (informational)
	CompactBudget int // estimated-token budget above which the model view is auto-trimmed
}

// estimateTokens approximates the token count of a string at ~4 characters per
// token — the common rough rule, and enough for a proportion view since the
// per-component parts are calibrated to the exact total.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// messageTokens estimates the tokens of one message (each block's text and
// tool-call arguments). Used by both the context indicator and the compactor's
// token-budget logic so they share one estimate.
func messageTokens(m Message) int {
	n := 0
	for _, b := range m.Blocks {
		n += estimateTokens(b.Text)
		n += estimateTokens(string(b.Input))
	}
	return n
}

// estimateMessages sums the estimated tokens of a transcript.
func estimateMessages(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += messageTokens(m)
	}
	return n
}

// estimateSchemas sums the estimated tokens of the exposed tool schemas.
func estimateSchemas(tools []ToolSchema) int {
	n := 0
	for _, t := range tools {
		n += estimateTokens(t.Name) + estimateTokens(t.Description) + estimateTokens(string(t.Parameters))
	}
	return n
}

// contextStats builds the calibrated stats for a completed turn. conv is the full
// turn transcript (which contains the per-turn bundle in its first user message);
// the bundle is subtracted out so history and bundle do not double-count. When the
// gateway reported a prompt-token total, the four estimated components are scaled so
// they sum to it exactly (History absorbs the rounding remainder); otherwise the raw
// estimates are returned with Used == 0.
func contextStats(system string, tools []ToolSchema, bundle string, conv []Message, usage *Usage) ContextStats {
	sys := estimateTokens(system)
	tl := estimateSchemas(tools)
	bn := estimateTokens(bundle)
	hist := estimateMessages(conv) - bn
	if hist < 0 {
		hist = 0
	}
	cs := ContextStats{System: sys, Tools: tl, Bundle: bn, History: hist}
	if usage != nil {
		cs.Used = usage.PromptTokens
		cs.Cached = usage.CachedTokens
	}
	if est := sys + tl + bn + hist; cs.Used > 0 && est > 0 {
		cs.System = sys * cs.Used / est
		cs.Tools = tl * cs.Used / est
		cs.Bundle = bn * cs.Used / est
		cs.History = cs.Used - cs.System - cs.Tools - cs.Bundle // remainder absorbs rounding
	}
	return cs
}
