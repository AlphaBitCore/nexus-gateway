//go:build vectorscan && vsstatic

// Package note for the static-link build variant (no code; the cgo source is
// vectorscan.go). Selecting the `vsstatic` tag excludes
// vectorscan_link_pkgconfig.go, so no pkg-config dynamic flags are emitted; the
// build driver supplies the static archive and include path via the environment
// instead — the same model the agent already uses for go-sqlcipher:
//
//	CGO_CFLAGS="-I<vectorscan>/include/hs"
//	CGO_LDFLAGS="<vectorscan>/lib/<arch>/libhs.a -lc++"
//
// Vectorscan is C++, so the C++ runtime (-lc++) must be linked. The archive is
// pinned per GOARCH by packages/agent/platform/darwin/Scripts/build.sh, which is
// what makes the resulting universal Mach-O self-contained (no libhs.dylib
// runtime dependency) and notarizable.
package matcher
