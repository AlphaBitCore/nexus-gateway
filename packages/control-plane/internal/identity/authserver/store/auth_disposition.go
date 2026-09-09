package store

import "time"

// StatusActive is the only NexusUser.status value that permits authentication.
// The column's documented domain is "active" | "suspended" | "deactivated";
// anything outside "active" withholds access, so the check is written against
// the one permitted value rather than against a list of denied ones — a status
// nobody has thought of yet must fail closed.
const StatusActive = "active"

// AuthDisposition is the store's VERDICT on whether an account may
// authenticate. Auth gates are handed the verdict and never the columns it is
// derived from, which is the point: the two columns that have ever expressed
// "this account is switched off" disagreed about which one is authoritative,
// and every gate in the tree had picked the one with no writer.
//
// The zero value permits authentication, matching a row with the default
// status and no disable timestamp.
type AuthDisposition struct {
	blocked bool
	reason  string
}

// Blocked reports whether the account is refused authentication.
func (d AuthDisposition) Blocked() bool { return d.blocked }

// Reason names why the account is refused, for server-side logs and audit
// entries. It is empty when the account is permitted. Callers MUST NOT put it
// in an anonymous-facing response body: the login surfaces deliberately answer
// with one generic error so an anonymous caller cannot enumerate account state.
func (d AuthDisposition) Reason() string { return d.reason }

// NewAuthDisposition resolves the two columns onto one verdict.
//
// Both are read because neither alone is the answer. Every surface that
// disables an account writes status='suspended' — the admin PUT
// (users/userstore), offboarding (governancestore.SuspendUser and its
// cross-path twin), fleet.SuspendAgentUser, and SCIM active:false — while
// disabledAt has no writer anywhere in the tree, no trigger and no generated
// expression. A gate reading only disabledAt therefore enforces nothing, and a
// gate reading only status would miss a row an operator disabled by hand.
//
// Resolution lives in Go rather than in a SQL predicate so both legs of a
// two-backend deployment reach the same verdict from the same code.
func NewAuthDisposition(status string, disabledAt *time.Time) AuthDisposition {
	if disabledAt != nil {
		return AuthDisposition{blocked: true, reason: "disabled"}
	}
	if status != StatusActive {
		return AuthDisposition{blocked: true, reason: status}
	}
	return AuthDisposition{}
}
