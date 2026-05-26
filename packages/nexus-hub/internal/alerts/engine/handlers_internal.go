package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
)

// raiserAPI is the subset of *Raiser that HandleRaise / HandleResolve need.
// Declared here so tests can inject a mock without touching a database.
type raiserAPI interface {
	Raise(ctx context.Context, in RaiseInput) error
	Resolve(ctx context.Context, ruleID, targetKey, reason string) error
}

// HandleRaise accepts alertclient.AlertEnvelope JSON, constructs a RaiseInput,
// and calls raiser.Raise. Returns 200 on success, 400 on bad input, 500 on
// raiser failure.
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
// Returns 204 on success, 400 on bad input, 500 on raiser failure.
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
		if err := r.Resolve(req.Context(), body.RuleID, body.TargetKey, body.Reason); err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
