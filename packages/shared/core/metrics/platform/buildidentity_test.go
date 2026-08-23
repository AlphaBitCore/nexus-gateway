package platform

import (
	"runtime/debug"
	"strings"
	"testing"
)

func bi(settings map[string]string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		out := &debug.BuildInfo{}
		for k, v := range settings {
			out.Settings = append(out.Settings, debug.BuildSetting{Key: k, Value: v})
		}
		return out, true
	}
}

func noBuildInfo() (*debug.BuildInfo, bool) { return nil, false }

// The whole point of the field is to answer "which build is this environment
// running". Every case below is a way that question got the wrong answer.
func TestResolveBuildIdentity(t *testing.T) {
	tests := []struct {
		name                           string
		service, buildVersion          string
		read                           func() (*debug.BuildInfo, bool)
		wantVersion, wantSHA, wantTime string
	}{
		{
			// The deploy recipe already stamps tag@sha into buildVersion, so the
			// sha is recoverable without changing the build script at all — and
			// without a second stamped variable holding the same fact twice.
			name:    "sha recovered from the tag@sha the release build already stamps",
			service: "nexus-hub", buildVersion: "prod-20260819b@caa2934c3",
			read:        noBuildInfo,
			wantVersion: "nexus-hub/prod-20260819b@caa2934c3", wantSHA: "caa2934c3", wantTime: "",
		},
		{
			// A release build comes from a `git archive` tarball with no .git,
			// so it has no VCS stamp and reports no build time. The version tag
			// carries the release date.
			name:    "a stamped version outranks the VCS stamp for the version string",
			service: "ai-gateway", buildVersion: "prod-20260819b@caa2934c3",
			read:        bi(map[string]string{"vcs.revision": "deadbeefdeadbeef", "vcs.time": "2020-01-01T00:00:00Z"}),
			wantVersion: "ai-gateway/prod-20260819b@caa2934c3", wantSHA: "caa2934c3",
			wantTime: "2020-01-01T00:00:00Z",
		},
		{
			name:        "VCS stamp fills both when nothing was stamped",
			service:     "control-plane",
			read:        bi(map[string]string{"vcs.revision": "0123456789abcdef0123", "vcs.time": "2026-08-01T09:30:00Z"}),
			wantVersion: "control-plane/0123456", wantSHA: "0123456789abcdef0123", wantTime: "2026-08-01T09:30:00Z",
		},
		{
			// A binary built from a dirty tree must not claim to be the commit.
			// That claim is worse than an empty field: it points a reader at
			// source that is not what is running.
			name:    "a dirty tree is marked, never reported as the clean commit",
			service: "compliance-proxy",
			read: bi(map[string]string{"vcs.revision": "abc1234def", "vcs.modified": "true",
				"vcs.time": "2026-08-01T09:30:00Z"}),
			wantVersion: "compliance-proxy/abc1234+dirty", wantSHA: "abc1234def+dirty",
			wantTime: "2026-08-01T09:30:00Z",
		},
		{
			name:    "the placeholder default is not mistaken for a real version",
			service: "nexus-agent", buildVersion: "dev",
			read:        noBuildInfo,
			wantVersion: "nexus-agent/dev", wantSHA: "", wantTime: "",
		},
		{
			// dev + a usable VCS stamp: the stamp is the better answer.
			name:    "the placeholder yields to a real VCS stamp",
			service: "nexus-agent", buildVersion: "dev",
			read:        bi(map[string]string{"vcs.revision": "feedface0000", "vcs.time": "2026-07-04T00:00:00Z"}),
			wantVersion: "nexus-agent/feedfac", wantSHA: "feedface0000", wantTime: "2026-07-04T00:00:00Z",
		},
		{
			name:        "nothing anywhere still produces a usable service version",
			service:     "nexus-hub",
			read:        noBuildInfo,
			wantVersion: "nexus-hub/dev", wantSHA: "", wantTime: "",
		},
		{
			// A version with no `@` is a tag, not a tag@sha — do not invent one.
			name:    "a version carrying no sha yields no sha rather than a guess",
			service: "nexus-hub", buildVersion: "v1.2.3",
			read:        noBuildInfo,
			wantVersion: "nexus-hub/v1.2.3", wantSHA: "", wantTime: "",
		},
		{
			// A trailing `@` has nothing after it, so the recovered sha is
			// empty — which is the honest answer, not a defect to guard around.
			name:    "a version ending in @ yields no sha",
			service: "nexus-hub", buildVersion: "v1.2.3@",
			read:        noBuildInfo,
			wantVersion: "nexus-hub/v1.2.3@", wantSHA: "", wantTime: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, sha, ts := resolveBuildIdentity(tt.service, tt.buildVersion, tt.read)
			if v != tt.wantVersion {
				t.Errorf("serviceVersion = %q, want %q", v, tt.wantVersion)
			}
			if sha != tt.wantSHA {
				t.Errorf("buildSHA = %q, want %q", sha, tt.wantSHA)
			}
			if ts != tt.wantTime {
				t.Errorf("buildTime = %q, want %q", ts, tt.wantTime)
			}
		})
	}
}

