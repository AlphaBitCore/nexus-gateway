//go:build vectorscan && !vsstatic

package matcher

// Dynamic linking via pkg-config — the zero-config default for local dev and
// tests (`go test -tags vectorscan`). Resolves libhs include + link flags from
// the system pkg-config (Homebrew / apt). The agent's static universal build
// uses the `vsstatic` tag instead (see vectorscan_link_static.go).

/*
#cgo pkg-config: libhs
*/
import "C"
