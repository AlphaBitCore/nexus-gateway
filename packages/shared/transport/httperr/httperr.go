// Package httperr provides the canonical cross-service JSON error envelope.
// All Nexus Gateway services use {"error":{"message","type","code"}} as the
// single HTTP error shape, so callers can parse any service response identically.
package httperr

import (
	"github.com/goccy/go-json"
	"net/http"
)

// ErrJSON builds {"error":{"message","type","code"}} — the canonical error envelope.
func ErrJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

// WriteError writes the canonical error envelope to a raw http.ResponseWriter.
// Use this for handlers that don't use Echo; Echo handlers use c.JSON(status, ErrJSON(...)).
func WriteError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrJSON(message, errType, code))
}