// The exported wrapper must reach the same logic; a service calling it with
// stamped values must get them back rather than whatever the test binary's own
// VCS state happens to be.
func TestResolveBuildIdentityExported(t *testing.T) {
	v, sha, _ := ResolveBuildIdentity("ai-gateway", "prod-1@abcdef0")
	if v != "ai-gateway/prod-1@abcdef0" || sha != "abcdef0" {
		t.Fatalf("got version=%q sha=%q", v, sha)
	}
}

// CaptureStaticInfo must carry the resolved identity through to the payload the
// Hub stores — the field being empty on every node is the defect this fixes.
func TestCaptureStaticInfoCarriesBuildIdentity(t *testing.T) {
	info := CaptureStaticInfo(BuildInfo{Service: "nexus-hub", BuildVersion: "prod-20260819b@caa2934c3"})
	if info.ServiceVersion != "nexus-hub/prod-20260819b@caa2934c3" {
		t.Errorf("ServiceVersion = %q", info.ServiceVersion)
	}
	if info.BuildSHA != "caa2934c3" {
		t.Errorf("BuildSHA = %q, want the sha recovered from the tag", info.BuildSHA)
	}
}

// A caller that supplies nothing must still get a usable identity rather than
// the three empty strings every service reported before this change.
//
// Asserted on the CONTENT, not on non-emptiness. `!= ""` could not fail:
// resolveBuildIdentity's default branch returns service + "/dev", so the string
// is non-empty by construction whatever the resolver does — this passed against
// a resolver that ignored its arguments entirely. What actually has to hold is
// that the version names THIS service and admits it has no build, so a reader
// is not left guessing which of the five nodes they are looking at.
func TestCaptureStaticInfoNeverReportsAnEmptyServiceVersion(t *testing.T) {
	info := captureStaticInfoWith(BuildInfo{Service: "ai-gateway"}, noBuildInfo)
	if info.ServiceVersion != "ai-gateway/dev" {
		t.Errorf("ServiceVersion = %q, want \"ai-gateway/dev\" — with nothing stamped and no "+
			"VCS info the identity must still name the service and say the build is unknown",
			info.ServiceVersion)
	}
	if info.BuildSHA != "" {
		t.Errorf("BuildSHA = %q, want empty — there is no sha to report, and inventing one "+
			"is worse than an empty field", info.BuildSHA)
	}
}

