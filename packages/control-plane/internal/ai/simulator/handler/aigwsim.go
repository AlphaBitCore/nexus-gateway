// Package handler — admin_ai_gateway_simulator.go forwards AI Gateway
// Client Simulator requests through the Control Plane backend so the
// browser only ever talks to the same HTTPS origin (no mixed-content
// blocker against an http://localhost:3050 ai-gateway when the CP UI
// runs over https://).
//
// Auth posture: the route mounts on the admin group,
// which already enforces the cookie/JWT session middleware — anyone
// hitting it must be a logged-in admin. No additional IAM action gate
// is applied; the simulator is a debugging tool and the gateway-side
// VK is itself the credential boundary.
//
// SSRF posture: the upstream host is NOT caller-controlled — every request is
// forwarded to the server-configured gateway (configuredGatewayURL), so a
// caller cannot point the proxy at an arbitrary internal host. The path must be
// one of the OpenAI-compatible surfaces the simulator actually drives
// (`/v1/models`, `/v1/chat/completions`, `/v1/messages`, `/v1/usage`, or a
// Gemini per-model endpoint); the method must be GET or POST. Together these
// stop the proxy from being used as a generic outbound HTTP client.
package aigwsim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/labstack/echo/v4"
)

// configuredGatewayURL returns the server-configured AI-gateway base URL
// (env AI_GATEWAY_URL, or the local default) and validates it is a well-formed
// http(s) URL with a host. The simulator ALWAYS forwards here: the caller
// cannot choose the upstream host (otherwise any admin-session
// principal, including a read-only viewer, could drive an arbitrary
// internal-network SSRF read). A misconfigured AI_GATEWAY_URL is an operator
// error, surfaced to the caller as a 500 rather than a 400.
func configuredGatewayURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv("AI_GATEWAY_URL"))
	if raw == "" {
		raw = "http://localhost:3050"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("configured AI_GATEWAY_URL %q: %w", raw, err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("configured AI_GATEWAY_URL scheme %q is not http/https", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("configured AI_GATEWAY_URL %q has no host", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// simulatorForwardRequest is the JSON body the UI posts. The upstream host is
// NOT part of it — see configuredGatewayURL. body is sent to the gateway as the
// upstream request body when method is POST; for GET it's ignored.
type simulatorForwardRequest struct {
	Path   string          `json:"path"`
	Method string          `json:"method"`
	VK     string          `json:"vk"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// allowedSimulatorPaths is the closed set of upstream paths the simulator
// is allowed to forward to. Drives both the SSRF check and a clear 400
// error message when an admin tampers with the UI to try something else.
var allowedSimulatorPaths = map[string]struct{}{
	"/v1/models":           {},
	"/v1/chat/completions": {},
	"/v1/messages":         {}, // Anthropic native ingress
	"/v1/usage":            {},
}

// isAllowedSimulatorPath returns true for a static-allowlist hit OR for
// Gemini's per-model endpoints, whose path embeds the model name and
// therefore can't sit in a fixed map. The structural shape
// `/v1beta/models/{model}:(stream)?generateContent` is what we actually
// trust — the model segment itself is opaque, but the simulator UI is
// the only producer of these paths so the surface is well-defined.
//
// Defense-in-depth: reject `..` and `?`/`#` so a tampered model
// segment can't smuggle a path-traversal or alt-route past the prefix
// check. The simulator UI never produces such characters in legitimate
// model names.
func isAllowedSimulatorPath(p string) bool {
	if p == "" || strings.Contains(p, "..") || strings.ContainsAny(p, "?#") {
		return false
	}
	if _, ok := allowedSimulatorPaths[p]; ok {
		return true
	}
	if strings.HasPrefix(p, "/v1beta/models/") {
		if strings.HasSuffix(p, ":generateContent") || strings.HasSuffix(p, ":streamGenerateContent") {
			return true
		}
	}
	return false
}

// validateForwardRequest enforces the caller-controlled SSRF mitigations: the
// path must be in the simulator allowlist and the method must be GET or POST.
// The upstream host is not caller-controlled (see configuredGatewayURL).
// Returned errors are written verbatim into the 400 body so a debugging admin
// can see why the call was rejected.
func validateForwardRequest(req *simulatorForwardRequest) error {
	if !isAllowedSimulatorPath(req.Path) {
		return fmt.Errorf("path %q is not in the simulator allowlist", req.Path)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch method {
	case http.MethodGet, http.MethodPost:
	default:
		return fmt.Errorf("method %q is not allowed (GET or POST only)", req.Method)
	}
	if req.VK == "" {
		return errors.New("vk is required")
	}
	return nil
}

// AIGatewaySimulatorForward proxies an AI-Gateway-Simulator request from
// the admin UI through to the server-configured gateway. Streaming
// responses (chat completion stream) flush per upstream chunk so the
// browser sees SSE deltas in real time. Aborting the browser request
// cancels the upstream call via the request context.
func (h *Handler) AIGatewaySimulatorForward(c echo.Context) error {
	var body simulatorForwardRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if err := validateForwardRequest(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}
	base, err := configuredGatewayURL()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON(err.Error(), "config_error", ""))
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	target := base + body.Path

	client := newSimulatorForwardClient()

	var upstreamBody io.Reader
	if method == http.MethodPost && len(body.Body) > 0 {
		upstreamBody = bytes.NewReader(body.Body)
	}

	upstreamReq, err := http.NewRequestWithContext(c.Request().Context(), method, target, upstreamBody)
	if err != nil {
		return c.JSON(http.StatusBadGateway, errJSON("build upstream request: "+err.Error(), "upstream_error", ""))
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(body.VK))
	if method == http.MethodPost {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}
	// Pass through Accept so SSE responses are negotiated correctly.
	if accept := c.Request().Header.Get("Accept"); accept != "" {
		upstreamReq.Header.Set("Accept", accept)
	}

	resp, err := client.Do(upstreamReq)
	if err != nil {
		// A canceled context manifests as net.Error / url.Error — surface
		// 499 (client-closed) so the UI can distinguish "abort" from
		// "upstream is down" without parsing error strings.
		if ctxErr := c.Request().Context().Err(); ctxErr != nil {
			return c.NoContent(499)
		}
		return c.JSON(http.StatusBadGateway, errJSON("upstream request failed: "+err.Error(), "upstream_error", ""))
	}
	defer resp.Body.Close() //nolint:errcheck

	// Mirror upstream Content-Type so the browser parses SSE / JSON correctly.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Response().Header().Set("Content-Type", ct)
	}
	c.Response().WriteHeader(resp.StatusCode)

	// Pipe the response body to the client. For SSE, flush after every
	// write so events reach the browser in real time. ResponseController
	// reaches the true underlying writer regardless of Echo wrapper depth,
	// avoiding the common pitfall where the Flusher type-assertion on
	// c.Response().Writer returns nil and silently disables flushing.
	rc := http.NewResponseController(c.Response().Writer)
	io.Copy(&simulatorFlushWriter{w: c.Response().Writer, rc: rc}, resp.Body) //nolint:errcheck
	return nil
}

// simulatorFlushWriter flushes after every Write so SSE chunks are
// forwarded to the browser immediately rather than buffered until EOF.
type simulatorFlushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (fw *simulatorFlushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		_ = fw.rc.Flush()
	}
	return n, err
}

// simulatorForwardTimeout bounds how long the simulator will wait for the
// upstream AI Gateway to begin responding (TCP connect + TLS + first
// response header). It is applied as Transport.ResponseHeaderTimeout, NOT
// as Client.Timeout: a flat Client.Timeout would also cap the body read and
// thus truncate a long-running streaming chat completion mid-stream, which
// is exactly the surface the simulator exists to exercise. 120s is generous
// enough that a slow-but-alive gateway (cold provider, queued LLM request)
// still gets through, while a wedged/black-holed upstream that never sends
// headers can no longer hang the admin request indefinitely.
const simulatorForwardTimeout = 120 * time.Second

// newSimulatorForwardClient builds the HTTP client used to forward simulator
// requests to the configured AI Gateway.
//
// Client.Timeout stays 0 on purpose: the request inherits the browser's
// lifetime via the request context, so cancellation (operator hits Stop)
// propagates upstream, and an in-progress SSE stream is allowed to run for
// as long as the gateway keeps sending bytes. The upper bound that prevents
// a hung upstream from pinning the connection forever lives on the
// transport's ResponseHeaderTimeout (= simulatorForwardTimeout). The
// transport is wrapped via nexushttp.WrapTransport so outbound debug logging
// and request-id propagation still apply on this path.
func newSimulatorForwardClient() *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = simulatorForwardTimeout
	return &http.Client{
		Timeout: 0,
		Transport: nexushttp.WrapTransport(base, nexushttp.WrapOpts{
			Caller:         "cp-admin-aiguard-simulator",
			PropagateReqID: true,
		}),
	}
}
