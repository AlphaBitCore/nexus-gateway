package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// newConfigCacheWithVaryBy builds the fleet semantic config the L1 scope reader
// consults, carrying just the one knob these tests exercise.
func newConfigCacheWithVaryBy(t *testing.T, varyBy string) *semantic.ConfigCache {
	t.Helper()
	cc := semantic.NewConfigCache()
	cc.Set(semantic.ConfigSnapshot{VaryBy: varyBy})
	return cc
}

// TestCacheScope_NeverWidensToFleetWide is a security test, not a coverage one.
//
// The cache scope decides who can read whose cached answer. An operator picking
// "user" or "org" is asking for isolation STRICTER than the "vk" default. The
// bug this pins: `rec.UserID` is populated only for personal virtual keys, so
// under "user" every application-key request resolved to the empty string —
// and the empty string means no scope token, i.e. fleet-wide cross-tenant
// sharing. The setting meant to tighten isolation silently removed it, for
// exactly the traffic shape enterprises use: one application key shared by a
// business system serving many end users.
//
// The invariant: no non-"none" setting may ever resolve to fleet-wide while the
// request carries a virtual key. Only "none" — which says so — may.
func TestCacheScope_NeverWidensToFleetWide(t *testing.T) {
	applicationKey := &audit.Record{
		VirtualKeyID: "vk-app-1",
		// An application VK owns no NexusUser and, in this deployment, no org
		// was resolved either — both of the stricter dimensions are absent.
		UserID:         "",
		OrganizationID: "",
	}

	for _, varyBy := range []string{"user", "org", "vk", "", "somethingUnknown"} {
		t.Run("L2/"+varyBy, func(t *testing.T) {
			got := resolveL2VKScope(applicationKey, varyBy)
			if got == "" {
				t.Fatalf("vary_by=%q resolved to fleet-wide for an application key — "+
					"an operator choosing a stricter dimension must never get cross-tenant sharing", varyBy)
			}
			if got != applicationKey.VirtualKeyID {
				t.Errorf("vary_by=%q scope = %q, want the virtual key %q as the narrowing fallback",
					varyBy, got, applicationKey.VirtualKeyID)
			}
		})
	}

	// "none" is the one value that may widen, because it is the operator saying
	// so out loud.
	if got := resolveL2VKScope(applicationKey, "none"); got != "" {
		t.Errorf(`vary_by="none" scope = %q, want fleet-wide — "none" is an explicit opt-out`, got)
	}
}

// TestCacheScope_PrefersTheChosenDimensionWhenPresent proves the fallback did
// not flatten the feature: a personal key still isolates per user, and an org
// still isolates per org. Without this, "always return the VK" would pass the
// test above while quietly deleting the setting.
func TestCacheScope_PrefersTheChosenDimensionWhenPresent(t *testing.T) {
	personalKey := &audit.Record{
		VirtualKeyID:   "vk-personal-1",
		UserID:         "user-7",
		OrganizationID: "org-3",
	}

	if got := resolveL2VKScope(personalKey, "user"); got != "user-7" {
		t.Errorf(`vary_by="user" scope = %q, want the user id — the fallback must not shadow a present value`, got)
	}
	if got := resolveL2VKScope(personalKey, "org"); got != "org-3" {
		t.Errorf(`vary_by="org" scope = %q, want the org id`, got)
	}
	if got := resolveL2VKScope(personalKey, "vk"); got != "vk-personal-1" {
		t.Errorf(`vary_by="vk" scope = %q, want the virtual key id`, got)
	}
}

// TestL1CacheScope_TypePrefixedAndNeverWidening mirrors the L2 invariant on the
// L1 exact-match tier, which folds its own token into the cache key. The type
// prefix matters as much as the value: without it a virtual-key id and a user
// id that happen to share a string would collide into one scope.
func TestL1CacheScope_TypePrefixedAndNeverWidening(t *testing.T) {
	applicationKey := &audit.Record{VirtualKeyID: "vk-app-1"}

	for _, varyBy := range []string{"user", "org", "vk"} {
		cc := newConfigCacheWithVaryBy(t, varyBy)
		got := resolveL1CacheScope(cc, applicationKey)
		if got == "" {
			t.Fatalf("vary_by=%q resolved L1 to fleet-wide for an application key", varyBy)
		}
		if got != "vk:vk-app-1" {
			t.Errorf("vary_by=%q L1 scope = %q, want the type-prefixed virtual-key fallback", varyBy, got)
		}
	}

	personalKey := &audit.Record{VirtualKeyID: "vk-p", UserID: "user-7"}
	if got := resolveL1CacheScope(newConfigCacheWithVaryBy(t, "user"), personalKey); got != "user:user-7" {
		t.Errorf(`vary_by="user" L1 scope = %q, want "user:user-7"`, got)
	}

	if got := resolveL1CacheScope(newConfigCacheWithVaryBy(t, "none"), applicationKey); got != "" {
		t.Errorf(`vary_by="none" L1 scope = %q, want fleet-wide`, got)
	}
}
