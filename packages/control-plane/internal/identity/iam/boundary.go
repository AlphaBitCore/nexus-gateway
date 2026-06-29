package iam

import (
	"context"
	"sort"
	"strings"

	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// universalNRN is the broadest resource scope an IAM statement can name — every
// service, scope, and resource id. A candidate statement that grants on the bare
// "*" is normalised to this NRN before the ceiling check so a principal whose own
// super-admin policy is written as `Resource: nrn:nexus:*:*:*/*` (the seeded
// shape) still covers a `Resource: "*"` grant, while a narrowly-scoped principal
// does not.
const universalNRN = "nrn:nexus:*:*:*/*"

// PrincipalCoversDocument reports whether the principal (principalType,
// principalID) already holds — via its own effective IAM policies — every
// permission the candidate document would confer. It is the grant-ceiling /
// permission-boundary primitive: a principal must never
// be able to author, attach, or otherwise grant a permission it does not itself
// possess, which would be privilege escalation toward super-admin.
//
// For each Allow statement, and each Resource pattern in it, the principal must
// evaluate Allow for:
//   - every concrete catalog action the statement's Action patterns match (so a
//     wildcard such as "admin:*" expands across the catalog and each expanded
//     action must be held), AND
//   - each literal Action token (so a bare "*" or a non-catalog identifier like
//     "gateway:invoke:*" is bounded directly, not silently skipped).
//
// Deny statements and Conditions in the candidate are deliberately ignored: a
// candidate Deny can only narrow a grant, so treating the document as if it had
// none is conservative — it may reject a grant the principal could technically
// make, but never permits one it could not. A super-admin (whose own policy
// Allows every action on the universal NRN) covers any document and always
// passes, with no special-casing.
//
// On the first permission the principal does NOT hold it returns covered=false
// with that (action, resource) for the caller's 403 message. A non-nil err is an
// engine/store failure that the caller MUST treat as fail-closed (deny the
// grant), never as "covered".
func (e *Engine) PrincipalCoversDocument(ctx context.Context, principalType, principalID string, doc PolicyDocument) (covered bool, missAction, missResource string, err error) {
	catalog := sharediam.AllActions()
	for _, stmt := range doc.Statement {
		if !strings.EqualFold(stmt.Effect, "Allow") {
			continue
		}

		// The set of request actions the principal must be shown to already
		// hold: every concrete catalog action this statement matches, plus
		// each literal token (covers wildcards / non-catalog identifiers).
		needSet := make(map[string]struct{}, len(catalog))
		for _, a := range catalog {
			if matchAction(stmt.Action, a) {
				needSet[a] = struct{}{}
			}
		}
		for _, lit := range stmt.Action {
			needSet[lit] = struct{}{}
		}
		need := make([]string, 0, len(needSet))
		for a := range needSet {
			need = append(need, a)
		}
		sort.Strings(need) // deterministic first-miss for the 403 message + tests

		resources := stmt.Resource
		if len(resources) == 0 {
			resources = []string{universalNRN}
		}
		for _, res := range resources {
			reqRes := res
			if reqRes == "*" {
				reqRes = universalNRN
			}
			for _, act := range need {
				r, evErr := e.Evaluate(ctx, principalType, principalID, act, reqRes, nil)
				if evErr != nil {
					return false, "", "", evErr
				}
				if r.Decision != "Allow" {
					return false, act, res, nil
				}
			}
		}
	}
	return true, "", "", nil
}

// PrincipalCoversPrincipal reports whether the caller (callerType, callerID)
// already holds — via its own effective IAM policies — every permission the
// OWNER (ownerType, ownerID) holds. It is the grant-ceiling primitive for the
// "mint a key that authenticates AS another principal" path: an admin
// API key delegates to its owner user, so issuing a key owned by a more-powerful
// principal would let a narrowly-scoped caller act as that principal. The check
// loads the owner's effective Allow statements and runs each through
// PrincipalCoversDocument against the caller, so a caller may only mint a key for
// an owner whose authority is a subset of its own. A super-admin caller (whose
// own policy Allows every action on the universal NRN) covers any owner.
//
// Semantics mirror PrincipalCoversDocument: on the first owner permission the
// caller does NOT hold it returns covered=false with that (action, resource); a
// non-nil err is an engine/store failure the caller MUST treat as fail-closed.
// An owner with no policies attached is trivially covered (covered=true) — there
// is nothing the resulting key could do beyond the caller's own authority.
func (e *Engine) PrincipalCoversPrincipal(ctx context.Context, callerType, callerID, ownerType, ownerID string) (covered bool, missAction, missResource string, err error) {
	ownerPolicies, _, err := e.loadPolicies(ctx, ownerType, ownerID)
	if err != nil {
		return false, "", "", err
	}
	for _, p := range ownerPolicies {
		c, ma, mr, cerr := e.PrincipalCoversDocument(ctx, callerType, callerID, p.Document)
		if cerr != nil {
			return false, "", "", cerr
		}
		if !c {
			return false, ma, mr, nil
		}
	}
	return true, "", "", nil
}
