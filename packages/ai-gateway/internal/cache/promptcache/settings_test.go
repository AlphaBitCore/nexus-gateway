package promptcache

import (
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

func ptr(b bool) *bool { return &b }

func anthropicFamilyOn() cacheconfig.CacheConfigBlob {
	return cacheconfig.CacheConfigBlob{
		Adapters: map[string]cacheconfig.AdapterConfig{
			"anthropic": {MarkerInjectEnabled: ptr(true)},
		},
	}
}

// Before any config has arrived the answer must be "off". A gateway that
// guessed "on" here would mark bodies against an operator setting nobody has
// made yet.
func TestMarkersEnabled_BeforeAnyConfig_IsOff(t *testing.T) {
	var zero Settings
	if zero.MarkersEnabled("p1", "anthropic") {
		t.Error("zero value must answer off")
	}
	if New().MarkersEnabled("p1", "anthropic") {
		t.Error("a constructed but unconfigured Settings must answer off")
	}
	// A nil holder is how a degraded startup reaches the request path. Both
	// methods must tolerate it: the config applier calls SetConfig without
	// knowing whether wiring produced a holder.
	var nilSettings *Settings
	if nilSettings.MarkersEnabled("p1", "anthropic") {
		t.Error("nil receiver must answer off, not panic")
	}
	nilSettings.SetConfig(anthropicFamilyOn())
	if nilSettings.MarkersEnabled("p1", "anthropic") {
		t.Error("a nil holder cannot hold config, and must still answer off")
	}
}

// The operator sets the adapter-family default; every provider on that family
// inherits it. This is the shape the staging deployment actually uses — the
// admin UI writes the family row, not a row per provider.
func TestMarkersEnabled_AdapterFamilyDefaultReachesEveryProvider(t *testing.T) {
	s := New()
	s.SetConfig(anthropicFamilyOn())
	for _, id := range []string{"prov-1", "prov-2", "a-provider-nobody-has-named-yet"} {
		if !s.MarkersEnabled(id, "anthropic") {
			t.Errorf("provider %q must inherit the anthropic family default", id)
		}
	}
}

// The regression this holder exists to remove. The previous design resolved
// every provider ONCE, when the `cache` shadow key was applied, against the
// provider list as it stood at that moment. A provider created afterwards was
// absent from that map and silently never got markers, while the admin UI kept
// showing the toggle on. Resolving per request means a provider id the holder
// has never seen answers from the family default like any other.
func TestMarkersEnabled_ProviderUnknownAtConfigTimeStillResolves(t *testing.T) {
	s := New()
	s.SetConfig(anthropicFamilyOn()) // no Providers map at all
	if !s.MarkersEnabled("created-after-the-config-push", "anthropic") {
		t.Fatal("a provider created after the last cache push must still resolve its family default")
	}
}

// A per-provider override is how an operator carves one provider out of the
// family default, in either direction.
func TestMarkersEnabled_ProviderOverrideWinsOverFamily(t *testing.T) {
	s := New()
	s.SetConfig(cacheconfig.CacheConfigBlob{
		Adapters:  map[string]cacheconfig.AdapterConfig{"anthropic": {MarkerInjectEnabled: ptr(true)}},
		Providers: map[string]cacheconfig.ProviderConfig{"opted-out": {MarkerInjectEnabled: ptr(false)}},
	})
	if s.MarkersEnabled("opted-out", "anthropic") {
		t.Error("provider override false must beat family default true")
	}
	if !s.MarkersEnabled("everyone-else", "anthropic") {
		t.Error("providers without an override keep the family default")
	}

	s.SetConfig(cacheconfig.CacheConfigBlob{
		Adapters:  map[string]cacheconfig.AdapterConfig{"anthropic": {MarkerInjectEnabled: ptr(false)}},
		Providers: map[string]cacheconfig.ProviderConfig{"opted-in": {MarkerInjectEnabled: ptr(true)}},
	})
	if !s.MarkersEnabled("opted-in", "anthropic") {
		t.Error("provider override true must beat family default false")
	}
}

// Only the Anthropic family has a marker knob. An OpenAI-wire provider must
// answer off even when its id happens to appear in the blob, because the
// marker it would enable does not exist on that wire.
func TestMarkersEnabled_NonAnthropicWireHasNoMarker(t *testing.T) {
	s := New()
	s.SetConfig(cacheconfig.CacheConfigBlob{
		Adapters:  map[string]cacheconfig.AdapterConfig{"anthropic": {MarkerInjectEnabled: ptr(true)}},
		Providers: map[string]cacheconfig.ProviderConfig{"p-openai": {MarkerInjectEnabled: ptr(true)}},
	})
	for _, adapter := range []string{"openai", "gemini", "deepseek", ""} {
		if s.MarkersEnabled("p-openai", adapter) {
			t.Errorf("adapter %q has no prompt-cache marker and must answer off", adapter)
		}
	}
}

// A config push replaces the answer for in-flight and subsequent requests.
func TestSetConfig_HotSwapChangesTheAnswer(t *testing.T) {
	s := New()
	s.SetConfig(anthropicFamilyOn())
	if !s.MarkersEnabled("p1", "anthropic") {
		t.Fatal("precondition: markers on")
	}
	s.SetConfig(cacheconfig.CacheConfigBlob{
		Adapters: map[string]cacheconfig.AdapterConfig{"anthropic": {MarkerInjectEnabled: ptr(false)}},
	})
	if s.MarkersEnabled("p1", "anthropic") {
		t.Fatal("a config push turning markers off must take effect")
	}
}

// Reads happen on every request while a config push can land at any moment.
func TestSettings_ConcurrentReadsDuringConfigPush(t *testing.T) {
	s := New()
	s.SetConfig(anthropicFamilyOn())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = s.MarkersEnabled("p1", "anthropic")
			}
		}()
	}
	for range 50 {
		s.SetConfig(anthropicFamilyOn())
	}
	wg.Wait()
}

// The boundary knob resolves on the same chain as the marker knob and is
// meaningful only alongside it, so it must answer off in exactly the same
// places: before any config, on a non-Anthropic wire, and on a nil holder.
func TestBoundaryEnabled_ResolvesOnTheSameChainAsMarkers(t *testing.T) {
	var nilSettings *Settings
	if nilSettings.BoundaryEnabled("p1", "anthropic") {
		t.Error("nil receiver must answer off, not panic")
	}
	if New().BoundaryEnabled("p1", "anthropic") {
		t.Error("unconfigured must answer off")
	}

	s := New()
	s.SetConfig(cacheconfig.CacheConfigBlob{
		Adapters: map[string]cacheconfig.AdapterConfig{
			"anthropic": {MarkerInjectEnabled: ptr(true), MarkerBoundary3Enabled: ptr(true)},
		},
		Providers: map[string]cacheconfig.ProviderConfig{
			"opted-out": {MarkerBoundary3Enabled: ptr(false)},
		},
	})
	if !s.BoundaryEnabled("anyone", "anthropic") {
		t.Error("the family default must reach a provider with no override")
	}
	if s.BoundaryEnabled("opted-out", "anthropic") {
		t.Error("a provider override must beat the family default")
	}
	for _, adapter := range []string{"openai", "gemini", ""} {
		if s.BoundaryEnabled("anyone", adapter) {
			t.Errorf("adapter %q has no boundary knob and must answer off", adapter)
		}
	}
}
