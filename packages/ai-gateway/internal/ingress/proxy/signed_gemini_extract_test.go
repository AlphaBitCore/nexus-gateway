package proxy

import "testing"

// The signed-call extractors run on EVERY canonical redaction, including the
// overwhelming majority of bodies that carry no signature at all. Their
// early-outs are the common path, not an edge case, and a wrong one either
// costs a walk on every request or silently drops the signature the invariant
// exists to protect.
func TestSignedGeminiExtractors_EarlyOuts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"choices is not an array", `{"choices":{"index":0}}`, 0},
		{"no choices at all", `{"id":"x"}`, 0},
		{"tool call without a signature", `{"choices":[{"message":{"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}"}}]}}]}`, 0},
		{"signature present but empty", `{"choices":[{"message":{"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}","thought_signature":""}}]}}]}`, 0},
		{"one signed call", `{"choices":[{"message":{"tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}","thought_signature":"s"}}]}}]}`, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(signedGeminiToolCalls([]byte(tt.body))); got != tt.want {
				t.Errorf("signedGeminiToolCalls=%d want %d", got, tt.want)
			}
		})
	}
}

func TestSignedGeminiRequestExtractor_EarlyOuts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"messages is not an array", `{"messages":{"role":"user"}}`, 0},
		{"no messages at all", `{"model":"m"}`, 0},
		{"tool call without a signature", `{"messages":[{"role":"assistant","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}"}}]}]}`, 0},
		{"one signed call", `{"messages":[{"role":"assistant","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}","thought_signature":"s"}}]}]}`, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(signedGeminiRequestToolCalls([]byte(tt.body))); got != tt.want {
				t.Errorf("signedGeminiRequestToolCalls=%d want %d", got, tt.want)
			}
		})
	}
}

// Dropping the only signed call must be refused: the request that reaches the
// upstream would no longer be the one the signature was issued for.
func TestSignedGeminiRequestToolCallsPreserved_CountMismatch(t *testing.T) {
	const one = `{"messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"n","arguments":"{}","thought_signature":"s"}}]}]}`
	const none = `{"messages":[{"role":"assistant","tool_calls":[]}]}`
	if signedGeminiRequestToolCallsPreserved([]byte(one), []byte(none)) {
		t.Error("dropping the only signed call was accepted")
	}
}
