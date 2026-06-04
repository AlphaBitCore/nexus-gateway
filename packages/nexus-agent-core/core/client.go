package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client.go is the HTTP engine for the typed capability surface: it builds and
// sends admin-authed requests, classifies non-2xx into *APIError, and exposes the
// shared adminGet/do/roundtrip primitives. The typed capability methods are split
// by direction into client_reads.go (GET queries) and client_writes.go (mutations
// + action POSTs); the wire structs live in types_*.go.

// Client is the typed capability surface over a Nexus deployment. Each face
// (CLI, TUI, MCP) calls these methods; none of them build HTTP requests.
type Client struct {
	env     Env
	ts      TokenSource
	httpc   *http.Client // admin (non-streaming) calls; bounded overall timeout
	streamc *http.Client // streaming (SSE) calls; no overall timeout — ctx-bound
}

// NewClient builds a Client for env using ts for admin credentials. A nil httpc
// gets a default with a widened TLS-handshake budget. The streaming client shares
// the admin transport (connection pooling, same proxy/TLS settings) but carries no
// overall timeout: an SSE turn is bounded by its request context (the agent's turn
// timeout), not a fixed wall clock.
func NewClient(env Env, ts TokenSource, httpc *http.Client) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 60 * time.Second, Transport: NewHTTPTransport()}
	}
	streamc := &http.Client{Transport: transportOf(httpc)}
	return &Client{env: env, ts: ts, httpc: httpc, streamc: streamc}
}

// NewHTTPTransport clones http.DefaultTransport's defaults but tunes two things for a
// low-traffic client (CLI/TUI) talking to prod over the public internet:
//
//   - TLSHandshakeTimeout widened to 30s. A slow upstream TLS termination (observed
//     against prod over some networks) exceeded the 10s default and surfaced as "TLS
//     handshake timeout" even though the route was healthy and curl succeeded.
//
//   - IdleConnTimeout lowered to 30s (from Go's 90s default). The CLI is low-traffic and
//     talks to prod through nginx (keepalive_timeout 65s) plus, on the user's network, a
//     NAT/firewall that silently drops idle TCP connections (no FIN). With the 90s
//     default the client held a pooled connection longer than the middlebox kept it
//     open, then reused a connection the far side had already dropped: the request was
//     written into a black hole, never reached the server (no server-side access-log
//     entry), and hung until the client/turn timeout — a 30s admin "context deadline
//     exceeded" or a multi-minute stuck chat turn. 30s evicts idle connections before
//     the middlebox does, so the pool opens a fresh connection instead of reusing a dead
//     one — matching shared/transport/http's inter-service default. Active polling (~5s)
//     still reuses a warm connection; only true >30s idle gaps pay a fresh handshake.
//
// Exported so the CLI can use it as the Base of its logging RoundTripper and keep these
// tunings; the kernel itself wraps it automatically (above).
func NewHTTPTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 30 * time.Second
	tr.IdleConnTimeout = 30 * time.Second
	return tr
}

// transportOf returns c's round-tripper, or a fresh widened transport when it has
// none — so the streaming client reuses an injected transport (test servers /
// mocks) yet still gets the widened handshake budget in production.
func transportOf(c *http.Client) http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return NewHTTPTransport()
}

// Env returns the environment this client targets.
func (c *Client) Env() Env { return c.env }

// AdminRequest performs an authenticated CP admin API call against an explicit
// path and returns the raw response body + HTTP status. It is the single
// execution path for the generic resource tools (the OpenAPI catalog supplies the
// method/path; this performs the call). Auth, refresh-on-401, base URL, and non-2xx
// classification are handled by roundtrip; the caller relays status+body to the
// model so a 403 (IAM) or 400 (validation) teaches it how to self-correct.
func (c *Client) AdminRequest(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error) {
	raw, status, err := c.roundtrip(ctx, method, c.env.CPBaseURL, path, query, body)
	return json.RawMessage(raw), status, err
}

