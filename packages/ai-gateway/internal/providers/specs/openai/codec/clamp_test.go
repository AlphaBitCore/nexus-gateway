package codec

// max_tokens / max_completion_tokens clamp: an OpenAI-family model 400s
// ("max_tokens is too large: N. This model supports at most M ...") when the
// caller asks for more output than its ceiling. The codec clamps the existing
// value to the ceiling on BOTH doors; it never fills an absent value, and a
// zero/unknown ceiling is a no-op.

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func chatTarget(model string, maxOut int) provcore.CallTarget {
	return provcore.CallTarget{ProviderModelID: model, MaxOutputTokens: maxOut}
}

func TestClampMaxTokens_OverCeiling_BothDoors(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":200000}`)
	target := chatTarget("m", 128000)

	// Canonical door (EncodeRequest chat).
	enc, err := plain().EncodeRequest(typology.WireShapeOpenAIChat, body, target)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(enc.Body, "max_tokens").Int(); got != 128000 {
		t.Errorf("EncodeRequest door: max_tokens = %d, want 128000 (clamped)", got)
	}
	if !hasRewrite(enc.Rewrites, "max_tokens→128000_model_max") {
		t.Errorf("EncodeRequest door: missing clamp rewrite: %v", enc.Rewrites)
	}

	// Native door (RewriteNative chat, non-stream).
	rw, err := plain().RewriteNative(typology.WireShapeOpenAIChat, body, target, false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if got := gjson.GetBytes(rw.Body, "max_tokens").Int(); got != 128000 {
		t.Errorf("RewriteNative door: max_tokens = %d, want 128000 (clamped)", got)
	}
	// Two-door parity: both produce the same clamped value.
	if gjson.GetBytes(enc.Body, "max_tokens").Int() != gjson.GetBytes(rw.Body, "max_tokens").Int() {
		t.Errorf("door parity broken: enc=%s native=%s", enc.Body, rw.Body)
	}
}

func TestClampMaxTokens_MaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"max_completion_tokens":300000}`)
	enc, err := plain().EncodeRequest(typology.WireShapeOpenAIChat, body, chatTarget("m", 128000))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(enc.Body, "max_completion_tokens").Int(); got != 128000 {
		t.Errorf("max_completion_tokens = %d, want 128000 (clamped)", got)
	}
}

func TestClampMaxTokens_AtOrUnderCeiling_Untouched(t *testing.T) {
	for _, mt := range []int{128000, 4096, 1} {
		body := []byte(`{"model":"m","messages":[],"max_tokens":` + itoa(mt) + `}`)
		enc, err := plain().EncodeRequest(typology.WireShapeOpenAIChat, body, chatTarget("m", 128000))
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if got := gjson.GetBytes(enc.Body, "max_tokens").Int(); got != int64(mt) {
			t.Errorf("max_tokens=%d (<=ceiling) must be untouched, got %d", mt, got)
		}
		if len(enc.Rewrites) != 0 {
			t.Errorf("no clamp rewrite expected for in-range max_tokens=%d: %v", mt, enc.Rewrites)
		}
	}
}

func TestClampMaxTokens_Absent_NeverFilled(t *testing.T) {
	// OpenAI treats max_tokens as optional — an absent field stays absent.
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	enc, err := plain().EncodeRequest(typology.WireShapeOpenAIChat, body, chatTarget("m", 128000))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(enc.Body, "max_tokens").Exists() {
		t.Errorf("absent max_tokens must NOT be filled: %s", enc.Body)
	}
	if gjson.GetBytes(enc.Body, "max_completion_tokens").Exists() {
		t.Errorf("absent max_completion_tokens must NOT be filled: %s", enc.Body)
	}
}

func TestClampMaxTokens_UnknownCeiling_NoOp(t *testing.T) {
	// MaxOutputTokens<=0 (capability unknown) must not coerce to a wrong number.
	body := []byte(`{"model":"m","messages":[],"max_tokens":200000}`)
	enc, err := plain().EncodeRequest(typology.WireShapeOpenAIChat, body, chatTarget("m", 0))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(enc.Body, "max_tokens").Int(); got != 200000 {
		t.Errorf("unknown ceiling must be a no-op, max_tokens=%d want 200000", got)
	}
}

func TestClampMaxTokens_StreamingDoor(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"max_tokens":200000,"stream":true}`)
	rw, err := plain().RewriteNative(typology.WireShapeOpenAIChat, body, chatTarget("m", 128000), true)
	if err != nil {
		t.Fatalf("RewriteNative stream: %v", err)
	}
	if got := gjson.GetBytes(rw.Body, "max_tokens").Int(); got != 128000 {
		t.Errorf("streaming door: max_tokens = %d, want 128000 (clamped)", got)
	}
}

func hasRewrite(rw []string, want string) bool {
	for _, r := range rw {
		if r == want {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
