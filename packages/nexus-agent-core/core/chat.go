package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// sseLineBufferMax bounds a single SSE line so a runaway frame can't grow the
// scanner buffer without limit. Chat deltas are tiny; 1 MiB is generous.
const sseLineBufferMax = 1 << 20

// chatStreamChunk is one OpenAI chat.completion.chunk frame. Tool-calls and
// finish_reason are modeled so the agent loop can drive native function calling;
// the gateway returns a superset.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning is the model's thinking channel, which the gateway preserves.
			// Different providers name it differently; the gateway normalizes toward
			// reasoning_content (DeepSeek-style), with reasoning as a fallback.
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
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
	var emitted bool
	od := func(s string) {
		if s != "" {
			emitted = true
		}
		if onDelta != nil {
			onDelta(s)
		}
	}
	usage, err := c.chatStreamOnce(ctx, vkSecret, req, od)
	// Transparent recovery: a connection dropped before any content reached the user —
	// retry once on a fresh connection (see retryableStreamDrop). Never after output.
	if err != nil && !emitted && retryableStreamDrop(err) {
		usage, err = c.chatStreamOnce(ctx, vkSecret, req, od)
	}
	return usage, err
}

func (c *Client) chatStreamOnce(ctx context.Context, vkSecret string, req ChatRequest, onDelta func(string)) (*ChatUsage, error) {
	body, err := c.openChatStream(ctx, vkSecret, req)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return scanChatSSE(body, onDelta)
}

// openChatStream is the shared VK-authed streaming prologue for ChatStream and
// ChatToolStream: it guards the VK, forces stream+usage, builds and sends the POST,
// and on a non-2xx maps the body to a classified *APIError. On success it returns the
// open response body for the caller's SSE scanner — the caller must Close it.
func (c *Client) openChatStream(ctx context.Context, vkSecret string, req ChatRequest) (io.ReadCloser, error) {
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

	resp, err := c.streamc.Do(httpReq)
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: err.Error()}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		resp.Body.Close()
		return nil, parseAPIError(resp.StatusCode, body)
	}
	return resp.Body, nil
}

