package envelope

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestWriteEndpointNotSupported_OpenAIShape(t *testing.T) {
	for _, path := range []string{"/v1/completions", "/v1/moderations", "/v1/images/edits", "/v1/images/variations"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()

			WriteEndpointNotSupported(rec, path)

			if rec.Code != 404 {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			// The whole point: Go's ServeMux default was text/plain, which the
			// OpenAI SDKs cannot parse, so err.message came out empty.
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			body := rec.Body.Bytes()
			if !gjson.ValidBytes(body) {
				t.Fatalf("body is not valid JSON: %s", body)
			}
			if got := gjson.GetBytes(body, "error.code").String(); got != "ENDPOINT_NOT_SUPPORTED" {
				t.Errorf("error.code = %q, want ENDPOINT_NOT_SUPPORTED", got)
			}
			if got := gjson.GetBytes(body, "error.type").String(); got != "not_found_error" {
				t.Errorf("error.type = %q, want not_found_error", got)
			}
			msg := gjson.GetBytes(body, "error.message").String()
			if msg == "" {
				t.Fatal("error.message is empty — the SDK would surface a 404 with no explanation")
			}
			if !strings.Contains(msg, path) {
				t.Errorf("message %q should name the path %q", msg, path)
			}
		})
	}
}

func TestWriteEndpointNotSupported_AnthropicShape(t *testing.T) {
	// An anthropic-SDK caller only understands {"type":"error","error":{…}};
	// handing it the OpenAI envelope would leave it unparseable in turn.
	rec := httptest.NewRecorder()

	WriteEndpointNotSupported(rec, "/v1/messages/batches")

	body := rec.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Errorf("top-level type = %q, want error", got)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "not_found_error" {
		t.Errorf("error.type = %q, want not_found_error", got)
	}
	if gjson.GetBytes(body, "error.message").String() == "" {
		t.Error("anthropic envelope must carry a message too")
	}
}
