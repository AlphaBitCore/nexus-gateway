// Package rulepacks: rulepacks_errors.go — store-error to HTTP-status mapping.
//
// Kept apart from the handlers so every rule-pack write classifies failures the
// same way, and so the classification is reviewable on its own. The rule that
// matters here: a rejected rule or override is the CALLER's mistake. Reporting
// it as 500 sends an operator hunting a server fault when the fix is a severity
// typo in their own request body — and until S-13, rulepack.InvalidRulesError
// had no status mapping anywhere, so every validation rejection did exactly
// that.
package rulepacks

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// respondInvalidRules renders an *InvalidRulesError as 400 with the per-rule
// reasons, so an admin fixing a batch learns WHICH entries were refused and why
// in one round trip instead of one-at-a-time. Nothing is persisted when this is
// returned — the store validates the whole batch before its first write.
func respondInvalidRules(c echo.Context, invalid *rulepack.InvalidRulesError) error {
	return c.JSON(http.StatusBadRequest, map[string]any{
		"error":  "validation_failed",
		"detail": invalid.Error(),
		"rules":  invalid.Errors,
	})
}

// respondImportError maps an ImportPack failure: 400 when validateRules
// rejected a rule (bad severity, uncompilable pattern), 409 on a duplicate
// (name, version), 500 otherwise.
func respondImportError(c echo.Context, err error) error {
	var invalid *rulepack.InvalidRulesError
	switch {
	case errors.As(err, &invalid):
		return respondInvalidRules(c, invalid)
	case errors.Is(err, rulepack.ErrDuplicatePackVersion):
		return c.JSON(http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

// respondWriteError maps a write that has no status-bearing failures of its own
// beyond validation: 400 on a rejected batch, 500 on anything else.
func respondWriteError(c echo.Context, err error) error {
	var invalid *rulepack.InvalidRulesError
	if errors.As(err, &invalid) {
		return respondInvalidRules(c, invalid)
	}
	return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
}
