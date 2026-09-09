package traffic

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// hostDomain is a minimal enabled interception domain for host-resolution tests.
func hostDomain(id, name, pattern string, mt interception.HostMatchType, priority int32, created time.Time) interception.InterceptionDomain {
	return interception.InterceptionDomain{
		Id:                id,
		Name:              name,
		HostPattern:       pattern,
		HostMatchType:     mt,
		AdapterId:         "openai-compat",
		Enabled:           true,
		Priority:          priority,
		DefaultPathAction: interception.DefaultPathActionProcess,
		OnAdapterError:    interception.FailureActionFailOpen,
		NetworkZone:       interception.NetworkZonePublic,
		Source:            "builtin",
		CreatedAt:         created,
		UpdatedAt:         created,
	}
}

func hostSnapshot(t *testing.T, domains ...interception.InterceptionDomain) *DomainSnapshot {
	t.Helper()
	reg := NewAdapterRegistry("test")
	_ = reg.Register("openai-compat", func() Adapter { return &stubAdapter{id: "openai-compat"} })
	reg.Freeze()
	return BuildDomainSnapshot(domains, nil, reg, testLogger())
}

// TestFindInstance_RegexHostFoldsCase is the bypass. An unmatched host resolves
// to Passthrough, so a regex-typed interception rule that did not fold case
// could be walked around by capitalising one letter of the Host header — the
// request then left the network uninspected. Exact, Prefix and Glob all folded;
// Regex was the outlier.
func TestFindInstance_RegexHostFoldsCase(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t, hostDomain("d1", "regex", `^api\.openai\.com$`, interception.HostMatchTypeRegex, 100, now))

	for _, host := range []string{
		"api.openai.com",
		"API.openai.com",
		"Api.OpenAI.Com",
		"API.OPENAI.COM",
	} {
		if got := snap.FindInstance(host); got == nil {
			t.Errorf("host %q escaped the regex rule — the request would be passed through uninspected", host)
		}
	}
}

// TestMatchPath_RegexStaysCaseSensitive pins that the fold did not spread.
// Paths ARE case-sensitive; folding them would silently widen every path rule.
func TestMatchPath_RegexStaysCaseSensitive(t *testing.T) {
	if !matchPath("/v1/Chat", `^/v1/Chat$`, interception.PathMatchTypeRegex) {
		t.Fatal("an exactly-matching path did not match")
	}
	if matchPath("/v1/chat", `^/v1/Chat$`, interception.PathMatchTypeRegex) {
		t.Fatal("path regex folded case; path rules must stay case-sensitive")
	}
}

// TestFindInstance_PriorityBeatsTheExactIndex is the override lever. Consulting
// the exact index BEFORE the priority-ordered scan makes an exact rule win
// regardless of what priority an admin set, so raising a broad rule above a
// narrow one — the entire purpose of the priority field — does nothing.
func TestFindInstance_PriorityBeatsTheExactIndex(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t,
		hostDomain("exact", "narrow", "api.openai.com", interception.HostMatchTypeExact, 10, now),
		hostDomain("glob", "broad", "*.openai.com", interception.HostMatchTypeGlob, 500, now),
	)

	got := snap.FindInstance("api.openai.com")
	if got == nil {
		t.Fatal("no instance matched")
	}
	if got.Domain.ID != "glob" {
		t.Fatalf("resolved to %q; the higher-priority glob must win — otherwise the priority field is decorative", got.Domain.ID)
	}
}

// TestFindInstance_IndexKeyIsCaseInsensitive — the index was keyed on the stored
// pattern's own capitalisation while the scan folds case, so the same policy
// resolved to different rules depending on whether the request's casing happened
// to match the config's.
//
// TWO RULES, DELIBERATELY. With a single rule this test proves nothing: the old
// raw-keyed index simply missed and the scan's EqualFold answered correctly for
// every casing. The defect is only observable in combination with the
// index-before-scan one — the mixed-case host hit the stale index and got the
// exact rule, while the lowercase host missed it and got the higher-priority
// glob. Both must now resolve the same way.
func TestFindInstance_IndexKeyIsCaseInsensitive(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t,
		hostDomain("exact", "narrow", "API.OpenAI.com", interception.HostMatchTypeExact, 10, now),
		hostDomain("glob", "broad", "*.openai.com", interception.HostMatchTypeGlob, 500, now),
	)

	for _, host := range []string{"API.OpenAI.com", "api.openai.com", "API.OPENAI.COM"} {
		got := snap.FindInstance(host)
		if got == nil {
			t.Fatalf("host %q did not resolve", host)
		}
		if got.Domain.ID != "glob" {
			t.Fatalf("host %q resolved to %q; every casing must resolve to the same rule (the higher-priority glob)", host, got.Domain.ID)
		}
	}
}

