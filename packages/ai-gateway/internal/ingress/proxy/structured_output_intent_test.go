package proxy

import "testing"

// Which requests constrain WHICH model may serve them.
//
// `json_schema` does: the answer either matches the caller's schema or their
// parse fails, and one model in the pool (kimi-k2.5, probed 2026-08-19) returns
// prose with HTTP 200 rather than refusing — nothing downstream can turn that
// into an error.
//
// `json_object` does not: it asks for "some JSON", and every target either
// honours it natively or is given a system instruction that does (the Anthropic
// codec builds exactly that). Treating it as a constraint would narrow the
// candidate pool for a requirement no model actually fails.
func TestRequestRequiresStructuredOutput(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"json_schema is a constraint", `{"response_format":{"type":"json_schema","json_schema":{"name":"v"}}}`, true},
		{"json_object is not", `{"response_format":{"type":"json_object"}}`, false},
		{"text is not", `{"response_format":{"type":"text"}}`, false},
		{"no response_format at all", `{"model":"gpt-4o","messages":[]}`, false},
		{"empty body", ``, false},
		{"malformed body does not panic or claim a constraint", `{"response_format":`, false},
		{"a json_schema nested elsewhere is not the top-level field",
			`{"tools":[{"function":{"parameters":{"response_format":{"type":"json_schema"}}}}]}`, false},

		// The gateway takes four chat dialects and each spells this differently.
		// Reading only the OpenAI spelling left `/v1/responses` and `/v1/messages`
		// unprotected — `auto` there could still pick a model that answers prose,
		// which is the whole failure this flag exists to prevent. The images and
		// audio endpoints are the counter-case: they carry `response_format` as a
		// STRING ("url", "mp3"), so `.type` must keep missing them.
		{"the Responses API spells it text.format",
			`{"model":"auto","input":"hi","text":{"format":{"type":"json_schema","name":"v"}}}`, true},
		{"the Anthropic Messages ingress spells it output_config.format",
			`{"model":"auto","messages":[],"output_config":{"format":{"type":"json_schema","schema":{}}}}`, true},
		{"the Gemini ingress spells it generationConfig.responseSchema",
			`{"contents":[],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT"}}}`, true},
		{"a Gemini request asking for JSON with NO schema does not constrain the pool",
			`{"contents":[],"generationConfig":{"responseMimeType":"application/json"}}`, false},
		{"images response_format is a string, not a schema",
			`{"model":"auto","prompt":"a cat","response_format":"url"}`, false},
		{"audio response_format is a string, not a schema",
			`{"model":"auto","input":"hi","response_format":"mp3"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestRequiresStructuredOutput([]byte(tt.body)); got != tt.want {
				t.Errorf("requestRequiresStructuredOutput(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
