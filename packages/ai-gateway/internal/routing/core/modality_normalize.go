package core

import "strings"

// NormalizeModalities lower-cases a catalog row's modality list once, where
// routing loads it, so every downstream comparison is exact. Doing it per
// candidate per request instead measured ~14% of the capability filter's cost.
//
// Not in store.Model: /v1/models publishes these values verbatim, so changing
// their case there changes a shipped response.
//
// Returns the input untouched unless a row actually carries mixed case.
func NormalizeModalities(in []string) []string {
	for i, s := range in {
		lower := strings.ToLower(s)
		if lower == s {
			continue
		}
		out := make([]string, len(in))
		copy(out, in[:i])
		for j := i; j < len(in); j++ {
			out[j] = strings.ToLower(in[j])
		}
		return out
	}
	return in
}
