// packages/ai-gateway/internal/handler/classify_handler.go
package classify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
)

// Classifier is the minimal interface the classify handler depends on.
// The live wiring in main.go adapts aiguard.Classify + config cache +
// backend selection into this single method (see wiring_aiguard.go).
type Classifier interface {
	Classify(ctx context.Context, req aiguard.Request) (*aiguard.Response, error)
}

// ClassifyHandler owns the POST /v1/ai-guard/classify route.
type ClassifyHandler struct {
	classifier Classifier
}

// ComplianceWebhookRequest mirrors the payload emitted by webhook-forward.
// The endpoint keeps this shape so existing webhook rows can call AI Guard
// without an extra translation proxy.
type ComplianceWebhookRequest struct {
	Stage             string   `json:"stage"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	TargetHost        string   `json:"targetHost"`
	SourceIP          string   `json:"sourceIP"`
	BodySize          int      `json:"bodySize"`
	ContentType       string   `json:"contentType"`
	Model             string   `json:"model"`
	IngressType       string   `json:"ingressType"`
	NormalizedContent []string `json:"normalizedContent"`
}

// ComplianceWebhookResponse mirrors the response contract consumed by
// webhook-forward.
type ComplianceWebhookResponse struct {
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	// Redactions carries AI-Guard's structured replacement suggestions
	// through to the webhook caller verbatim. Callers that decode this
	// emit one normalize.TransformSpan per redaction so the audit pipeline
	// records the judge's intent even when the caller cannot apply the
	// rewrite inflight. Absent when AI-Guard had no span-level suggestions
	// (approve / soft-block paths).
	Redactions []aiguard.Redaction `json:"redactions,omitempty"`
}

type contentSource string

const (
	contentSourceNormalized contentSource = "normalized_content"
	contentSourceMessages   contentSource = "messages_content"
	contentSourceTextField  contentSource = "text_field"
	contentSourcePayloadRaw contentSource = "payload_json_fallback"
)

// NewClassifyHandler constructs the handler. classifier must be non-nil;
// mounting a nil classifier would panic on first request and is a wiring
// error, not a runtime condition, so we do not guard at construction time.
func NewClassifyHandler(classifier Classifier) *ClassifyHandler {
	return &ClassifyHandler{classifier: classifier}
}

// ServeClassify is the Echo-native entry point. It is used directly by
// Echo-based test harnesses and by the stdlib wrapper (ServeClassifyHTTP)
// for services mounting http.ServeMux.
//
// Error taxonomy — matches aiguard spec §4.5 / §4.6:
//   - malformed JSON body              -> 400 malformed_json
//   - missing detector_type / content  -> 400 missing_required_field
//   - aiguard.BackendUnavailable       -> 503 backend_unavailable
//   - anything else                    -> 500 internal
func (h *ClassifyHandler) ServeClassify(c echo.Context) error {
	var req aiguard.Request
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, aiguard.ErrorBody{
			Error:  "malformed_json",
			Detail: err.Error(),
		})
	}
	if req.DetectorType == "" || req.Content == "" {
		return c.JSON(http.StatusBadRequest, aiguard.ErrorBody{
			Error:  "missing_required_field",
			Detail: "detector_type and content are required",
		})
	}

	resp, err := h.classifier.Classify(c.Request().Context(), req)
	if err != nil {
		var backendErr *aiguard.BackendUnavailable
		if errors.As(err, &backendErr) {
			return c.JSON(http.StatusServiceUnavailable, aiguard.ErrorBody{
				Error:  "backend_unavailable",
				Detail: backendErr.Detail,
			})
		}
		return c.JSON(http.StatusInternalServerError, aiguard.ErrorBody{
			Error:  "internal",
			Detail: err.Error(),
		})
	}
	return c.JSON(http.StatusOK, resp)
}

// ServeClassifyHTTP is a stdlib net/http entry point for services that
// mount http.ServeMux rather than an Echo router (ai-gateway). It uses
// the same decoding + error mapping as ServeClassify without standing up
// an Echo instance per request.
func (h *ClassifyHandler) ServeClassifyHTTP(w http.ResponseWriter, r *http.Request) {
	var req aiguard.Request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeClassifyJSON(w, http.StatusBadRequest, aiguard.ErrorBody{
			Error:  "malformed_json",
			Detail: err.Error(),
		})
		return
	}
	if req.DetectorType == "" || req.Content == "" {
		writeClassifyJSON(w, http.StatusBadRequest, aiguard.ErrorBody{
			Error:  "missing_required_field",
			Detail: "detector_type and content are required",
		})
		return
	}

	resp, err := h.classifier.Classify(r.Context(), req)
	if err != nil {
		var backendErr *aiguard.BackendUnavailable
		if errors.As(err, &backendErr) {
			writeClassifyJSON(w, http.StatusServiceUnavailable, aiguard.ErrorBody{
				Error:  "backend_unavailable",
				Detail: backendErr.Detail,
			})
			return
		}
		writeClassifyJSON(w, http.StatusInternalServerError, aiguard.ErrorBody{
			Error:  "internal",
			Detail: err.Error(),
		})
		return
	}
	writeClassifyJSON(w, http.StatusOK, resp)
}

// ServeComplianceWebhookHTTP accepts webhook-forward payloads and returns a
// webhook-compatible decision response.
func (h *ClassifyHandler) ServeComplianceWebhookHTTP(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&payload); err != nil {
		writeClassifyJSON(w, http.StatusBadRequest, aiguard.ErrorBody{
			Error:  "malformed_json",
			Detail: err.Error(),
		})
		return
	}
	req := payloadToComplianceWebhookRequest(payload)
	content, source := webhookPayloadContent(payload, req)

	classifyReq := aiguard.Request{
		DetectorType: "compliance_webhook",
		Content:      content,
		Context: aiguard.Context{
			Ingress:     req.IngressType,
			TargetModel: req.Model,
			HookName:    "compliance-webhook",
		},
	}
	resp, err := h.classifier.Classify(r.Context(), classifyReq)
	if err != nil {
		var backendErr *aiguard.BackendUnavailable
		if errors.As(err, &backendErr) {
			writeClassifyJSON(w, http.StatusServiceUnavailable, aiguard.ErrorBody{
				Error:  "backend_unavailable",
				Detail: backendErr.Detail,
			})
			return
		}
		writeClassifyJSON(w, http.StatusInternalServerError, aiguard.ErrorBody{
			Error:  "internal",
			Detail: err.Error(),
		})
		return
	}

	writeClassifyJSON(w, http.StatusOK, ComplianceWebhookResponse{
		Decision:   toWebhookDecision(resp.Decision),
		Reason:     resp.Reason,
		ReasonCode: webhookReasonCode(resp, source),
		Redactions: resp.Redactions,
	})
}

func payloadToComplianceWebhookRequest(payload map[string]any) ComplianceWebhookRequest {
	return ComplianceWebhookRequest{
		Stage:             stringField(payload, "stage"),
		Method:            stringField(payload, "method"),
		Path:              stringField(payload, "path"),
		TargetHost:        stringField(payload, "targetHost"),
		SourceIP:          stringField(payload, "sourceIP"),
		BodySize:          intField(payload, "bodySize"),
		ContentType:       stringField(payload, "contentType"),
		Model:             stringField(payload, "model"),
		IngressType:       stringField(payload, "ingressType"),
		NormalizedContent: stringSliceField(payload, "normalizedContent"),
	}
}

func webhookPayloadContent(payload map[string]any, req ComplianceWebhookRequest) (string, contentSource) {
	if joined := joinNonEmpty(req.NormalizedContent, "\n"); joined != "" {
		return joined, contentSourceNormalized
	}

	if joined := extractMessagesText(payload); joined != "" {
		return joined, contentSourceMessages
	}

	if direct := firstTextField(payload, "content", "input", "text", "prompt"); direct != "" {
		return direct, contentSourceTextField
	}

	parts := []string{
		req.Stage,
		req.Method,
		req.Path,
		req.TargetHost,
		req.Model,
		req.ContentType,
	}
	if joined := joinNonEmpty(parts, " "); joined != "" {
		return joined, contentSourceTextField
	}

	if raw, err := json.Marshal(payload); err == nil {
		if text := strings.TrimSpace(string(raw)); text != "" && text != "{}" {
			return text, contentSourcePayloadRaw
		}
	}
	return "compliance-webhook request", contentSourcePayloadRaw
}

func toWebhookDecision(decision string) string {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "reject_hard":
		return "REJECT_HARD"
	case "reject_soft":
		return "BLOCK_SOFT"
	case "modify":
		return "MODIFY"
	case "abstain":
		return "ABSTAIN"
	default:
		return "APPROVE"
	}
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func webhookReasonCode(resp *aiguard.Response, source contentSource) string {
	if resp != nil {
		if code := firstNonEmpty(resp.Labels); code != "" {
			return code
		}
	}
	return string(source)
}

func stringField(payload map[string]any, key string) string {
	v := payload[key]
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func intField(payload map[string]any, key string) int {
	v, ok := payload[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func stringSliceField(payload map[string]any, key string) []string {
	raw, ok := payload[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

func joinNonEmpty(values []string, sep string) string {
	items := make([]string, 0, len(values))
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			items = append(items, s)
		}
	}
	return strings.Join(items, sep)
}

func firstTextField(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := stringField(payload, key); text != "" {
			return text
		}
	}
	return ""
}

func extractMessagesText(payload map[string]any) string {
	// OpenAI-style: { messages: [{role, content}] }
	if joined := extractMessagesFromValue(payload["messages"]); joined != "" {
		return joined
	}
	// Common wrapped test payloads: { input: { messages: [...] } }
	if inputObj, ok := payload["input"].(map[string]any); ok {
		if joined := extractMessagesFromValue(inputObj["messages"]); joined != "" {
			return joined
		}
	}
	return ""
}

func extractMessagesFromValue(raw any) string {
	rows, ok := raw.([]any)
	if !ok {
		return ""
	}
	chunks := make([]string, 0, len(rows))
	for _, row := range rows {
		msg, ok := row.(map[string]any)
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if s := strings.TrimSpace(content); s != "" {
				chunks = append(chunks, s)
			}
		case []any:
			for _, block := range content {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if s, ok := b["text"].(string); ok && strings.TrimSpace(s) != "" {
					chunks = append(chunks, strings.TrimSpace(s))
				}
			}
		}
	}
	return strings.Join(chunks, "\n")
}

func writeClassifyJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