// TestFindInstance_NonASCIIHostAgreesWithTheScan is the folding-relation trap.
//
// The memo key is built with strings.ToLower while every matcher uses simple
// case folding, and outside ASCII those disagree: ToLower("İ") (U+0130) is "i",
// so a host of "İ.test" would hit the entry memoised for "i.test" and be
// answered with a rule that does not describe it — and, with a lower-priority
// exact rule in the map, answered with a WEAKER action than the scan gives.
// The memo is therefore consulted only for ASCII hosts.
func TestFindInstance_NonASCIIHostAgreesWithTheScan(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t,
		hostDomain("exact-lo", "narrow", "i.test", interception.HostMatchTypeExact, 10, now),
		hostDomain("regex-hi", "broad", `^İ\.test$`, interception.HostMatchTypeRegex, 900, now),
	)

	const host = "İ.test"
	indexed := snap.FindInstance(host)
	scanned := scanForHost(snap, host)
	if indexed != scanned {
		gotID, wantID := "<nil>", "<nil>"
		if indexed != nil {
			gotID = indexed.Domain.ID
		}
		if scanned != nil {
			wantID = scanned.Domain.ID
		}
		t.Fatalf("host %q: index says %q, scan says %q — ToLower and case folding are different relations outside ASCII",
			host, gotID, wantID)
	}
}

// TestBuildHostIndex_SkipsNonASCIIPatterns pins the other half of the same
// decision: a non-ASCII exact pattern is not memoised at all, so it cannot
// become a key a differently-folded host collides with.
func TestBuildHostIndex_SkipsNonASCIIPatterns(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t, hostDomain("d1", "unicode", "İ.test", interception.HostMatchTypeExact, 100, now))

	if len(snap.ByHost) != 0 {
		t.Fatalf("a non-ASCII pattern was memoised: %v", snap.ByHost)
	}
	// It must still RESOLVE — the scan answers, just not the memo.
	if got := snap.FindInstance("İ.test"); got == nil || got.Domain.ID != "d1" {
		t.Fatalf("the scan did not resolve the non-ASCII host: %v", got)
	}
}

// TestFindInstance_IndexAgreesWithTheScan is the invariant that makes the index
// safe to keep at all: for every host it answers, it must answer what the
// priority-ordered walk answers. An index that is a second definition of "which
// domain owns this host" is what produced two of the three defects here.
func TestFindInstance_IndexAgreesWithTheScan(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t,
		hostDomain("exact-low", "a", "api.openai.com", interception.HostMatchTypeExact, 1, now),
		hostDomain("glob-high", "b", "*.openai.com", interception.HostMatchTypeGlob, 900, now),
		hostDomain("exact-high", "c", "chat.openai.com", interception.HostMatchTypeExact, 950, now),
		hostDomain("regex", "d", `^.*\.anthropic\.com$`, interception.HostMatchTypeRegex, 500, now),
	)

	hosts := []string{
		"api.openai.com", "API.OPENAI.COM", "chat.openai.com", "CHAT.OpenAI.com",
		"other.openai.com", "api.anthropic.com", "API.Anthropic.COM", "example.test",
	}
	for _, h := range hosts {
		indexed := snap.FindInstance(h)
		scanned := scanForHost(snap, h)
		if indexed != scanned {
			gotID, wantID := "<nil>", "<nil>"
			if indexed != nil {
				gotID = indexed.Domain.ID
			}
			if scanned != nil {
				wantID = scanned.Domain.ID
			}
			t.Errorf("host %q: index says %q, scan says %q — the two must not disagree", h, gotID, wantID)
		}
	}
}

// TestBuildHostIndex_MemoisesTheScanNotTheRule proves the index stores the
// scan's winner rather than the exact rule its key came from.
func TestBuildHostIndex_MemoisesTheScanNotTheRule(t *testing.T) {
	now := time.Now()
	snap := hostSnapshot(t,
		hostDomain("exact", "narrow", "api.openai.com", interception.HostMatchTypeExact, 10, now),
		hostDomain("glob", "broad", "*.openai.com", interception.HostMatchTypeGlob, 500, now),
	)

	inst, ok := snap.ByHost["api.openai.com"]
	if !ok {
		t.Fatal("the exact rule's host is not indexed")
	}
	if inst.Domain.ID != "glob" {
		t.Fatalf("index holds %q; it must hold the scan's winner, not the exact rule", inst.Domain.ID)
	}
}
