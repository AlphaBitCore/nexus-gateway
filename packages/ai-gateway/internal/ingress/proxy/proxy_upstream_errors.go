package proxy

// proxy_upstream_errors.go carries the upstream-failure classification to the
// two operator-facing surfaces that exist for it: the errors_total counter and
// the diagnostic event stream behind the operator errors page. The executor
// already normalises every adapter outcome onto one canonical code; these
// helpers are the boundary that stops it being flattened back into a single
// undifferentiated bucket on the way out.

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// Messages logged for a failed upstream attempt, one constant per cause.
//
// Two properties make these strings a shipped surface rather than prose, and
// both are load-bearing:
//
// The operator errors page groups issues by a hash of the message text alone —
// attributes are not part of the key. One message for every cause therefore
// collapses every gateway failure into a single row, and per-cause silencing
// becomes impossible: silencing rate-limit noise would silence credential
// failures with it. Splitting the text by cause is what gives each cause its
// own row.
//
// For the same reason no per-request value may ever be interpolated into one of
// these strings. A provider or model spliced into the text mints a fresh issue
// group per distinct value — a provider x model cartesian product of
// one-event groups — which buries the page and inflates the event table.
// Variables belong in the attributes, which do not affect the hash.
//
// And because a silence is keyed on the message, rewording one silently
// un-silences whatever an operator had muted. Treat the text as an identifier:
// choose it once, change it never.
const (
	msgUpstreamRateLimited         = "upstream attempt failed: provider rate limited"
	msgUpstreamTimeout             = "upstream attempt failed: provider timed out"
	msgUpstreamServerError         = "upstream attempt failed: provider returned a server error"
	msgUpstreamContextOverflow     = "upstream attempt failed: prompt exceeds the model context window"
	msgUpstreamInvalidRequest      = "upstream attempt failed: provider rejected the request as invalid"
	msgUpstreamAuthFailed          = "upstream attempt failed: provider rejected the credential"
	msgUpstreamEndpointUnsupported = "upstream attempt failed: provider does not serve this endpoint"
	msgUpstreamNotImplemented      = "upstream attempt failed: provider capability not implemented"
	msgUpstreamNoCompatible        = "upstream attempt failed: no compatible provider"
	msgUpstreamTransport           = "upstream attempt failed: transport error"
	msgUpstreamNotDispatched       = "upstream target unusable before dispatch"
)

// errorTypeUnclassified is the errors_total label for a dispatched attempt that
// produced no canonical code (a transport failure that never got a provider
// envelope). Named rather than left empty so the counter reads as a deliberate
// bucket instead of a missing label.
const errorTypeUnclassified = "unclassified"

// upstreamFailureMessage picks the constant message for one failed attempt.
// The returned string is always a literal from the block above — never a
// formatted value.
func upstreamFailureMessage(a executor.Attempt) string {
	if !a.Dispatched {
		return msgUpstreamNotDispatched
	}
	switch a.Code {
	case provcore.CodeRateLimited:
		return msgUpstreamRateLimited
	case provcore.CodeTimeout:
		return msgUpstreamTimeout
	case provcore.CodeUpstreamError:
		return msgUpstreamServerError
	case provcore.CodeContextOverflow:
		return msgUpstreamContextOverflow
	case provcore.CodeInvalidRequest:
		return msgUpstreamInvalidRequest
	case provcore.CodeAuthFailed:
		return msgUpstreamAuthFailed
	case provcore.CodeEndpointUnsupported:
		return msgUpstreamEndpointUnsupported
	case provcore.CodeNotImplemented:
		return msgUpstreamNotImplemented
	case provcore.CodeNoCompatibleProvider:
		return msgUpstreamNoCompatible
	default:
		return msgUpstreamTransport
	}
}

// errorType is the errors_total label for one attempt: the canonical code, or
// a named bucket when the attempt carries none. Cardinality is bounded by the
// canonical code set plus the two sentinels, which is the reason the counter
// takes the code rather than free-form error text.
func errorType(a executor.Attempt) string {
	switch {
	case !a.Dispatched:
		return "not_dispatched"
	case a.Code == "":
		return errorTypeUnclassified
	default:
		return a.Code
	}
}

// logUpstreamFailures emits one diagnostic event per failed attempt, each under
// the message that names its cause, splitting which group an event lands in
// without changing how many are emitted per call.
//
// Per call is the caveat: the caller must not reach here on a rate-limited
// exhaust. That path returns before this, because a rate-limit storm is
// high-volume by definition and one event per attempt would be unbounded
// volume on the hottest failure mode. Rate limits are counted, not logged.
func logUpstreamFailures(logger *slog.Logger, attempts []executor.Attempt) {
	for i, a := range attempts {
		if a.Error == "" {
			continue
		}
		logger.Error(upstreamFailureMessage(a),
			"attempt", i+1,
			"error_code", errorType(a),
			"provider", a.Target.ProviderName,
			"model", a.Target.ModelCode,
			"reason", a.Error)
	}
}

// recordUpstreamFailure increments errors_total for the attempt that decided a
// failed request. Nil terminal means no call ever reached a provider, so there
// is no provider to attribute the failure to and nothing is counted.
func (h *Handler) recordUpstreamFailure(terminal *executor.Attempt) {
	if h.deps.Metrics == nil || terminal == nil {
		return
	}
	h.deps.Metrics.RecordError(terminal.Target.ProviderName, errorType(*terminal))
}
