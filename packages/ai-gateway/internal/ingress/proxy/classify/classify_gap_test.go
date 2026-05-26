// Package classify — classify_gap_test.go covers branches not reached by
// coverage_test.go.
//
// Named failure modes:
//   - ServeClassify (Echo): malformed JSON, missing field, backend_unavailable,
//     internal error, success
//   - ServeClassifyHTTP: backend_unavailable 503
//   - ServeComplianceWebhookHTTP: malformed JSON, backend_unavailable
package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
)

// backendUnavailableClassifier always returns BackendUnavailable.
type backendUnavailableClassifier struct{ detail string }

func (b *backendUnavailableClassifier) Classify(_ context.Context, _ aiguard.Request) (*aiguard.Response, error) {
	return nil, &aiguard.BackendUnavailable{Detail: b.detail}
}

// okClassifier always returns a successful classify response.
type okClassifier struct{}

func (o *okClassifier) Classify(_ context.Context, _ aiguard.Request) (*aiguard.Response, error) {
	return &aiguard.Response{Decision: "APPROVE", Labels: []string{}}, nil
}

// ServeClassify (Echo)

func echoRequest(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	return e.NewContext(req, w), w
}

func TestServeClassify_malformedJSON(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	c, w := echoRequest(http.MethodPost, "/v1/ai-guard/classify", `{bad json`)
	_ = h.ServeClassify(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "malformed_json" {
		t.Errorf("error: got %v, want malformed_json", body["error"])
	}
}

func TestServeClassify_missingField(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	c, w := echoRequest(http.MethodPost, "/v1/ai-guard/classify", `{}`)
	_ = h.ServeClassify(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "missing_required_field" {
		t.Errorf("error: got %v, want missing_required_field", body["error"])
	}
}

func TestServeClassify_backendUnavailable(t *testing.T) {
	h := NewClassifyHandler(&backendUnavailableClassifier{detail: "judge down"})
	c, w := echoRequest(http.MethodPost, "/v1/ai-guard/classify",
		`{"detector_type":"pii","content":"test content"}`)
	_ = h.ServeClassify(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "backend_unavailable" {
		t.Errorf("error: got %v, want backend_unavailable", body["error"])
	}
}

func TestServeClassify_internalError(t *testing.T) {
	h := NewClassifyHandler(&errClassifier{err: &someError{}})
	c, w := echoRequest(http.MethodPost, "/v1/ai-guard/classify",
		`{"detector_type":"pii","content":"test content"}`)
	_ = h.ServeClassify(c)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestServeClassify_success(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	c, w := echoRequest(http.MethodPost, "/v1/ai-guard/classify",
		`{"detector_type":"content_safety","content":"hello world"}`)
	_ = h.ServeClassify(c)
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// someError is a non-BackendUnavailable error.
type someError struct{}

func (s *someError) Error() string { return "some internal failure" }

// ServeClassifyHTTP: backend_unavailable 503

func TestServeClassifyHTTP_backendUnavailable(t *testing.T) {
	h := NewClassifyHandler(&backendUnavailableClassifier{detail: "judge unreachable"})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify",
		strings.NewReader(`{"detector_type":"pii","content":"secret data"}`))
	w := httptest.NewRecorder()
	h.ServeClassifyHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "backend_unavailable" {
		t.Errorf("error: got %v, want backend_unavailable", body["error"])
	}
	if body["detail"] != "judge unreachable" {
		t.Errorf("detail: got %v, want judge unreachable", body["detail"])
	}
}

func TestServeClassifyHTTP_success(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/classify",
		strings.NewReader(`{"detector_type":"content_safety","content":"safe text"}`))
	w := httptest.NewRecorder()
	h.ServeClassifyHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

// ServeComplianceWebhookHTTP: additional branches

func TestServeComplianceWebhookHTTP_malformedJSON(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/compliance-webhook",
		strings.NewReader(`{not json`))
	w := httptest.NewRecorder()
	h.ServeComplianceWebhookHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "malformed_json" {
		t.Errorf("error: got %v, want malformed_json", body["error"])
	}
}

func TestServeComplianceWebhookHTTP_backendUnavailable(t *testing.T) {
	h := NewClassifyHandler(&backendUnavailableClassifier{detail: "judge timeout"})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/compliance-webhook",
		strings.NewReader(`{"stage":"request","normalizedContent":["some content"]}`))
	w := httptest.NewRecorder()
	h.ServeComplianceWebhookHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
}

func TestServeComplianceWebhookHTTP_success(t *testing.T) {
	h := NewClassifyHandler(&okClassifier{})
	r := httptest.NewRequest(http.MethodPost, "/v1/ai-guard/compliance-webhook",
		strings.NewReader(`{"stage":"request","model":"gpt-4","normalizedContent":["hello"]}`))
	w := httptest.NewRecorder()
	h.ServeComplianceWebhookHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}
