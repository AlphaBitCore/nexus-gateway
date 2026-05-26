package pgx

import "strings"

// ilikeEscaper escapes the three characters that PostgreSQL LIKE/ILIKE treats
// as metacharacters — % (match any sequence), _ (match any single character),
// and the default escape \ — so a user-supplied substring passed through
// "%"+s+"%" is matched literally rather than as a wildcard pattern. This
// prevents attackers from enumerating data via wildcards or building oracles
// against search endpoints. PostgreSQL's default ILIKE escape character is
// backslash, so no explicit ESCAPE clause is needed alongside the escaped
// value.
var ilikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeILIKE escapes PostgreSQL ILIKE metacharacters in user-supplied input.
// Callers typically wrap the result with leading/trailing % wildcards, e.g.
// "%"+EscapeILIKE(s)+"%".
func EscapeILIKE(s string) string {
	return ilikeEscaper.Replace(s)
}
