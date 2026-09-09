package iam

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"

	"github.com/labstack/echo/v4"

	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// IAM grant ceiling (permission boundary).
//
// IAM policy authoring and the grant operations (attach-policy-to-principal,
// attach-policy-to-group, add-group-member) each need a check that the
// permissions being conferred are a subset of the CALLER's own. Without one, a
// principal holding only a delegated `iam-policy.*` / `iam-group.*` scope can
// author or attach an `admin:*` policy to itself (or a group it belongs to) and
// silently become super-admin. This is the AWS-style no-privilege-escalation
// ceiling: a principal may never confer a permission it does not itself hold.
//
// The conferring chokepoints are guarded with ceilingBlocks below; the actual
// subset evaluation lives in the engine (cpiam.Engine.PrincipalCoversDocument),
// which super-admins pass automatically.

// callerPrincipal returns the authenticated caller's IAM principal identity,
// normalising the "admin_user" auth principal type to the "nexus_user" type the
// IAM engine stores policies under (mirrors isSuperAdmin elsewhere). ok=false
// when the request is unauthenticated.
func callerPrincipal(c echo.Context) (principalType, principalID string, ok bool) {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.KeyID == "" {
		return "", "", false
	}
	pt, _ := cpiam.NormalisePrincipalType(aa.AuthPrincipalType)
	return pt, aa.KeyID, true
}

// ceilingBlocks enforces the grant ceiling for a single candidate document.
// It returns blocked=true together with the echo response the handler MUST
// return when the grant would escalate beyond the caller's own permissions (or
// cannot be safely verified). blocked=false means the grant is within the
// caller's authority and the handler should proceed.
//
// Fail-closed: a missing IAM engine (503) or an evaluation error (500) blocks
// the grant — the ceiling is never silently skipped on the privilege-conferring
// path.
func (h *Handler) ceilingBlocks(c echo.Context, document cpiam.PolicyDocument) (blocked bool, resp error) {
	if h.iamEngine == nil {
		return true, c.JSON(http.StatusServiceUnavailable, errJSON("IAM engine not available", "server_error", ""))
	}
	pt, pid, ok := callerPrincipal(c)
	if !ok {
		return true, c.JSON(http.StatusForbidden, errJSON("Unauthenticated", "authorization_error", ""))
	}
	covered, missAction, missResource, err := h.iamEngine.PrincipalCoversDocument(c.Request().Context(), pt, pid, document)
	if err != nil {
		h.logger.Error("grant-ceiling evaluation failed", "error", err)
		return true, c.JSON(http.StatusInternalServerError, errJSON("Failed to verify permissions", "server_error", ""))
	}
	if !covered {
		return true, c.JSON(http.StatusForbidden, errJSON(
			"Cannot grant a permission you do not hold: "+missAction+" on "+missResource,
			"authorization_error", "PRIVILEGE_ESCALATION_BLOCKED"))
	}
	return false, nil
}

// ceilingBlocksPrincipal is the shared no-privilege-escalation check for any
// operation that would hand the caller another principal's authority — as
// opposed to conferring a named policy, which ceilingBlocks covers. It requires
// the caller to already hold every permission the target holds.
//
// Two families of operation reach it, and they are the same problem wearing two
// shapes. Minting an admin API key whose ownerUserId is set produces a
// credential that authenticates AS that owner (authn.EffectivePrincipal). Setting
// another user's local password lets the caller sign in as them at
// /authserver/password, after which every request is evaluated under THEIR
// policies. Either one bypasses the whole ceiling apparatus not by out-granting
// the owner but by becoming them.
//
// refusal names the operation in the 403 body so an operator can tell which door
// they hit. Fail-closed identically to ceilingBlocks: a missing IAM engine
// (503), an evaluation error (500), or an unauthenticated request (403) blocks.
func (h *Handler) ceilingBlocksPrincipal(c echo.Context, targetType, targetID, refusal string) (blocked bool, resp error) {
	pt, pid, ok := callerPrincipal(c)
	if !ok {
		return true, c.JSON(http.StatusForbidden, errJSON("Unauthenticated", "authorization_error", ""))
	}
	// Acting on yourself confers nothing you do not already hold, so it is
	// permitted without consulting the engine. Checked BEFORE the engine-missing
	// refusal on purpose: a deployment with no engine must still let an operator
	// rotate their own credential or set their own password.
	if pt == targetType && pid == targetID {
		return false, nil
	}
	if h.iamEngine == nil {
		return true, c.JSON(http.StatusServiceUnavailable, errJSON("IAM engine not available", "server_error", ""))
	}
	covered, missAction, missResource, err := h.iamEngine.PrincipalCoversPrincipal(c.Request().Context(), pt, pid, targetType, targetID)
	if err != nil {
		h.logger.Error("grant-ceiling (principal) evaluation failed", "error", err)
		return true, c.JSON(http.StatusInternalServerError, errJSON("Failed to verify permissions", "server_error", ""))
	}
	if !covered {
		return true, c.JSON(http.StatusForbidden, errJSON(
			refusal+": "+missAction+" on "+missResource,
			"authorization_error", "PRIVILEGE_ESCALATION_BLOCKED"))
	}
	return false, nil
}

