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
// SSRF mitigations: the target URL must be http(s); the path must be
// one of the OpenAI-compatible surfaces the simulator actually drives
// (`/v1/models`, `/v1/chat/completions`, `/v1/usage`); the method must
// be GET or POST. These together stop the proxy from being used as a
// generic outbound HTTP client.
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

// defaultSimulatorTargetURL is the fallback ai-gateway base URL used
// when the request body leaves targetUrl empty. The UI no longer asks
// the operator to type a URL — CP already knows where the gateway is —
// so this default is the canonical value for "the local stack". Prod
// deployments override via env (typically the same env that the
// admin UI reads via /api/admin/me / config endpoints).
func defaultSimulatorTargetURL() string {
	if v := strings.TrimSpace(os.Getenv("AI_GATEWAY_URL")); v != "" {
		return v
	}
	return "http://localhost:3050"
}

// simulatorForwardRequest is the JSON body the UI posts. body is sent
// to the gateway as the upstream request body when method is POST; for
// GET it's ignored.
type simulatorForwardRequest struct {
	TargetURL string          `json:"targetUrl"`
	Path      string          `json:"path"`
	Method    string          `json:"method"`
	VK        string          `json:"vk"`
	Body      json.RawMessage `json:"body,omitempty"`
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

// validateForwardRequest enforces the SSRF mitigations described in the
// file header. Returned errors are written verbatim into the 400 body
// so a debugging admin can see why the call was rejected.
//
// Empty TargetURL is replaced in place with the server-configured
// default (env AI_GATEWAY_URL or localhost:3050). The UI relies
// on this so the gateway URL never has to be typed for the common
// "I just want to test the local stack" case.
func validateForwardRequest(req *simulatorForwardRequest) error {
	if strings.TrimSpace(req.TargetURL) == "" {
		req.TargetURL = defaultSimulatorTargetURL()
	}
	parsed, err := url.Parse(strings.TrimSpace(req.TargetURL))
	if err != nil {
		return fmt.Errorf("targetUrl: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("targetUrl scheme %q is not allowed (http/https only)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("targetUrl has no host")
	}
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
// the admin UI through to the user-supplied gateway URL. Streaming
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

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	target := strings.TrimRight(strings.TrimSpace(body.TargetURL), "/") + body.Path

	// We can't reuse `nexushttp.New` directly because it forces a default
	// timeout that's too short for streaming chat completions. Instead we
	// keep `Timeout: 0` (the proxy inherits the request lifetime from the
	// browser via ctx so cancellation propagates upstream when the
	// operator hits Stop) AND route the transport through
	// `nexushttp.WrapTransport` so outbound debug logging + request-id
	// propagation still apply on this path.
	client := &http.Client{
		Timeout: 0,
		Transport: nexushttp.WrapTransport(http.DefaultTransport, nexushttp.WrapOpts{
			Caller:         "cp-admin-aiguard-simulator",
			PropagateReqID: true,
		}),
	}

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

// simulatorForwardTimeout caps the connect/read budget when the UI is
// not actively streaming. Currently unused; kept here as a knob for a
// later admin-configurable upper bound on long-running chat tests.
const simulatorForwardTimeout = 10 * time.Minute

var _ = simulatorForwardTimeout
