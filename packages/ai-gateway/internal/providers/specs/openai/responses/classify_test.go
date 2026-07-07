package responses

import "testing"

func TestClassifyNonStreamBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want WireClass
	}{
		{"responses", `{"object":"response","id":"resp_1"}`, ClassResponses},
		{"chat", `{"object":"chat.completion","id":"x"}`, ClassChat},
		{"unknown_object", `{"object":"embedding"}`, ClassUnknown},
		{"no_object", `{"foo":1}`, ClassUnknown},
		{"empty", ``, ClassUnknown},
	}
	for _, c := range cases {
		if got := ClassifyNonStreamBody([]byte(c.body)); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestClassifyFirstSSEFrame(t *testing.T) {
	cases := []struct {
		name, ev, data string
		want           WireClass
	}{
		{"event_line", "response.created", `{"type":"response.created"}`, ClassResponses},
		{"type_in_data", "", `{"type":"response.output_text.delta","delta":"hi"}`, ClassResponses},
		{"builtin_tool_event", "response.web_search_call.in_progress", `{}`, ClassResponses},
		{"chat_chunk", "", `{"object":"chat.completion.chunk","choices":[]}`, ClassChat},
		{"keepalive", "", `: keep-alive`, ClassUnknown},
		{"empty", "", ``, ClassUnknown},
	}
	for _, c := range cases {
		if got := ClassifyFirstSSEFrame(c.ev, []byte(c.data)); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
