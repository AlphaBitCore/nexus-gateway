package alerting

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// raiserAPI is the subset of *Raiser that HandleRaise / HandleResolve need.
// Declared here so tests can inject a mock without touching a database.
type raiserAPI interface {
	Raise(ctx context.Context, in RaiseInput) error
	Resolve(ctx context.Context, ruleID, targetKey, reason string) error
}

// Caller carries the authenticated identity of the alerts API caller into the
// raw http.Handler raise/resolve paths. The Echo route adapter resolves the
// device-token Thing from the DeviceOrServiceAuth middleware context and stamps
// a Caller onto the request context; the handlers read it back to enforce
// per-Thing scoping.
//
//   - IsService=true  → the internal service token authenticated the call
//     (Control Plane / Hub-internal). Service callers may raise/resolve any
//     target.
//   - IsService=false → a device token authenticated the call. ThingID is the
//     authenticated Thing; the caller may only raise alerts whose target is
//     itself, and may NOT resolve alerts at all (resolving another Thing's
//     alert could suppress a live incident).
type Caller struct {
	IsService bool
	ThingID   string
}

type callerCtxKey struct{}

// WithCaller returns a child context carrying the authenticated caller. Used by
// the Echo route adapter in the handler package.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// callerFromContext extracts the Caller stamped by WithCaller. The second
// return is false when no caller was set (e.g. a unit test that exercises the
// handler directly without the route adapter) — in that case the handlers fall
// back to the pre-scoping behaviour so existing internal callers keep working.
func callerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerCtxKey{}).(Caller)
	return c, ok
}

// targetThingID extracts the thing identifier from an alert TargetKey. Device
// alert targets are formatted `<sourceType>:<thingID>` (e.g. `thing:n1`,
// `proxy:n1`) — see raiser.go and jobs/thing_offline_alerts.go. A key without a
// colon is returned verbatim. The thing portion is what a device caller must
// match to raise an alert about itself.
func targetThingID(targetKey string) string {
	if i := strings.IndexByte(targetKey, ':'); i >= 0 {
		return targetKey[i+1:]
	}
	return targetKey
}

// HandleRaise accepts alertclient.AlertEnvelope JSON, constructs a RaiseInput,
// and calls raiser.Raise. Returns 200 on success, 400 on bad input, 403 when a
// device caller targets a Thing other than itself, 500 on raiser failure.
func HandleRaise(r raiserAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body alertclient.AlertEnvelope
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.RuleID == "" || body.TargetKey == "" {
			httpErr(w, http.StatusBadRequest, "ruleId and targetKey required")
			return
		}
		// Per-Thing scoping: a device-token caller may only raise alerts whose
		// target is itself. Service-token callers (CP / Hub-internal) are
		// unrestricted. A caller absent from context (direct unit-test call)
		// is treated as trusted to preserve existing behaviour.
		if caller, ok := callerFromContext(req.Context()); ok && !caller.IsService {
			if targetThingID(body.TargetKey) != caller.ThingID {
				httpErr(w, http.StatusForbidden,
					"device caller may only raise alerts for its own thing")
				return
			}
		}
		if body.FiredAt.IsZero() {
			body.FiredAt = time.Now().UTC()
		}
		err := r.Raise(req.Context(), RaiseInput{
			RuleID:      body.RuleID,
			TargetKey:   body.TargetKey,
			TargetLabel: body.TargetLabel,
			Severity:    Severity(body.Severity),
			Message:     body.Message,
			Details:     body.Details,
			FiredAt:     body.FiredAt,
		})
		if err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// HandleResolve accepts alertclient.ResolveRequest JSON, calls raiser.Resolve.
// Returns 204 on success, 400 on bad input, 403 when a device caller attempts a
// resolve, 500 on raiser failure.
func HandleResolve(r raiserAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body alertclient.ResolveRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if body.RuleID == "" || body.TargetKey == "" {
			httpErr(w, http.StatusBadRequest, "ruleId and targetKey required")
			return
		}
		// Only service-token callers may resolve. A device token resolving
		// another Thing's alert could suppress a real incident; even resolving
		// its own alert is reserved for the Hub-internal evaluation path.
		if caller, ok := callerFromContext(req.Context()); ok && !caller.IsService {
			httpErr(w, http.StatusForbidden,
				"device callers may not resolve alerts")
			return
		}
		if err := r.Resolve(req.Context(), body.RuleID, body.TargetKey, body.Reason); err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	codeStr := codeForStatus(code)
	nexushttperr.WriteError(w, code, msg, typeForCode(codeStr), codeStr)
}

// codeForStatus maps an HTTP status to the canonical machine-readable code
// string used by handler.ErrorResponse so both error paths agree.
func codeForStatus(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "INVALID_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusServiceUnavailable:
		return "SERVICE_UNAVAILABLE"
	default:
		return "INTERNAL_ERROR"
	}
}
