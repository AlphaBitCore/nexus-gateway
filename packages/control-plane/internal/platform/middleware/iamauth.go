package middleware

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
)

// DeviceGroupLookup is the seam IAM middleware uses to resolve which
// DeviceGroups a given device belongs to. Implemented by *store.DB in
// production (pulls from DeviceGroupMembership ∪ device_group_membership_cache
// once the membership cache is populated; static rows only until then). Declared as an interface
// so unit tests can wire an in-memory fake. Nil disables group-scope
// resolution and middleware falls back to the unscoped-only path —
// safe but maximally permissive for legacy policies.
type DeviceGroupLookup interface {
	GroupsOfDevice(ctx context.Context, deviceID string) ([]string, error)
}

// RequireIAMPermission returns Echo middleware that evaluates IAM policies
// for admin API routes.
//
// action is the IAM action (e.g. iam.ResourceProvider.Action(iam.VerbRead)).
// resourceFn extracts the NRN from the request; nil uses a wildcard resource.
func RequireIAMPermission(engine *iam.Engine, action string, resourceFn func(echo.Context) string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			aa := AdminAuthFromContext(c)
			if aa == nil {
				return c.JSON(http.StatusUnauthorized, errorResp("Authentication required", "authentication_error", "AUTH_REQUIRED"))
			}

			// No principal ID is treated as privileged: every authenticated
			// caller is evaluated against IAM policy. There is deliberately
			// no magic-string ("bootstrap"/"dev") bypass — a future seed or
			// fixture minting such a subject must NOT gain unconditional
			// super-access. First-boot grants come from seeded
			// IAM policies, not from a hardcoded principal-ID short-circuit.

			// Derive the request NRN from the action so resource-scoped
			// policies (the seed.ts default) evaluate correctly. See
			// iam.BuildRequestNRNForAction for the contract; non-canonical
			// actions fall back to a fully-wildcarded NRN.
			resource := iam.BuildRequestNRNForAction(action)
			if resourceFn != nil {
				resource = resourceFn(c)
			}

			ctx := iam.ConditionContext{
				"nexus:SourceIp": c.RealIP(),
			}

			// Translate session auth principal type to IAM storage principal type.
			// Session context uses "admin_user" for dashboard JWT sessions;
			// IAM storage (IamPolicyAttachment, IamGroupMembership) uses
			// the "nexus_user" type.
			iamPrincipalType := aa.AuthPrincipalType
			if iamPrincipalType == "admin_user" {
				iamPrincipalType = "nexus_user"
			}

			// resources holds the candidate set EvaluateMulti scans
			// against each Statement's Resource pattern. Default: just
			// the single derived resource. The device-scoped variant
			// (RequireIAMPermissionForDevice) replaces this list with
			// unscoped + per-group candidates.
			resources := []string{resource}
			result, err := engine.EvaluateMulti(c.Request().Context(), iamPrincipalType, aa.KeyID, action, resources, ctx)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, errorResp("Authorization service error", "server_error", "IAM_EVAL_ERROR"))
			}

			// L3 metric: iam.eval_total{decision, cache} — cache flag comes
			// from EvaluationResult.CacheHit, set by Engine.loadPolicies.
			cacheLabel := "miss"
			if result.CacheHit {
				cacheLabel = "hit"
			}
			if metrics.IAMEvalTotal != nil {
				if result.Decision == "Deny" {
					metrics.IAMEvalTotal.With("deny", cacheLabel).Inc()
				} else {
					metrics.IAMEvalTotal.With("allow", cacheLabel).Inc()
				}
			}

			if result.Decision == "Deny" {
				return c.JSON(http.StatusForbidden, map[string]any{
					"error": map[string]any{
						"message": "Access denied by IAM policy",
						"type":    "authorization_error",
						"code":    "IAM_ACCESS_DENIED",
						"details": map[string]any{
							"action":   action,
							"resource": resource,
							"reason":   result.Reason,
						},
					},
				})
			}

			return next(c)
		}
	}
}

