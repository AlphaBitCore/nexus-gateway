package httperr_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// TestErrJSON verifies the returned map carries the canonical nested error envelope shape.
func TestErrJSON(t *testing.T) {
	got := httperr.ErrJSON("something broke", "internal_error", "INTERNAL_ERROR")

	// Top-level key must be "error".
	errVal, ok := got["error"]
	if !ok {
		t.Fatal("ErrJSON: missing top-level 'error' key")
	}

	inner, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("ErrJSON: 'error' value must be map[string]any, got %T", errVal)
	}

	cases := []struct {
		field string
		want  string
	}{
		{"message", "something broke"},
		{"type", "internal_error"},
		{"code", "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		v, ok := inner[tc.field]
		if !ok {
			t.Errorf("ErrJSON: inner map missing field %q", tc.field)
			continue
		}
		s, ok := v.(string)
		if !ok {
			t.Errorf("ErrJSON: field %q must be string, got %T", tc.field, v)
			continue
		}
		if s != tc.want {
			t.Errorf("ErrJSON: field %q = %q; want %q", tc.field, s, tc.want)
		}
	}

	// No extra keys at the top level.
	if len(got) != 1 {
		t.Errorf("ErrJSON: top-level map has %d keys, want 1", len(got))
	}
	// No extra keys inside the inner envelope.
	if len(inner) != 3 {
		t.Errorf("ErrJSON: inner map has %d keys, want 3 (message, type, code)", len(inner))
	}
}

// TestErrJSON_EmptyStrings verifies ErrJSON does not panic or omit keys for empty inputs.
func TestErrJSON_EmptyStrings(t *testing.T) {
	got := httperr.ErrJSON("", "", "")
	inner, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatal("ErrJSON: 'error' must be map[string]any even for empty inputs")
	}
	for _, f := range []string{"message", "type", "code"} {
		if _, ok := inner[f]; !ok {
			t.Errorf("ErrJSON: field %q missing for empty-string inputs", f)
		}
	}
}

// TestWriteError verifies that WriteError emits the correct HTTP status, Content-Type,
// and a JSON body that conforms to the canonical envelope.
func TestWriteError(t *testing.T) {
	rr := httptest.NewRecorder()
	httperr.WriteError(rr, http.StatusBadRequest, "missing field", "validation_error", "MISSING_FIELD")

	// Status code.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("WriteError: status = %d; want %d", rr.Code, http.StatusBadRequest)
	}

	// Content-Type header.
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("WriteError: Content-Type = %q; want \"application/json\"", ct)
	}

	// Body must be valid JSON and match the canonical envelope.
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("WriteError: body is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}

	errVal, ok := body["error"]
	if !ok {
		t.Fatal("WriteError: JSON body missing top-level 'error' key")
	}
	inner, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("WriteError: 'error' value must be object, got %T", errVal)
	}

	wantFields := map[string]string{
		"message": "missing field",
		"type":    "validation_error",
		"code":    "MISSING_FIELD",
	}
	for f, want := range wantFields {
		got, ok := inner[f].(string)
		if !ok {
			t.Errorf("WriteError: field %q missing or non-string in response body", f)
			continue
		}
		if got != want {
			t.Errorf("WriteError: field %q = %q; want %q", f, got, want)
		}
	}
}

// TestWriteError_StatusCodes verifies WriteError propagates arbitrary status codes correctly.
func TestWriteError_StatusCodes(t *testing.T) {
	cases := []struct {
		status  int
		message string
		errType string
		code    string
	}{
		{http.StatusUnauthorized, "invalid token", "auth_error", "INVALID_TOKEN"},
		{http.StatusForbidden, "forbidden", "auth_error", "FORBIDDEN"},
		{http.StatusNotFound, "not found", "not_found", "NOT_FOUND"},
		{http.StatusServiceUnavailable, "service down", "service_unavailable", "SERVICE_UNAVAILABLE"},
		{http.StatusInternalServerError, "internal error", "internal_error", "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			rr := httptest.NewRecorder()
			httperr.WriteError(rr, tc.status, tc.message, tc.errType, tc.code)

			if rr.Code != tc.status {
				t.Errorf("status = %d; want %d", rr.Code, tc.status)
			}

			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not valid JSON: %v", err)
			}
			inner, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatal("body missing 'error' object")
			}
			if inner["message"] != tc.message {
				t.Errorf("message = %q; want %q", inner["message"], tc.message)
			}
			if inner["type"] != tc.errType {
				t.Errorf("type = %q; want %q", inner["type"], tc.errType)
			}
			if inner["code"] != tc.code {
				t.Errorf("code = %q; want %q", inner["code"], tc.code)
			}
		})
	}
}
