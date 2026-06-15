// Package httperr provides the canonical JSON error envelope shared by all
// Control Plane admin handlers. Keeping a single definition here avoids the
// per-package copies of the same helper that previously drifted in formatting.
package httperr

// ErrJSON builds the canonical JSON error envelope used across admin handlers:
//
//	{"error": {"message": ..., "type": ..., "code": ...}}
func ErrJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}
