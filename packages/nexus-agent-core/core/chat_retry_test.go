package core

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetryableStreamDrop(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"h2 connection lost", &APIError{kind: ErrTransport, Message: "read chat stream: http2: client connection lost"}, true},
		{"connection reset", &APIError{kind: ErrTransport, Message: "read chat stream: read tcp: connection reset by peer"}, true},
		{"unexpected EOF", &APIError{kind: ErrTransport, Message: "unexpected EOF"}, true},
		{"closed network conn", &APIError{kind: ErrTransport, Message: "use of closed network connection"}, true},
		{"user canceled", &APIError{kind: ErrTransport, Message: "Post ...: context canceled"}, false},
		{"deadline", &APIError{kind: ErrTransport, Message: "context deadline exceeded"}, false},
		{"not transport (401)", &APIError{kind: ErrUnauthorized, Message: "connection lost"}, false},
		{"transport but unknown", &APIError{kind: ErrTransport, Message: "decode chat request"}, false},
		{"settled 5xx envelope with drop word", &APIError{kind: ErrTransport, Status: 502, Message: "upstream connection reset"}, false},
		{"plain error", errors.New("connection lost"), false},
	}
	for _, tc := range cases {
		if got := retryableStreamDrop(tc.err); got != tc.want {
			t.Errorf("%s: retryableStreamDrop = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// rstAfter hijacks the response, writes raw, flushes, then aborts the TCP connection
// with an RST (SetLinger 0) so the client's next read fails with "connection reset" —
// a faithful mid-stream connection drop, unlike a graceful Close which is a clean EOF.
func rstAfter(w http.ResponseWriter, raw string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = bufrw.WriteString(raw)
	_ = bufrw.Flush()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

const sseHeaders = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"

func TestChatToolStream_RecoversWhenConnDropsBeforeOutput(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			rstAfter(w, sseHeaders) // headers only, then RST — drop before any data
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var got strings.Builder
	res, err := c.ChatToolStream(context.Background(), "vk", ChatRequest{Model: "m"},
		func(s string) { got.WriteString(s) }, nil)
	if err != nil {
		t.Fatalf("expected transparent recovery, got error: %v", err)
	}
	if res.Content != "hello" || got.String() != "hello" {
		t.Fatalf("recovered content = %q / delta = %q, want hello", res.Content, got.String())
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("server calls = %d, want 2 (drop + one retry)", n)
	}
}

func TestChatStream_RecoversWhenConnDropsBeforeOutput(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			rstAfter(w, sseHeaders) // headers only, then RST — drop before any data
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var got strings.Builder
	usage, err := c.ChatStream(context.Background(), "vk", ChatRequest{Model: "m"},
		func(s string) { got.WriteString(s) })
	if err != nil {
		t.Fatalf("expected transparent recovery, got error: %v", err)
	}
	if got.String() != "hi" {
		t.Fatalf("recovered delta = %q, want hi", got.String())
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("recovered usage = %+v, want total_tokens=2", usage)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("server calls = %d, want 2 (drop + one retry)", n)
	}
}

func TestChatStream_NoRetryOnceContentStreamed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		rstAfter(w, sseHeaders+"data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var got strings.Builder
	_, err := c.ChatStream(context.Background(), "vk", ChatRequest{Model: "m"},
		func(s string) { got.WriteString(s) })
	if err == nil {
		t.Fatal("expected the drop-after-content to surface an error (no silent retry)")
	}
	if got.String() != "par" {
		t.Fatalf("partial delta = %q, want par", got.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("server calls = %d, want 1 (must NOT retry after content streamed)", n)
	}
}

// TestChatToolStream_ForwardsReasoningNoRetryOnSuccess exercises the reasoning
// callback path and confirms a clean stream is not retried.
func TestChatToolStream_ForwardsReasoningNoRetryOnSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var content, reasoning strings.Builder
	_, err := c.ChatToolStream(context.Background(), "vk", ChatRequest{Model: "m"},
		func(s string) { content.WriteString(s) },
		func(s string) { reasoning.WriteString(s) })
	if err != nil {
		t.Fatalf("clean stream errored: %v", err)
	}
	if content.String() != "done" || reasoning.String() != "think" {
		t.Fatalf("content=%q reasoning=%q, want done/think", content.String(), reasoning.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("server calls = %d, want 1 (no retry on a clean stream)", n)
	}
}

// TestChatToolStream_RetriesToolOnlyDropBeforeOutput proves a tool-only response
// (no visible content/reasoning) that drops before [DONE] IS retried: the tool call
// never finalized, so completing it on a fresh connection is correct — and the first
// attempt's partial result is discarded, so there is nothing to double-execute.
func TestChatToolStream_RetriesToolOnlyDropBeforeOutput(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// A half-streamed tool call (incomplete arguments), then RST — never finalized.
			rstAfter(w, sseHeaders+
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	res, err := c.ChatToolStream(context.Background(), "vk", ChatRequest{Model: "m"}, nil, nil)
	if err != nil {
		t.Fatalf("expected transparent recovery of the tool-only drop, got: %v", err)
	}
	if res == nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Arguments != "{\"a\":1}" {
		t.Fatalf("recovered tool call wrong: %+v", res)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("server calls = %d, want 2 (drop + one retry to finalize the tool call)", n)
	}
}

func TestChatToolStream_NoRetryOnceContentStreamed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Deliver one content delta, THEN drop — a retry here would duplicate the answer.
		rstAfter(w, sseHeaders+"data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var got strings.Builder
	_, err := c.ChatToolStream(context.Background(), "vk", ChatRequest{Model: "m"},
		func(s string) { got.WriteString(s) }, nil)
	if err == nil {
		t.Fatal("expected the drop-after-content to surface an error (no silent retry)")
	}
	if got.String() != "par" {
		t.Fatalf("partial delta = %q, want par", got.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("server calls = %d, want 1 (must NOT retry after content streamed)", n)
	}
}
