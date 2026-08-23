package platform

import (
	"runtime/debug"
	"strings"
)

// ResolveBuildIdentity answers "which build is this process" for the
// staticInfo payload a service reports to the Hub.
//
// It exists because the answer used to be nothing: `staticInfo.buildSha` and
// `buildTime` were empty on every node, so no environment could be tied to a
// build and "is this already fixed?" had to be settled by replaying traffic
// instead of by reading a version.
//
// The only input is the `-X main.buildVersion=` stamp, because that is the only
// build-identity input the release recipe actually produces. Precedence:
//
//  1. The sha embedded in a `tag@sha` build version. The deploy recipe already
//     stamps `-X main.buildVersion=prod-20260819b@caa2934c3`, so the sha is
//     recoverable from what is being built today — no build-script change and
//     no second stamped variable holding the same fact twice.
//  2. The VCS stamp Go embeds when the binary is built inside a git checkout.
//     Absent from release builds (they build from a `git archive` tarball with
//     no .git) but present for every local and CI build, which is where the
//     field is otherwise least likely to be filled in.
//
// buildTime therefore comes only from the VCS stamp and is empty for a release
// build; the release's own date is carried by the version tag. A tree with
// uncommitted changes is marked `+dirty` rather than reported as the commit it
// was branched from — an unqualified sha for a build that is not that sha is
// worse than an empty field, because it sends a reader to source that is not
// what is running.
func ResolveBuildIdentity(service, buildVersion string) (version, sha, buildTime string) {
	return resolveBuildIdentity(service, buildVersion, debug.ReadBuildInfo)
}

// placeholderVersion is the compile-time default every service's buildVersion
// carries when nothing was stamped. It is a marker, not a version, so a real
// VCS stamp outranks it.
const placeholderVersion = "dev"

func resolveBuildIdentity(
	service, buildVersion string,
	read func() (*debug.BuildInfo, bool),
) (version, sha, buildTime string) {
	vcsRev, vcsTime, dirty := readVCS(read)

	sha = shaFromTaggedVersion(buildVersion)
	if sha == "" && vcsRev != "" {
		sha = vcsRev
	}
	// Marked on BOTH paths, because the tagged one is the only path that
	// produces a sha in prod. scripts/release/build-tarball.sh stamps
	// `${VERSION}@$(git rev-parse HEAD)` with no cleanliness check and builds
	// from the live repo, so a tree with uncommitted changes used to report a
	// clean sha — sending a reader to source that is not what is running,
	// which is the outcome this doc calls worse than an empty field.
	if sha != "" && dirty && !strings.HasSuffix(sha, "+dirty") {
		sha += "+dirty"
	}
	buildTime = vcsTime

	switch {
	case buildVersion != "" && buildVersion != placeholderVersion:
		version = service + "/" + buildVersion
	case vcsRev != "":
		version = service + "/" + shortSHA(vcsRev, dirty)
	default:
		version = service + "/" + placeholderVersion
	}
	return version, sha, buildTime
}

func readVCS(read func() (*debug.BuildInfo, bool)) (rev, ts string, dirty bool) {
	info, ok := read()
	if !ok || info == nil {
		return "", "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			ts = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return rev, ts, dirty
}

// shaFromTaggedVersion pulls the sha out of a `tag@sha` version string, and
// returns "" when what follows the `@` is not shaped like one.
//
// The validation is the point. Returning the substring unchecked meant two
// shipped recipes reported a buildSha that is not a sha — and worse, the bad
// value SUPPRESSED the VCS revision the binary already carried:
//
//   - nexus-ami/scripts/build-binaries.sh stamps
//     `ami@$(cat NEXUS_VERSION || echo unknown)+vs`, so a missing file yields
//     the literal `unknown+vs`.
//   - any recipe stamping a word after `@` wins over the real stamp silently.
//
// A `+marker` suffix is KEPT rather than rejected: `+vs` records that the
// binary linked vectorscan instead of falling back to RE2, which is precisely
// what a deploy verifies, and dropping it would trade one lost fact for
// another.
func shaFromTaggedVersion(v string) string {
	i := strings.LastIndex(v, "@")
	if i < 0 {
		return ""
	}
	candidate := v[i+1:]
	core := candidate
	if j := strings.Index(core, "+"); j >= 0 {
		core = core[:j]
	}
	if !isSHAShaped(core) {
		return ""
	}
	return candidate
}

// isSHAShaped reports whether s is a git object name: 7 to 40 lowercase hex
// digits. 7 is git's own abbreviation floor; uppercase is rejected because git
// does not write them that way, so an uppercase run is some other identifier
// that happened to land after an `@`.
func isSHAShaped(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func shortSHA(rev string, dirty bool) string {
	s := rev
	if len(s) > 7 {
		s = s[:7]
	}
	if dirty {
		s += "+dirty"
	}
	return s
}
