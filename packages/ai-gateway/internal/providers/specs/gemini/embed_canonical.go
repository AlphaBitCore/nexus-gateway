// Package gemini — embedding canonicalization helpers.
//
// Cross-format embedding routing uses OpenAI /v1/embeddings as the
// canonical hub shape. These helpers translate Gemini :embedContent (single)
// and :batchEmbedContents requests into the canonical shape and canonical
// responses back into the corresponding Gemini wire shape.
//
// Gemini-specific extensions (taskType, title) ride under
// nexus.ext.gemini.<key> via [canonicalext].
//
// The canonical "batch" flag is recorded as nexus.ext.gemini.batch = true|false
// so the reverse projection (canonical → Gemini wire) can emit the matching
// single-vs-batch envelope without inspecting the original URL.
package gemini

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// extractGeminiText concatenates the .parts[*].text fragments of a Gemini
// Content object into a single string. Missing or non-string parts are
// skipped (Gemini docs allow inlineData parts that have no text — those
// are silently dropped on the canonical side, the same way chat-side
// non-text parts are dropped).
func extractGeminiText(content gjson.Result) string {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return ""
	}
	var b strings.Builder
	parts.ForEach(func(_, p gjson.Result) bool {
		if t := p.Get("text"); t.Type == gjson.String {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Str)
		}
		return true
	})
	return b.String()
}

// stampGeminiExtensions writes the per-request Gemini extension fields
// (taskType, title, outputDimensionality) into the canonical body via
// canonicalext + the top-level "dimensions" field where the canonical
// already has a slot.
func stampGeminiExtensions(canonical []byte, req gjson.Result) ([]byte, error) {
	if v := req.Get("taskType"); v.Exists() && v.Str != "" {
		c, err := canonicalext.Set(canonical, "gemini", "taskType", v.Str)
		if err != nil {
			return nil, fmt.Errorf("gemini embed: stamp taskType: %w", err)
		}
		canonical = c
	}
	if v := req.Get("title"); v.Exists() && v.Str != "" {
		c, err := canonicalext.Set(canonical, "gemini", "title", v.Str)
		if err != nil {
			return nil, fmt.Errorf("gemini embed: stamp title: %w", err)
		}
		canonical = c
	}
	if v := req.Get("outputDimensionality"); v.Exists() && v.Type == gjson.Number {
		canonical, _ = sjson.SetBytes(canonical, "dimensions", v.Int())
	}
	return canonical, nil
}

// EmbedContentRequestToCanonical translates a Gemini :embedContent request
// body into OpenAI canonical embedding shape. The "batch" extension flag
// is set to false.
func EmbedContentRequestToCanonical(body []byte, providerModelID string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: invalid JSON request body",
		}
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: missing 'content' object",
		}
	}
	text := extractGeminiText(content)
	if text == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: content.parts contains no text",
		}
	}
	if providerModelID == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini embed: missing model id",
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", providerModelID)
	canonical, _ = sjson.SetBytes(canonical, "input", text)

	// Whole-body Gemini fields (taskType / title / outputDimensionality)
	// live at the request root for :embedContent.
	canonical, err := stampGeminiExtensions(canonical, gjson.ParseBytes(body))
	if err != nil {
		return nil, err
	}
	canonical, err = canonicalext.Set(canonical, "gemini", "batch", false)
	if err != nil {
		return nil, fmt.Errorf("gemini embed: stamp batch=false: %w", err)
	}
	return canonical, nil
}

