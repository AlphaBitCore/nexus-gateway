// Package patternperf is the CP BFF proxy for the authoring-time pattern
// performance test. The control plane has no libhs, so the actual Vectorscan
// measurement runs on the AI Gateway; this forwards the author's regex over the
// INTERNAL_SERVICE_TOKEN-gated /internal hop (same trust model as provider
// discover / embedding probe) and relays the result to the rule-pack and hook
// regex editors.
package patternperf

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// Handler forwards pattern perf-test requests to the AI Gateway.
type Handler struct {
	gatewayURL    string
	internalToken string
	logger        *slog.Logger
}

// New builds the handler from the BFF's AI-Gateway base URL + internal token.
func New(gatewayURL, internalToken string, logger *slog.Logger) *Handler {
	return &Handler{gatewayURL: gatewayURL, internalToken: internalToken, logger: logger}
}

// RegisterRoutes wires POST /rule-packs/pattern-perf-test onto the admin group.
//
// IAM: gated on rule-pack:update — this is an authoring-time validation helper
// (the same authoring surface as rule-pack preview, which is also :update), not
// a read-only viewer affordance. The same endpoint serves hook regexes, which
// are authored by the same governance roles.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.POST("/rule-packs/pattern-perf-test", h.Test, iamMW(iam.ResourceRulePack.Action(iam.VerbUpdate)))
}

// Test forwards {pattern, flags} to the gateway's /internal/pattern-perf-test
// and relays its JSON verbatim. On a transport failure it returns HTTP 200 with
// success:false + detail so the editor can show a user-readable message rather
// than an opaque 5xx.
func (h *Handler) Test(c echo.Context) error {
	var body struct {
		Pattern string `json:"pattern"`
		Flags   string `json:"flags"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "invalid request body", "code": "validation_error"})
	}
	if strings.TrimSpace(body.Pattern) == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "pattern is required", "code": "validation_error"})
	}

	gwURL := strings.TrimRight(h.gatewayURL, "/") + "/internal/pattern-perf-test"
	payload, _ := json.Marshal(map[string]string{"pattern": body.Pattern, "flags": body.Flags})

	client := nexushttp.New(nexushttp.Config{
		Timeout:        15 * time.Second,
		Caller:         "cp-pattern-perf-test",
		PropagateReqID: true,
	})
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodPost, gwURL, strings.NewReader(string(payload)))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"success": false, "error": "Failed to build request: " + err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.internalToken)

	resp, err := client.Do(req)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"success": false, "error": "AI Gateway unreachable: " + err.Error()})
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB — matches the ai-gateway internal cap
	c.Response().Header().Set("Content-Type", "application/json")
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Write(bodyBytes)
	return nil
}
