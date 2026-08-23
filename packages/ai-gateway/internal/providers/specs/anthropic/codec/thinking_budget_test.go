package codec

import (
	"errors"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// Anthropic, probed 2026-08-06 against claude-haiku-4-5:
//
//	max_tokens 2048, budget 1024 -> 200
//	max_tokens 1024, budget 1024 -> 400 max_tokens must be greater than thinking.budget_tokens
//	max_tokens 1024, budget  512 -> 400 budget_tokens must be >= 1024
//
// The thinking block is forwarded verbatim on the grounds that the gateway
// does not validate it. That holds only while the numbers are the caller's —
// max_tokens is clamped by us to the model ceiling, or filled by us when the
// caller omitted it, so our own coercion can make a consistent pair
// inconsistent and hand the caller a 400 for a request they never sent.

func encodeWithThinking(t *testing.T, maxTok int, budget int, modelLimit int) (map[string]any, []string, error) {
	t.Helper()
	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}],` +
		`"max_completion_tokens":` + itoa(maxTok) +
		`,"nexus":{"ext":{"anthropic":{"thinking":{"type":"enabled","budget_tokens":` + itoa(budget) + `}}}}}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: modelLimit})
	if err != nil {
		return nil, nil, err
	}
	root := gjson.ParseBytes(res.Body)
	out := map[string]any{
		"max_tokens": root.Get("max_tokens").Int(),
		"budget":     root.Get("thinking.budget_tokens").Int(),
	}
	return out, res.Rewrites, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A consistent pair passes through untouched — the reconciliation must not
// rewrite requests that were already valid.
func TestThinkingBudget_ConsistentPairUntouched(t *testing.T) {
	out, rewrites, err := encodeWithThinking(t, 4096, 1024, 8192)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out["max_tokens"] != int64(4096) || out["budget"] != int64(1024) {
		t.Errorf("got max_tokens=%v budget=%v, want 4096/1024 unchanged", out["max_tokens"], out["budget"])
	}
	for _, r := range rewrites {
		if strings.Contains(r, "budget_tokens") {
			t.Errorf("a valid pair was rewritten: %v", rewrites)
		}
	}
}

// The defect: our clamp lowers max_tokens to the model ceiling and strands a
// budget above it. Anthropic then rejects a pair the caller never sent.
func TestThinkingBudget_OurClampMustNotStrandTheBudget(t *testing.T) {
	// Caller asked for 8192; the model ceiling is 2048, so the clamp fires.
	out, rewrites, err := encodeWithThinking(t, 8192, 4096, 2048)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	maxTok := out["max_tokens"].(int64)
	budget := out["budget"].(int64)
	if maxTok != 2048 {
		t.Fatalf("max_tokens=%d, want the model ceiling 2048", maxTok)
	}
	if budget >= maxTok {
		t.Errorf("budget=%d is not below max_tokens=%d — Anthropic rejects this pair, and we built it", budget, maxTok)
	}
	if budget < anthropicMinThinkingBudget {
		t.Errorf("budget=%d is under Anthropic's floor of %d", budget, anthropicMinThinkingBudget)
	}
	var recorded bool
	for _, r := range rewrites {
		if strings.Contains(r, "budget_tokens") {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("the budget changed without a coercion record: %v", rewrites)
	}
}

// A budget at or above max_tokens straight from the caller is also repaired
// rather than forwarded to earn a 400.
func TestThinkingBudget_EqualToMaxTokensIsRepaired(t *testing.T) {
	out, _, err := encodeWithThinking(t, 4096, 4096, 8192)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out["budget"].(int64) >= out["max_tokens"].(int64) {
		t.Errorf("budget %v not strictly below max_tokens %v", out["budget"], out["max_tokens"])
	}
}

// When the cap cannot house the 1024 floor, thinking is impossible inside it.
// Refused here so the message can name both bounds; Anthropic's own error
// reports only whichever it checked first.
func TestThinkingBudget_CapTooSmallForTheFloorIsRefused(t *testing.T) {
	_, _, err := encodeWithThinking(t, 512, 2048, 512)
	if err == nil {
		t.Fatal("a cap below the thinking floor must be refused, not forwarded to earn a provider 400")
	}
	var pe *provcore.ProviderError
	if !asProviderError(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T", err)
	}
	if !strings.Contains(pe.Message, "1024") || !strings.Contains(pe.Message, "512") {
		t.Errorf("the message must name both the floor and the cap, got %q", pe.Message)
	}
}

func asProviderError(err error, out **provcore.ProviderError) bool {
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if ok {
		*out = pe
	}
	return ok
}

// thinking disabled carries no budget to reconcile and must be left alone.
func TestThinkingBudget_DisabledIsUntouched(t *testing.T) {
	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}],` +
		`"max_completion_tokens":512,` +
		`"nexus":{"ext":{"anthropic":{"thinking":{"type":"disabled"}}}}}`
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, []byte(body),
		provcore.CallTarget{ProviderModelID: "claude-haiku-4-5", MaxOutputTokens: 512})
	if err != nil {
		t.Fatalf("disabled thinking must not be refused: %v", err)
	}
	if gjson.GetBytes(res.Body, "thinking.type").String() != "disabled" {
		t.Errorf("thinking block was altered: %s", res.Body)
	}
}
