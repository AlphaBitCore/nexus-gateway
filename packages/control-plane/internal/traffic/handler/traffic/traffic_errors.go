package traffic

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// Attribution buckets for the traffic error-governance view. The bucket is a
// FIRST-SUSPECT triage priority, not a verdict: it says which side should
// look first, and it is a filter/sort dimension in the UI — never an
// exclusion gate.
const (
	attributionOurs     = "ours"
	attributionClient   = "client"
	attributionUpstream = "upstream"
	// attributionMixed marks the overflow pseudo-class: it aggregates classes
	// shed by the rollup cardinality cap, whose individual attributions are
	// unknowable. Any ?attribution filter excludes it — the row appears only
	// in the unfiltered view, where its count keeps the total exact.
	attributionMixed = "mixed"
)

// classifyTrafficErrorAttribution maps an aggregated error class to its
// first-suspect bucket, from the two error_code vocabularies documented on
// traffic_event.error_code (schema/traffic.prisma):
//
//   - "ours" — failures gateway routing / format translation / provider
//     configuration decided or should have prevented. auth_failed means the
//     UPSTREAM rejected the gateway's configured provider credential (the
//     caller's own key failures surface as AUTH_INVALID instead), and
//     context_overflow means routing placed the prompt on a model it cannot
//     fit — both admin-actionable.
//   - "client" — caller-side failures: bad or expired virtual key, the
//     caller's own rate/quota limits, the caller disconnecting, or a request
//     the provider rejected as malformed after faithful translation.
//   - "upstream" — provider-side failures passed through.
//
// Unclassified rows (empty error_code) split by status class: a 5xx with no
// code is a raw upstream error propagated unchanged; a 4xx with no code is a
// gateway-side rejection that predates classification — most commonly a
// compliance block or auth failure, so the caller is the first suspect.
func classifyTrafficErrorAttribution(errorCode, statusRange string) string {
	if errorCode == metrics.ErrorClassOverflowCode {
		return attributionMixed
	}
	switch errorCode {
	// BUMP_FAILED is the compliance-proxy failing to TLS-intercept —
	// interception infrastructure, admin-actionable.
	case "ROUTING_NO_MATCH", "USAGE_QUERY_FAILED",
		"no_compatible_provider", "not_implemented", "endpoint_unsupported",
		"context_overflow", "auth_failed", "BUMP_FAILED":
		return attributionOurs
	// invalid_request assumes faithful translation; a provider "malformed"
	// 400 can still be a gateway codec defect, so after adapter changes
	// check the "ours" lens too — first-suspect, not verdict.
	// POLICY_DENIED is a compliance block doing its job: caller-side.
	case "AUTH_INVALID", "AUTH_KEY_EXPIRED", "RATE_LIMITED", "QUOTA_EXCEEDED",
		"CLIENT_CLOSED", "invalid_request", "POLICY_DENIED":
		return attributionClient
	case "PROVIDER_UNAVAILABLE", "PROVIDER_RATE_LIMITED", "PROVIDER_ERROR":
		return attributionUpstream
	}
	if statusRange == "5xx" {
		return attributionUpstream
	}
	return attributionClient
}

// errorGroupsBucketInterval picks the sparkline bin width for the window:
// 5-minute bins up to 48h (max 576 bins), 1-hour bins up to 14 days
// (max 336), daily bins beyond — so the bucket query's cardinality stays
// bounded for ANY caller-supplied window, not just the ranges the UI
// offers (from/to are unrestricted API inputs).
func errorGroupsBucketInterval(from, to time.Time) time.Duration {
	window := to.Sub(from)
	if window <= 48*time.Hour {
		return 5 * time.Minute
	}
	if window <= 14*24*time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

// ListTrafficErrorGroups returns the aggregated error-governance view over
// traffic_event: top error classes by (errorCode, statusRange, provider,
// model) with counts, sparkline buckets, and a computed first-suspect
// attribution. Optional `?attribution=ours|client|upstream` narrows to one
// bucket; `?source=` accepts the same product domains as the traffic list.
func (h *Handler) ListTrafficErrorGroups(c echo.Context) error {
	fromStr := strings.TrimSpace(c.QueryParam("from"))
	toStr := strings.TrimSpace(c.QueryParam("to"))
	if fromStr == "" || toStr == "" {
		return c.JSON(http.StatusBadRequest, errJSON("from and to are required (RFC3339)", "validation_error", ""))
	}
	from, ok := parseRFC3339Flexible(fromStr)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON("invalid from (RFC3339)", "validation_error", ""))
	}
	to, ok := parseRFC3339Flexible(toStr)
	if !ok {
		return c.JSON(http.StatusBadRequest, errJSON("invalid to (RFC3339)", "validation_error", ""))
	}
	if !from.Before(to) {
		return c.JSON(http.StatusBadRequest, errJSON("from must be before to", "validation_error", ""))
	}
	attribution := strings.TrimSpace(c.QueryParam("attribution"))
	switch attribution {
	case "", attributionOurs, attributionClient, attributionUpstream:
	default:
		return c.JSON(http.StatusBadRequest, errJSON("invalid attribution (ours|client|upstream)", "validation_error", ""))
	}
	// mode=direct forces the raw traffic_event scan — an ops/verification
	// escape hatch for cross-checking the rollup path's numbers; the default
	// (auto) serves rollup-first with a direct tail for the unsealed window.
	mode := strings.TrimSpace(c.QueryParam("mode"))
	switch mode {
	case "", "auto", "direct":
	default:
		return c.JSON(http.StatusBadRequest, errJSON("invalid mode (auto|direct)", "validation_error", ""))
	}

	interval := errorGroupsBucketInterval(from, to)
	// Snap the window outward to bucket boundaries so the rollup path's
	// whole-bucket sums and the direct path's row filter cover the SAME
	// event population — an unaligned edge would otherwise under-count the
	// head bucket (rollup) versus direct by up to one bucket width. Both
	// modes use the snapped window; the response reports it.
	from = from.Truncate(interval)
	if snapped := to.Truncate(interval); snapped.Before(to) {
		to = snapped.Add(interval)
	}
	params := trafficstore.TrafficErrorGroupsParams{
		From:           from,
		To:             to,
		DBSources:      parseTrafficDomainParam(c.QueryParam("source")),
		BucketInterval: interval,
	}
	var groups []trafficstore.TrafficErrorGroup
	var dataSource string
	var err error
	if mode == "direct" {
		groups, err = h.traffic.ListTrafficErrorGroups(c.Request().Context(), params)
		dataSource = trafficstore.ErrorGroupsSourceDirect
	} else {
		groups, dataSource, err = h.traffic.ListTrafficErrorGroupsScaled(c.Request().Context(), params)
	}
	if err != nil {
		h.logger.Error("list traffic error groups", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Attribution is computed here (logic stays in Go; the store only
	// aggregates) and doubles as the optional response filter.
	filtered := make([]trafficstore.TrafficErrorGroup, 0, len(groups))
	for i := range groups {
		groups[i].Attribution = classifyTrafficErrorAttribution(groups[i].ErrorCode, groups[i].StatusRange)
		if attribution == "" || groups[i].Attribution == attribution {
			filtered = append(filtered, groups[i])
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"data":          filtered,
		"bucketSeconds": int(interval.Seconds()),
		"dataSource":    dataSource,
		"effectiveFrom": from.UTC().Format(time.RFC3339),
		"effectiveTo":   to.UTC().Format(time.RFC3339),
	})
}