// A sha is a sha, or it is not reported as one.
//
// `shaFromTaggedVersion` used to return whatever followed the last `@`, which
// made two shipped build recipes produce a buildSha that is not a sha, and in
// both cases the bad value SUPPRESSED the real VCS revision that was sitting
// right there:
//
//   - nexus-ami/scripts/build-binaries.sh stamps
//     `ami@$(cat NEXUS_VERSION || echo unknown)+vs`. With the file present that
//     is `<sha>+vs`, whose `+vs` is syntactically indistinguishable from the
//     `+dirty` marker this package appends. With the file MISSING it is the
//     literal `unknown+vs`.
//   - scripts/release/build-tarball.sh defaults VERSION to `dev`, so an
//     un-versioned release build stamps `dev@<sha>` — fine — but any recipe
//     stamping a non-sha after `@` silently wins over the VCS stamp.
//
// The rule: the part before any `+marker` must be 7-40 hex. Markers are kept,
// because `+vs` records that the binary linked vectorscan rather than falling
// back to RE2 — the exact property a deploy is verified on.
func TestShaFromTaggedVersion_onlyAcceptsSomethingShaShaped(t *testing.T) {
	tests := []struct {
		name, version, want string
	}{
		{"the deploy recipe's own shape", "prod-20260819b@caa2934c3", "caa2934c3"},
		{"a full 40-char sha", "v1@" + strings.Repeat("a", 40), strings.Repeat("a", 40)},
		{"the AMI's vectorscan marker is kept", "ami@" + strings.Repeat("b", 40) + "+vs",
			strings.Repeat("b", 40) + "+vs"},
		{"the AMI with NEXUS_VERSION missing reports no sha", "ami@unknown+vs", ""},
		{"a word is not a sha", "dev@local", ""},
		{"too short to be a sha", "v1@abc", ""},
		{"too long to be a sha", "v1@" + strings.Repeat("a", 41), ""},
		{"uppercase is not how git writes them", "v1@CAFEBABE", ""},
		{"no @ at all", "prod-20260819b", ""},
		{"a trailing @ carries nothing", "prod-20260819b@", ""},
		{"a marker with no sha in front", "v1@+vs", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shaFromTaggedVersion(tt.version); got != tt.want {
				t.Errorf("shaFromTaggedVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

// A non-sha after `@` must not cost us the revision we actually know.
//
// This is the half that makes the validation worth doing: `ami@unknown+vs`
// returned "unknown+vs" AND stopped the VCS branch from ever running, so a
// build whose commit was recorded in the binary reported a word instead.
func TestResolveBuildIdentity_aBadTagFallsThroughToTheVCSStamp(t *testing.T) {
	rev := strings.Repeat("c", 40)
	_, sha, _ := resolveBuildIdentity("ai-gateway", "ami@unknown+vs",
		bi(map[string]string{"vcs.revision": rev}))
	if sha != rev {
		t.Errorf("BuildSHA = %q, want the VCS revision %q — the tag carried no sha, and "+
			"reporting a word instead of the commit we had is the defect", sha, rev)
	}
}

// `+dirty` has to apply on the path that actually produces a sha in prod.
//
// It was only consulted in the VCS branch, and prod never takes that branch:
// build-tarball.sh stamps `${VERSION}@$(git rev-parse HEAD)` with no
// cleanliness check and builds from the live repo, so a tree with uncommitted
// changes reported a clean sha. That sends a reader to source that is not what
// is running — the exact outcome the doc comment calls worse than an empty
// field.
func TestResolveBuildIdentity_dirtyMarksTheTaggedShaToo(t *testing.T) {
	rev := strings.Repeat("d", 40)
	_, sha, _ := resolveBuildIdentity("ai-gateway", "prod-20260819b@"+rev,
		bi(map[string]string{"vcs.revision": rev, "vcs.modified": "true"}))
	if sha != rev+"+dirty" {
		t.Errorf("BuildSHA = %q, want %q+dirty — the tree had uncommitted changes and the "+
			"tagged path never asked", sha, rev)
	}
}

// A clean tagged build must NOT be marked dirty. The counter-case, because a
// marker that is always present says nothing.
func TestResolveBuildIdentity_cleanTaggedShaIsNotMarked(t *testing.T) {
	rev := strings.Repeat("e", 40)
	_, sha, _ := resolveBuildIdentity("ai-gateway", "prod-1@"+rev,
		bi(map[string]string{"vcs.revision": rev, "vcs.modified": "false"}))
	if sha != rev {
		t.Errorf("BuildSHA = %q, want a bare %q", sha, rev)
	}
}

// The marker is not doubled when a recipe already stamped one, and a dirty
// vectorscan build says both things.
func TestResolveBuildIdentity_markersCompose(t *testing.T) {
	rev := strings.Repeat("f", 40)
	_, sha, _ := resolveBuildIdentity("ai-gateway", "ami@"+rev+"+vs",
		bi(map[string]string{"vcs.revision": rev, "vcs.modified": "true"}))
	if sha != rev+"+vs+dirty" {
		t.Errorf("BuildSHA = %q, want %q+vs+dirty — both facts are true and both matter: "+
			"+vs says the binary linked vectorscan rather than falling back to RE2", sha, rev)
	}
}

// buildTime has to REACH the payload, not merely be resolved.
//
// resolveBuildIdentity returned it correctly and nothing asserted that
// CaptureStaticInfo passed it on — the same shape of gap that left buildSha
// empty on every node while the resolver looked fine.
func TestCaptureStaticInfoCarriesBuildTime(t *testing.T) {
	info := captureStaticInfoWith(BuildInfo{Service: "nexus-hub", BuildVersion: "dev"},
		bi(map[string]string{
			"vcs.revision": strings.Repeat("a", 40),
			"vcs.time":     "2026-08-19T04:05:06Z",
		}))
	if info.BuildTime != "2026-08-19T04:05:06Z" {
		t.Errorf("BuildTime = %q, want the VCS stamp — it is resolved and then dropped "+
			"between the resolver and the payload", info.BuildTime)
	}
}
