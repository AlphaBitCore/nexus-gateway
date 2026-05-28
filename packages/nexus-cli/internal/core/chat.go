package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// sseLineBufferMax bounds a single SSE line so a runaway frame can't grow the
// scanner buffer without limit. Chat deltas are tiny; 1 MiB is generous.
const sseLineBufferMax = 1 << 20

// chatStreamChunk is one OpenAI chat.completion.chunk frame. Only the fields the
// Playground consumes are modeled; the gateway returns a superset.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *ChatUsage `json:"usage"`
}

// ChatStream streams a chat completion from the AI Gateway's
// /v1/chat/completions SSE endpoint, authenticated with vkSecret — a Virtual
// Key, NOT the admin token (the Playground accrues cost/usage to that VK like
// any other client). It calls onDelta for each non-empty content delta and
// returns the final usage block once the stream ends (data: [DONE]).
//
// onDelta runs on the streaming goroutine; callers driving a TUI typically push
// each delta onto a channel. A nil onDelta is allowed (usage-only callers).
func (c *Client) ChatStream(ctx context.Context, vkSecret string, req ChatRequest, onDelta func(string)) (*ChatUsage, error) {
	if strings.TrimSpace(vkSecret) == "" {
		return nil, &APIError{kind: ErrUnauthorized, Message: "no virtual key selected for chat"}
	}
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}

	raw, err := json.Marshal(req)
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: "marshal chat request: " + err.Error()}
	}
	url := strings.TrimRight(c.env.AIGatewayBaseURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: "build chat request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(vkSecret))

	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		return nil, parseAPIError(resp.StatusCode, body)
	}
	return scanChatSSE(resp.Body, onDelta)
}

// scanChatSSE parses an OpenAI chat SSE body: it forwards each content delta to
// onDelta and returns the usage frame. It stops at "data: [DONE]" or EOF. Split
// out so the parser is unit-testable against a string body without a server.
func scanChatSSE(body io.Reader, onDelta func(string)) (*ChatUsage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), sseLineBufferMax)
	var usage *ChatUsage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// A malformed frame is skipped rather than aborting the turn — the
			// gateway occasionally emits non-chunk control frames.
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" && onDelta != nil {
				onDelta(ch.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, &APIError{kind: ErrTransport, Message: "read chat stream: " + err.Error()}
	}
	return usage, nil
}