// GatewayModels returns the model ids the given Virtual Key may actually use, by
// calling the AI Gateway's VK-scoped GET /v1/models (the same endpoint every upstream
// provider exposes). Callers use it to derive an offered model set from a VK's real
// allowed models (E90 FR-17) rather than a static list that could name an unreachable
// model. The OpenAI-shape response is parsed (`{data:[{id}]}`); Nexus extension fields
// are ignored.
func (c *Client) GatewayModels(ctx context.Context, vkSecret string) ([]string, error) {
	if strings.TrimSpace(vkSecret) == "" {
		return nil, &APIError{kind: ErrUnauthorized, Message: "no virtual key for models lookup"}
	}
	u := strings.TrimRight(c.env.AIGatewayBaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: "build models request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(vkSecret))
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, &APIError{kind: ErrTransport, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &APIError{kind: ErrTransport, Message: "decode models: " + err.Error()}
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if strings.TrimSpace(m.ID) != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// ChatToolStream streams a (possibly tool-calling) completion from the AI
// Gateway's /v1/chat/completions SSE endpoint under vkSecret (a Virtual Key, not
// the admin token). It forwards content deltas to onDelta and returns the
// assembled ChatResult (content, accumulated tool calls, finish reason, usage).
func (c *Client) ChatToolStream(ctx context.Context, vkSecret string, req ChatRequest, onDelta, onReasoning func(string)) (*ChatResult, error) {
	var emitted bool
	mark := func(s string) {
		if s != "" {
			emitted = true
		}
	}
	od := func(s string) {
		mark(s)
		if onDelta != nil {
			onDelta(s)
		}
	}
	or := func(s string) {
		mark(s)
		if onReasoning != nil {
			onReasoning(s)
		}
	}
	res, err := c.chatToolStreamOnce(ctx, vkSecret, req, od, or)
	// Transparent recovery: when a connection drops BEFORE any content/reasoning reached
	// the user (e.g. a half-dead pooled h2 connection the health-check had not yet
	// evicted), retry once on a fresh connection so the user never sees a transient drop.
	// An error means the stream never reached [DONE], so the result is not finalized and
	// the first attempt's res is discarded here — there is no completed tool call to
	// replay. NEVER retried once content/reasoning streamed: re-running would duplicate
	// the visible answer.
	if err != nil && !emitted && retryableStreamDrop(err) {
		res, err = c.chatToolStreamOnce(ctx, vkSecret, req, od, or)
	}
	return res, err
}

func (c *Client) chatToolStreamOnce(ctx context.Context, vkSecret string, req ChatRequest, onDelta, onReasoning func(string)) (*ChatResult, error) {
	body, err := c.openChatStream(ctx, vkSecret, req)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return scanChatToolSSE(body, onDelta, onReasoning)
}

// retryableStreamDrop reports whether err is a transient connection drop that a fresh
// connection can recover — the case a silent reuse of a dead keep-alive connection
// produces. It is deliberately narrow: only ErrTransport failures whose message names a
// connection-level drop, and NOT a user interrupt or a turn-deadline (those are not
// transient and must not silently re-run).
func retryableStreamDrop(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	// Only a genuine connection-level failure is safe to silently re-POST: kind
	// ErrTransport AND Status 0 (no HTTP response ever completed). A settled 4xx/5xx
	// envelope carries a non-zero Status even when mapped to ErrTransport — the gateway
	// already processed the request (and may have billed it), so it must NOT be retried,
	// even if its error text happens to contain a connection-drop word.
	if ae.kind != ErrTransport || ae.Status != 0 {
		return false
	}
	msg := ae.Message
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "deadline exceeded") {
		return false
	}
	for _, sig := range []string{
		"connection lost",                  // http2: client connection lost
		"connection reset",                 // peer/NAT RST
		"broken pipe",                      // write to a half-closed conn
		"unexpected EOF",                   // truncated stream
		"use of closed network connection", // pool handed back a closed conn
		"GOAWAY",                           // server/proxy graceful close mid-stream
		"server closed",                    // idle conn closed under us
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// scanChatToolSSE parses an OpenAI chat SSE body, forwarding content deltas to
// onDelta, reasoning/thinking deltas to onReasoning (either callback may be nil),
// and accumulating tool_calls by index across frames. It stops at "data: [DONE]"
// or EOF. Split out so the parser is unit-testable against a string body.
func scanChatToolSSE(body io.Reader, onDelta, onReasoning func(string)) (*ChatResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), sseLineBufferMax)
	res := &ChatResult{}
	var content strings.Builder
	byIndex := map[int]*ToolCall{} // accumulator keyed by the streamed index
	var order []int
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
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(ch.Delta.Content)
				}
			}
			if onReasoning != nil {
				if r := ch.Delta.ReasoningContent; r != "" {
					onReasoning(r)
				} else if r := ch.Delta.Reasoning; r != "" {
					onReasoning(r)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := byIndex[tc.Index]
				if !ok {
					acc = &ToolCall{Type: "function"}
					byIndex[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				if tc.Function.Name != "" {
					acc.Function.Name = tc.Function.Name
				}
				acc.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				res.FinishReason = *ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			res.Usage = chunk.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return res, &APIError{kind: ErrTransport, Message: "read chat stream: " + err.Error()}
	}
	res.Content = content.String()
	sort.Ints(order)
	for _, i := range order {
		res.ToolCalls = append(res.ToolCalls, *byIndex[i])
	}
	return res, nil
}

// scanChatSSE preserves the plain content+usage contract for ChatStream callers
// by delegating to the richer scanner and discarding tool calls. scanChatToolSSE
// always returns a non-nil result (even on a reader error), so res.Usage is safe.
func scanChatSSE(body io.Reader, onDelta func(string)) (*ChatUsage, error) {
	res, err := scanChatToolSSE(body, onDelta, nil)
	return res.Usage, err
}
