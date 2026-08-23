package errors

import (
	"net/http"
	"strings"
	"testing"
)

// The production body, verbatim from the 21 rows recorded on 2026-08-05
// (every row model gpt-audio-mini).
const prodAudioRejectionBody = `{"error":{"message":"Invalid value: 'input_audio'. Supported values are: 'input_text', 'input_image', 'output_text', 'refusal', 'input_file', 'computer_screenshot', 'summary_text', and 'encrypted_content'.","type":"invalid_request_error","param":"input[0].content[0].type","code":"invalid_value"}}`

func TestNormalize_ResponsesAudio_KeepsUpstreamTextAndAddsTheRemedy(t *testing.T) {
	pe := ErrorNormalizer{}.Normalize(http.StatusBadRequest, nil, []byte(prodAudioRejectionBody))
	if pe == nil {
		t.Fatal("no ProviderError produced")
	}
	// The upstream's own text is the accurate part — it names the rejected
	// value and lists what the line takes. Replacing it would be a downgrade.
	if !strings.Contains(pe.Message, "Invalid value: 'input_audio'.") {
		t.Errorf("upstream text was lost: %q", pe.Message)
	}
	if !strings.Contains(pe.Message, "'computer_screenshot'") {
		t.Errorf("the supported-values list a caller may be reading was lost: %q", pe.Message)
	}
	// The remedy is the one thing OpenAI cannot know: the same model serves
	// audio on the chat line.
	if !strings.Contains(pe.Message, "/v1/chat/completions") {
		t.Errorf("no remedy — a caller concludes the model cannot take audio at all: %q", pe.Message)
	}
	if pe.Code != "invalid_request" {
		t.Errorf("Code=%q want invalid_request", pe.Code)
	}
}

// Every other 400 must be left exactly as the upstream wrote it. A remedy
// that leaks onto unrelated errors is worse than none — it sends callers to
// a line that has nothing to do with their problem.
func TestNormalize_OtherBadRequests_AreNotAnnotated(t *testing.T) {
	for name, body := range map[string]string{
		"unsupported parameter": `{"error":{"message":"Unsupported parameter: 'top_p' is not supported with this model.","type":"invalid_request_error"}}`,
		"context overflow":      `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error"}}`,
		"another invalid value": `{"error":{"message":"Invalid value: 'input_video'. Supported values are: 'input_text'.","type":"invalid_request_error"}}`,
	} {
		pe := ErrorNormalizer{}.Normalize(http.StatusBadRequest, nil, []byte(body))
		if pe == nil {
			t.Fatalf("%s: no ProviderError", name)
		}
		if strings.Contains(pe.Message, "Nexus:") {
			t.Errorf("%s: remedy leaked onto an unrelated error: %q", name, pe.Message)
		}
	}
}

// Context-overflow classification must survive the annotation hook running
// on the same arm.
func TestNormalize_ContextOverflowStillClassified(t *testing.T) {
	body := `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error"}}`
	pe := ErrorNormalizer{}.Normalize(http.StatusBadRequest, nil, []byte(body))
	if pe.Code != "context_overflow" {
		t.Errorf("Code=%q want context_overflow", pe.Code)
	}
}