// BatchEmbedContentsRequestToCanonical translates a Gemini
// :batchEmbedContents request body into OpenAI canonical embedding shape
// (input: []string, batch=true, lifted taskType/title when uniform across
// all sub-requests).
//
// Uniformity constraint (Gemini server-side limit): the Gemini
// batchEmbedContents API requires every sub-request in a batch to share
// the same taskType and outputDimensionality. If sub-requests disagree on
// either field, the Gemini server returns an error. The canonical layer
// enforces this constraint early (HTTP 400) and produces an actionable
// error message listing the differing values so callers can split the
// request rather than guessing which field caused the upstream 400.
func BatchEmbedContentsRequestToCanonical(body []byte, providerModelID string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini batch embed: invalid JSON request body",
		}
	}
	reqs := gjson.GetBytes(body, "requests")
	if !reqs.IsArray() {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini batch embed: missing or non-array 'requests'",
		}
	}
	if providerModelID == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "gemini batch embed: missing model id",
		}
	}

	inputs := make([]string, 0, 4)
	var sharedTaskType, sharedTitle string
	var sharedDim int64
	var taskTypeSet, titleSet, dimSet bool
	var taskTypeMixed, titleMixed, dimMixed bool
	var dimSeenZero bool
	// seenTaskTypes collects the distinct taskType values observed across
	// sub-requests; used to build an actionable error message listing the
	// conflicting values when taskTypeMixed is true.
	seenTaskTypes := make([]string, 0, 2)

	var parseErr error
	reqs.ForEach(func(_, r gjson.Result) bool {
		t := extractGeminiText(r.Get("content"))
		if t == "" {
			parseErr = &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "gemini batch embed: a request has no text parts",
			}
			return false
		}
		inputs = append(inputs, t)
		if tt := r.Get("taskType"); tt.Exists() && tt.Str != "" {
			if !taskTypeSet {
				sharedTaskType = tt.Str
				taskTypeSet = true
				seenTaskTypes = append(seenTaskTypes, tt.Str)
			} else if sharedTaskType != tt.Str {
				taskTypeMixed = true
				// Record the new differing value (deduplicate to keep message terse).
				found := false
				for _, v := range seenTaskTypes {
					if v == tt.Str {
						found = true
						break
					}
				}
				if !found {
					seenTaskTypes = append(seenTaskTypes, tt.Str)
				}
			}
		}
		if ti := r.Get("title"); ti.Exists() && ti.Str != "" {
			if !titleSet {
				sharedTitle = ti.Str
				titleSet = true
			} else if sharedTitle != ti.Str {
				titleMixed = true
			}
		}
		if od := r.Get("outputDimensionality"); od.Exists() && od.Type == gjson.Number {
			if !dimSet {
				sharedDim = od.Int()
				dimSet = true
			} else if sharedDim != od.Int() {
				dimMixed = true
			}
		} else if dimSet {
			// One had a dim, another doesn't.
			dimSeenZero = true
		}
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if taskTypeMixed {
		return nil, &provcore.ProviderError{
			Status: http.StatusBadRequest,
			Code:   provcore.CodeInvalidRequest,
			Message: fmt.Sprintf(
				"Gemini :batchEmbedContents requires all sub-requests to share the same taskType and outputDimensionality. Got: mixed taskType=%v in batch of %d items. Split into separate requests.",
				seenTaskTypes, len(inputs),
			),
		}
	}
	if dimMixed || dimSeenZero {
		return nil, &provcore.ProviderError{
			Status: http.StatusBadRequest,
			Code:   provcore.CodeInvalidRequest,
			Message: fmt.Sprintf(
				"Gemini :batchEmbedContents requires all sub-requests to share the same taskType and outputDimensionality. Got: mixed outputDimensionality in batch of %d items. Split into separate requests.",
				len(inputs),
			),
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", providerModelID)
	canonical, _ = sjson.SetBytes(canonical, "input", inputs)
	if dimSet {
		canonical, _ = sjson.SetBytes(canonical, "dimensions", sharedDim)
	}
	if taskTypeSet {
		c, err := canonicalext.Set(canonical, "gemini", "taskType", sharedTaskType)
		if err != nil {
			return nil, fmt.Errorf("gemini batch embed: stamp taskType: %w", err)
		}
		canonical = c
	}
	if titleSet && !titleMixed {
		c, err := canonicalext.Set(canonical, "gemini", "title", sharedTitle)
		if err != nil {
			return nil, fmt.Errorf("gemini batch embed: stamp title: %w", err)
		}
		canonical = c
	}
	c, err := canonicalext.Set(canonical, "gemini", "batch", true)
	if err != nil {
		return nil, fmt.Errorf("gemini batch embed: stamp batch=true: %w", err)
	}
	return c, nil
}

// CanonicalToEmbedContentResponse converts a canonical OpenAI-shape
// embedding response with exactly one data[] entry into the Gemini
// :embedContent response shape: {"embedding": {"values": [...]}}.
//
// Multi-item canonical responses error rather than truncating — they
// belong on the :batchEmbedContents path.
func CanonicalToEmbedContentResponse(canonical []byte) ([]byte, error) {
	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("gemini embed response: canonical body missing data[]")
	}
	rows := data.Array()
	if len(rows) != 1 {
		return nil, fmt.Errorf("gemini embed response: expected 1 data[] entry, got %d", len(rows))
	}
	values := rows[0].Get("embedding")
	if !values.IsArray() {
		return nil, fmt.Errorf("gemini embed response: data[0].embedding missing or not array")
	}
	vec := make([]float64, 0)
	values.ForEach(func(_, n gjson.Result) bool {
		vec = append(vec, n.Float())
		return true
	})
	return json.Marshal(map[string]any{
		"embedding": map[string]any{"values": vec},
	})
}

// CanonicalToBatchEmbedContentsResponse converts a canonical OpenAI-shape
// embedding response into the Gemini :batchEmbedContents response shape:
// {"embeddings": [{"values": [...]}, ...]}. Order is preserved from the
// canonical data[] array.
func CanonicalToBatchEmbedContentsResponse(canonical []byte) ([]byte, error) {
	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("gemini batch embed response: canonical body missing data[]")
	}
	out := make([]map[string]any, 0)
	var parseErr error
	data.ForEach(func(_, item gjson.Result) bool {
		vec := item.Get("embedding")
		if !vec.IsArray() {
			parseErr = fmt.Errorf("gemini batch embed response: data[].embedding missing or not array")
			return false
		}
		row := make([]float64, 0)
		vec.ForEach(func(_, n gjson.Result) bool {
			row = append(row, n.Float())
			return true
		})
		out = append(out, map[string]any{"values": row})
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return json.Marshal(map[string]any{"embeddings": out})
}

// CanonicalEmbeddingBatchFlag reads nexus.ext.gemini.batch from a canonical
// body. Returns (true, true) when the flag is present and true; (false, true)
// when present and false; (false, false) when the flag is absent.
//
// Used by ResponseCanonicalToIngressEmbeddings to pick the correct Gemini
// response shape when the ingress arrived via a Gemini-shaped URL.
func CanonicalEmbeddingBatchFlag(canonical []byte) (batch bool, present bool) {
	v := canonicalext.Get(canonical, "gemini", "batch")
	if !v.Exists() {
		return false, false
	}
	return v.Bool(), true
}