const maxRespBody = 8 << 20 // 8 MiB cap on a single admin response body

// adminGet decodes a GET against the CP admin API into out.
func (c *Client) adminGet(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, c.env.CPBaseURL, path, query, nil, out)
}

// do performs one admin-authed request and decodes the 2xx body into out.
func (c *Client) do(ctx context.Context, method, baseURL, path string, query url.Values, body, out any) error {
	respBody, status, err := c.roundtrip(ctx, method, baseURL, path, query, body)
	if err != nil {
		return err
	}
	// An empty 2xx body (an empty 200 or a 204) is a success, not a decode failure —
	// leave out at its zero value rather than unmarshalling "" into a bogus ErrTransport.
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return &APIError{kind: ErrTransport, Status: status, Message: "decode response: " + err.Error()}
		}
	}
	return nil
}

// roundtrip attaches the admin credential, sends the request, maps non-2xx to a
// classified *APIError, and returns the raw 2xx body. Callers that need a typed
// value go through do; passthrough callers (simulator forward) keep the bytes.
func (c *Client) roundtrip(ctx context.Context, method, baseURL, path string, query url.Values, body any) ([]byte, int, error) {
	u := strings.TrimRight(baseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, &APIError{kind: ErrTransport, Message: "marshal request body: " + err.Error()}
		}
		raw = b
	}

	// attempt sends one request with the given auth header/value, returning the HTTP
	// status + body (or a transport error). Built fresh each call so a 401 retry gets an
	// un-consumed body reader.
	attempt := func(header, value string) (int, []byte, error) {
		var reqBody io.Reader
		if raw != nil {
			reqBody = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
		if err != nil {
			return 0, nil, &APIError{kind: ErrTransport, Message: "build request: " + err.Error()}
		}
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(header, value)
		resp, err := c.httpc.Do(req)
		if err != nil {
			return 0, nil, &APIError{kind: ErrTransport, Message: err.Error()}
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		return resp.StatusCode, respBody, nil
	}

	header, value, err := c.ts.Credential(ctx)
	if err != nil {
		return nil, 0, err // already a classified *APIError (ErrUnauthorized)
	}
	status, respBody, terr := attempt(header, value)
	if terr != nil {
		return nil, 0, terr
	}

	// Reactive refresh-on-401: the proactive near-expiry refresh in Credential covers
	// ordinary expiry, but a token the source believed valid can still be rejected
	// (revoked server-side, clock skew, or an exp it could not parse). If the source can
	// force a refresh, do it ONCE and retry — bounded to a single retry so a genuinely
	// unauthorized call cannot loop. Only retry if the refreshed credential actually
	// changed (otherwise the same token would just 401 again).
	if status == http.StatusUnauthorized {
		if r, ok := c.ts.(credentialRefresher); ok {
			if h2, v2, rerr := r.RefreshCredential(ctx, value); rerr == nil && v2 != value {
				if status, respBody, terr = attempt(h2, v2); terr != nil {
					return nil, 0, terr
				}
			}
		}
	}

	if status >= 400 {
		return nil, status, parseAPIError(status, respBody)
	}
	return respBody, status, nil
}

// parseAPIError turns a non-2xx response into a classified *APIError, reading
// the standard {error:{message,type,code}} envelope when present.
func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, kind: kindForStatus(status)}
	var env struct {
		Error struct {
			Message        string `json:"message"`
			Type           string `json:"type"`
			Code           string `json:"code"`
			Action         string `json:"action"`
			RequiredAction string `json:"requiredAction"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.Message = env.Error.Message
		e.Type = env.Error.Type
		e.Code = env.Error.Code
		e.IAMAction = firstNonEmpty(env.Error.Action, env.Error.RequiredAction)
	} else {
		e.Message = strings.TrimSpace(string(body))
		if e.Message == "" {
			e.Message = http.StatusText(status)
		}
	}
	return e
}