// RequireIAMPermissionForDevice is the device-scope-aware variant of
// RequireIAMPermission. It builds the candidate resource list from
// (unscoped agent-device NRN) + (per-group NRN for each group the
// device belongs to), then evaluates against the full candidate set
// via Engine.EvaluateMulti. A policy with Resource scoped to
// `agent-device/group:<id>/*` matches only when the target device is
// a member of group `<id>`; an unscoped policy continues to match
// every device, preserving backward compat.
//
// Use it on routes that operate on a single device identified by a
// path parameter:
//
//	g.POST("/agent-devices/:id/force-refresh",
//	  h.ForceRefreshAgentDevice,
//	  iamMWForDevice(iam.ResourceAgentDevice.Action(iam.VerbForceResync), "id"))
//
// deviceIDParam is the Echo path-parameter name. lookup may be nil —
// when nil the middleware behaves exactly like RequireIAMPermission
// (unscoped only), which is the safe default for any handler that
// hasn't wired group resolution yet. Once the membership caches are live and the
// DB-backed lookup is constructed in main.go, every device-scoped
// route should switch to this middleware.
func RequireIAMPermissionForDevice(engine *iam.Engine, action, deviceIDParam string, lookup DeviceGroupLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			aa := AdminAuthFromContext(c)
			if aa == nil {
				return c.JSON(http.StatusUnauthorized, errorResp("Authentication required", "authentication_error", "AUTH_REQUIRED"))
			}
			// No magic-string ("bootstrap"/"dev") IAM bypass — see the note
			// in RequireIAMPermission.

			deviceID := c.Param(deviceIDParam)
			if deviceID == "" {
				// Missing path param — fall back to unscoped check
				// rather than crash, so misuse degrades to old
				// behaviour instead of a 500.
				return RequireIAMPermission(engine, action, nil)(next)(c)
			}

			// Resolve group memberships. A lookup error is logged but
			// must not authorise the request — fail closed by treating
			// the device as having no group memberships (only unscoped
			// candidate remains, so scoped-policy admins are denied,
			// fleet-wide admins still allowed).
			var deviceGroups []string
			if lookup != nil {
				gs, err := lookup.GroupsOfDevice(c.Request().Context(), deviceID)
				if err == nil {
					deviceGroups = gs
				}
				// On err, deviceGroups stays empty — fail-closed.
			}

			resources := iam.BuildDeviceCandidateNRNs(action, deviceID, deviceGroups)

			ctxCond := iam.ConditionContext{
				"nexus:SourceIp": c.RealIP(),
			}

			iamPrincipalType := aa.AuthPrincipalType
			if iamPrincipalType == "admin_user" {
				iamPrincipalType = "nexus_user"
			}

			result, err := engine.EvaluateMulti(c.Request().Context(), iamPrincipalType, aa.KeyID, action, resources, ctxCond)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, errorResp("Authorization service error", "server_error", "IAM_EVAL_ERROR"))
			}

			cacheLabel := "miss"
			if result.CacheHit {
				cacheLabel = "hit"
			}
			if metrics.IAMEvalTotal != nil {
				if result.Decision == "Deny" {
					metrics.IAMEvalTotal.With("deny", cacheLabel).Inc()
				} else {
					metrics.IAMEvalTotal.With("allow", cacheLabel).Inc()
				}
			}

			if result.Decision == "Deny" {
				return c.JSON(http.StatusForbidden, map[string]any{
					"error": map[string]any{
						"message": "Access denied by IAM policy",
						"type":    "authorization_error",
						"code":    "IAM_ACCESS_DENIED",
						"details": map[string]any{
							"action":       action,
							"deviceId":     deviceID,
							"deviceGroups": deviceGroups,
							"reason":       result.Reason,
						},
					},
				})
			}

			return next(c)
		}
	}
}
