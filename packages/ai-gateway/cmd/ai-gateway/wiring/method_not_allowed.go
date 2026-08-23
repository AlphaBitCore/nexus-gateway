package wiring

import (
	"net/http"
	"slices"
)

// probeMethods are the methods a caller could plausibly reach a mounted route
// with. HEAD is included because ServeMux serves it from a GET registration.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// servedUnderOtherMethods reports which methods DO have a mounted route for
// this request's path, by re-asking the mux with each method in turn.
//
// It answers the question ServeMux answers itself when nothing matched, and
// which a registered catch-all takes away: the catch-all matches, so the 405
// branch never runs. Reading the answer back out of the mux rather than from a
// hand-kept table means a route added later is covered without anyone
// remembering to update this.
func servedUnderOtherMethods(mux *http.ServeMux, r *http.Request) []string {
	var out []string
	for _, m := range probeMethods {
		if m == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = m
		// A fallback pattern matching is the same as nothing matching.
		if _, pattern := mux.Handler(probe); pattern != "" && !slices.Contains(fallbackPatterns, pattern) {
			out = append(out, m)
		}
	}
	return out
}

// fallbackPatterns are the catch-alls registered by MountCoreRoutes. Matching
// one of them means the path is not served, whatever the method.
var fallbackPatterns = []string{"/", "/v1/"}
