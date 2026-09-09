package iam

// Two vocabularies name the same principal, and they are not going away.
//
// The session layer reports "admin_user" for a dashboard login; IAM storage
// records that same principal as "nexus_user". The session spelling is a
// shipped contract — GET /api/admin/me returns it as `authPrincipalType` and
// the console branches on it in several places — so it cannot simply be
// renamed away.
//
// What CAN be fixed is where the translation happens. Until this file, every
// READ path translated by hand (middleware/iamauth.go, handler/me.go,
// handler/iam_grant_ceiling.go each carried their own `if pt == "admin_user"`)
// and no WRITE path translated at all: the attachment handler took
// `principalType` straight off the URL and the group-membership handler took it
// straight off the JSON body. LoadPolicies matches with `WHERE "principalType"
// = $1`, an exact comparison, so an attachment written as "admin_user" was live
// in the table, reported by both read paths — they read by the same parameter —
// and invisible to every evaluation. The console showed the grant; the request
// was still refused.
//
// The fossil that proves it already bit someone: nexus-hub's smart_group.go
// queries `WHERE igm."principalType" IN ('nexus_user', 'admin_user')`. Somebody
// papered over the same divergence in a different query rather than at its
// source.
//
// So: one translation point, applied on the way IN, and storage only ever holds
// the canonical spelling.

// CanonicalPrincipalTypes is the set IAM storage and the evaluator use. Adding
// a principal type means adding it here and to the mapping below, together.
var CanonicalPrincipalTypes = map[string]struct{}{
	"nexus_user": {},
	"api_key":    {},
}

// principalTypeAliases maps a wire/session spelling to its canonical storage
// spelling. A type that is already canonical is not an alias and is accepted
// as-is by NormalisePrincipalType.
var principalTypeAliases = map[string]string{
	"admin_user": "nexus_user",
}

// NormalisePrincipalType converts a principal type from any accepted spelling
// to the one IAM storage uses. ok=false for a value nothing recognises — and
// callers on a WRITE path must refuse rather than store it, because a row under
// an unrecognised type can never be loaded back: LoadPolicies compares the
// column for equality, so the row is real, listable, and permanently inert.
func NormalisePrincipalType(pt string) (canonical string, ok bool) {
	if alias, isAlias := principalTypeAliases[pt]; isAlias {
		return alias, true
	}
	if _, isCanonical := CanonicalPrincipalTypes[pt]; isCanonical {
		return pt, true
	}
	return "", false
}

// AcceptedPrincipalTypes returns every spelling NormalisePrincipalType accepts,
// so an error message can say what the value should have been rather than only
// that it was wrong. Order is stable for tests and for log output.
func AcceptedPrincipalTypes() []string {
	return []string{"api_key", "admin_user", "nexus_user"}
}