// ceilingBlocksKeyOwner is the single predicate every admin-API-key MINTING
// path shares — create, regenerate and rotate. It owns the skip conditions so
// they cannot be spelled three ways: an unowned key delegates to nobody, and a
// key a caller mints for themselves confers nothing they do not already hold.
//
// Create had this check from the day it was written; regenerate and rotate did
// not, and both mint an equally usable plaintext credential for the SAME owner.
// A caller holding only admin:api-key.update could therefore pick a
// super-admin-owned key, POST /api-keys/{id}/regenerate, and read the plaintext
// out of the 200 body. Rotate is the same defect one verb along — its successor
// inherits the predecessor's owner.
//
// Adding a fourth minting path now requires deliberately NOT calling this;
// before, it silently inherited the hole.
func (h *Handler) ceilingBlocksKeyOwner(c echo.Context, ownerID *string) (blocked bool, resp error) {
	if ownerID == nil || *ownerID == "" {
		return false, nil // unowned key: authenticates as itself, delegates nothing
	}
	// The owner-is-the-caller skip lives in ceilingBlocksPrincipal, shared with
	// the password path.
	return h.ceilingBlocksPrincipal(c, "nexus_user", *ownerID,
		"Cannot mint a key for an owner whose permission you do not hold")
}

// ceilingBlocksRaw parses a raw policy document and enforces the ceiling. A
// malformed document is reported as a 400 so the conferring handlers never
// persist an undecodable blob (the structural validator runs separately).
func (h *Handler) ceilingBlocksRaw(c echo.Context, raw []byte) (blocked bool, resp error) {
	var document cpiam.PolicyDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return true, c.JSON(http.StatusBadRequest, errJSON("document is not valid JSON", "validation_error", "document"))
	}
	return h.ceilingBlocks(c, document)
}

// ceilingBlocksPolicyID loads the document of an existing policy by id and
// enforces the ceiling against it — used by the attach-policy chokepoints which
// confer an already-persisted policy. A policy that cannot be loaded blocks the
// grant fail-closed (the attach would also fail downstream, but the ceiling must
// not be skipped on a read error). A not-found policy is left to the downstream
// attach call to reject.
func (h *Handler) ceilingBlocksPolicyID(c echo.Context, policyID string) (blocked bool, resp error) {
	document, found, err := h.policyDocumentByID(c.Request().Context(), policyID)
	if err != nil {
		h.logger.Error("grant-ceiling: load policy document", "policyId", policyID, "error", err)
		return true, c.JSON(http.StatusInternalServerError, errJSON("Failed to verify permissions", "server_error", ""))
	}
	if !found {
		// Nothing conferred (the attach will reject the missing policy); do not
		// block on the ceiling for a policy that does not exist.
		return false, nil
	}
	return h.ceilingBlocks(c, document)
}

// policyDocumentByID fetches and decodes a policy's document. found=false means
// the policy row was absent.
func (h *Handler) policyDocumentByID(ctx context.Context, policyID string) (cpiam.PolicyDocument, bool, error) {
	row, err := h.iam.GetIamPolicy(ctx, policyID)
	if err != nil {
		return cpiam.PolicyDocument{}, false, err
	}
	if row == nil || len(row.Document) == 0 {
		return cpiam.PolicyDocument{}, false, nil
	}
	var document cpiam.PolicyDocument
	if err := json.Unmarshal(row.Document, &document); err != nil {
		return cpiam.PolicyDocument{}, false, err
	}
	return document, true, nil
}
