// packages/ai-gateway/internal/policy/aiguard/backend_external.go
package aiguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ExternalBackend calls an OpenAI-compatible chat-completion endpoint over
// plain HTTP. The caller supplies the fully-rendered prompt as `prompt`;
// the backend wraps it in a single `user` message and sets
// response_format=json_object so compatible providers return strict JSON.
// Anthropic and other non-OpenAI providers handled via the fence-parser
// in DecodeJudgeOutput.
//
// SECURITY: this backend deliberately has NO field that can
// hold a stored provider Credential. The external_url judge is the
// operator's OWN classifier service; it authenticates ONLY via
// CustomHeaders (e.g. the operator sets `Authorization: Bearer <their
// judge token>` or `X-Api-Key: ...`). There is intentionally no
// `APIKey`/credential reference — a Credential in this product is always
// a real upstream provider key, and forwarding one to an operator-chosen
// URL was a key-exfiltration path. Use backend_provider.go
// (configured_provider mode) when the judge IS a real Nexus provider, so
// the credential only ever reaches its own provider.
//
// COST SEMANTICS: External-URL backend deliberately does NOT populate
// Response.Metadata.PromptTokens / CompletionTokens / CostUsd. The
// gateway has no idea what the operator's external classifier service
// charges — could be free (their own infra), could be a flat per-call
// fee, could be token-priced under a model the gateway doesn't know. So
// we stay silent: Metadata cost fields stay zero, the sink stamps zero
// on rec.AIGuardCostUsd, and the audit Writer's
// `if rec.AIGuardCostUsd != 0` guard skips emission so the row carries
// NULL. Configured-provider mode (backend_provider.go) is the only path
// that stamps cost, because there we own the LLM call + know the
// model's per-million pricing.
type ExternalBackend struct {
	URL           string            // base URL, e.g. https://judge.example.com/v1
	Model         string            // model identifier passed to the upstream
	CustomHeaders map[string]string // operator-supplied headers; the ONLY auth channel (e.g. Authorization / X-Api-Key)
	HTTPClient    *http.Client      // must be set; caller controls timeout
}

// Call sends prompt to the upstream and returns the parsed Response.
// Errors are returned as-is; callers map them into 503 backend_unavailable.
func (b *ExternalBackend) Call(ctx context.Context, prompt string) (*Response, error) {
	if b.HTTPClient == nil {
		return nil, fmt.Errorf("aiguard external: HTTPClient is nil")
	}
	body := map[string]any{
		"model":           b.Model,
		"messages":        []map[string]any{{"role": "user", "content": prompt}},
		"response_format": map[string]any{"type": "json_object"},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aiguard external: marshal: %w", err)
	}
	endpoint := b.URL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("aiguard external: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Auth is operator-supplied via CustomHeaders only (see struct doc).
	// No provider credential is ever attached here.
	for k, v := range b.CustomHeaders {
		req.Header.Set(k, v)
	}
	httpResp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aiguard external: http: %w", err)
	}
	defer httpResp.Body.Close() //nolint:errcheck
	if httpResp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		return nil, fmt.Errorf("aiguard external: status=%d body=%s", httpResp.StatusCode, string(raw))
	}
	// Cap the success-path read: the external classifier is operator-supplied
	// and could (mis)behave by streaming an unbounded body, which io.ReadAll
	// would buffer entirely into memory. A judge verdict is a small JSON object,
	// so 1 MiB is a generous ceiling.
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("aiguard external: read body: %w", err)
	}
	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return nil, fmt.Errorf("aiguard external: parse chat response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("aiguard external: empty choices")
	}
	return DecodeJudgeOutput(chatResp.Choices[0].Message.Content)
}
