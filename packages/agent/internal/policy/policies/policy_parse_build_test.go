package policies

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

func TestSnapshotCache_NewIsEmpty(t *testing.T) {
	c := NewSnapshotCache()
	if c == nil {
		t.Fatal("NewSnapshotCache returned nil")
	}
	if got := c.Get("any"); got != nil {
		t.Errorf("fresh cache should return nil for unknown key, got %q", string(got))
	}
}

func TestSnapshotCache_SetThenGetReturnsCopy(t *testing.T) {
	c := NewSnapshotCache()
	payload := json.RawMessage(`{"hookConfigs":[]}`)
	c.Set("hooks", payload)

	got := c.Get("hooks")
	if string(got) != `{"hookConfigs":[]}` {
		t.Errorf("Get returned %q, want hookConfigs payload", string(got))
	}
	// Mutating returned slice must NOT affect cache (defensive copy).
	got[0] = 'X'
	got2 := c.Get("hooks")
	if got2[0] == 'X' {
		t.Errorf("Get must return a defensive copy; second Get sees mutation")
	}
}

func TestSnapshotCache_SetCopiesInputSoCallerMutationDoesNotLeak(t *testing.T) {
	c := NewSnapshotCache()
	payload := json.RawMessage(`{"a":1}`)
	c.Set("k", payload)
	// Mutate caller's slice — cache must keep its own copy.
	payload[0] = 'X'
	got := c.Get("k")
	if got[0] == 'X' {
		t.Errorf("Set must copy input; caller mutation leaked into cache: %q", string(got))
	}
}

func TestSnapshotCache_SetEmptyPayloadClears(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("k", json.RawMessage(`{"x":1}`))
	if got := c.Get("k"); len(got) == 0 {
		t.Fatalf("precondition: key should be populated")
	}
	// Clear via zero-length payload.
	c.Set("k", json.RawMessage{})
	if got := c.Get("k"); got != nil {
		t.Errorf("after Set(empty), Get should return nil, got %q", string(got))
	}
	// Also nil payload clears.
	c.Set("k2", json.RawMessage(`{"y":2}`))
	c.Set("k2", nil)
	if got := c.Get("k2"); got != nil {
		t.Errorf("after Set(nil), Get should return nil, got %q", string(got))
	}
}

func TestSnapshotCache_SetOverwrites(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("k", json.RawMessage(`{"v":1}`))
	c.Set("k", json.RawMessage(`{"v":2}`))
	if got := c.Get("k"); string(got) != `{"v":2}` {
		t.Errorf("overwrite: got %q, want {\"v\":2}", string(got))
	}
}

func TestSnapshotCache_ConcurrentSetGetRaceSafe(t *testing.T) {
	c := NewSnapshotCache()
	const writers, readers = 8, 8
	const iters = 200
	var wg sync.WaitGroup

	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := []byte(`{"v":` + string(rune('0'+id)) + `}`)
			for range iters {
				c.Set("k", payload)
			}
		}(w)
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				_ = c.Get("k")
			}
		}()
	}
	wg.Wait()
	// Final state must be one of the writers' payloads.
	got := c.Get("k")
	if len(got) == 0 {
		t.Errorf("expected populated key after concurrent writes, got empty")
	}
}

// stubApplier records the raw payload it was asked to apply and returns
// the pre-canned error (nil = success).
type stubApplier struct {
	mu      sync.Mutex
	gotRaw  json.RawMessage
	callCnt int32
	err     error
}

func (s *stubApplier) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	atomic.AddInt32(&s.callCnt, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotRaw = append(s.gotRaw[:0], raw...)
	return s.err
}

func TestTeeApplier_DelegatesAndCachesOnSuccess(t *testing.T) {
	inner := &stubApplier{}
	cache := NewSnapshotCache()
	tee := TeeApplier{Inner: inner, Cache: cache, CfgKey: "hooks"}

	payload := json.RawMessage(`{"hookConfigs":[{"id":"x"}]}`)
	if err := tee.ApplyShadowState(context.Background(), payload); err != nil {
		t.Fatalf("ApplyShadowState err = %v, want nil", err)
	}
	if atomic.LoadInt32(&inner.callCnt) != 1 {
		t.Errorf("inner should have been called once, got %d", inner.callCnt)
	}
	if got := cache.Get("hooks"); string(got) != string(payload) {
		t.Errorf("cache after success: got %q, want %q", string(got), string(payload))
	}
}

func TestTeeApplier_PropagatesErrorAndDoesNotCache(t *testing.T) {
	inner := &stubApplier{err: errors.New("apply failed")}
	cache := NewSnapshotCache()
	// Pre-populate cache with a prior good payload to confirm it's NOT
	// overwritten by the failing apply.
	cache.Set("hooks", json.RawMessage(`{"hookConfigs":[{"id":"prior"}]}`))

	tee := TeeApplier{Inner: inner, Cache: cache, CfgKey: "hooks"}
	broken := json.RawMessage(`{"hookConfigs":"not-an-array"}`)
	err := tee.ApplyShadowState(context.Background(), broken)
	if err == nil || err.Error() != "apply failed" {
		t.Fatalf("err = %v, want \"apply failed\"", err)
	}
	if got := cache.Get("hooks"); string(got) != `{"hookConfigs":[{"id":"prior"}]}` {
		t.Errorf("cache must retain prior payload on failure, got %q", string(got))
	}
}

func TestTeeApplier_NilCacheToleratedNoPanic(t *testing.T) {
	inner := &stubApplier{}
	tee := TeeApplier{Inner: inner, Cache: nil, CfgKey: "k"}
	if err := tee.ApplyShadowState(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("err = %v, want nil (nil cache must be tolerated)", err)
	}
	if atomic.LoadInt32(&inner.callCnt) != 1 {
		t.Errorf("inner should still be called once with nil cache")
	}
}

// applied.go — gap branches not exercised by applied_test.go

// helper to construct an accessor seeded with one raw payload under key.
func accessorWithRaw(key string, raw []byte) *fakeAccessor {
	return &fakeAccessor{snap: map[string]thingclient.ConfigState{
		key: {State: raw},
	}}
}

func TestBuild_CachePreferredOverSnapshot(t *testing.T) {
	// snap carries the legacy/empty version; cache carries the canonical one.
	// pick() prefers cache when populated.
	snap := map[string]thingclient.ConfigState{
		"hooks": {State: []byte(`{"hookConfigs":[{"id":"from-snap","name":"snap","enabled":true}]}`)},
	}
	acc := &fakeAccessor{snap: snap}
	cache := NewSnapshotCache()
	cache.Set("hooks", json.RawMessage(`{"hookConfigs":[{"id":"from-cache","name":"cache","enabled":true}]}`))

	got := Build(acc, cache).Hooks
	if len(got) != 1 || got[0].ID != "from-cache" {
		t.Errorf("cache should win over snap; got %+v", got)
	}
}

func TestBuild_FallsThroughToSnapshotWhenCacheEmpty(t *testing.T) {
	snap := map[string]thingclient.ConfigState{
		"interception_domains": {State: []byte(`{"interceptionDomains":[{"id":"d1","name":"D1","hostPattern":"*.x","enabled":true}]}`)},
	}
	acc := &fakeAccessor{snap: snap}
	cache := NewSnapshotCache() // empty cache, no Set
	got := Build(acc, cache).InterceptionDomains
	if len(got) != 1 || got[0].ID != "d1" {
		t.Errorf("empty cache should fall through to snap; got %+v", got)
	}
}

// --- parseInterceptionDomains gap branches ---

func TestParseInterceptionDomains_MalformedJSONReturnsEmpty(t *testing.T) {
	acc := accessorWithRaw("interception_domains", []byte(`{not-json`))
	got := Build(acc, nil).InterceptionDomains
	if len(got) != 0 {
		t.Errorf("malformed JSON must yield empty slice (lenient), got %+v", got)
	}
}

func TestParseInterceptionDomains_PathsCarriedThrough(t *testing.T) {
	acc := accessorWithRaw("interception_domains", []byte(`{
		"interceptionDomains":[{
			"id":"1","name":"OpenAI","hostPattern":"*.openai.com","enabled":true,"priority":0,
			"paths":[
				{"id":"p1","pathPattern":["/v1/chat"],"matchType":"prefix","action":"intercept","priority":10,"enabled":true},
				{"id":"p2","pathPattern":["/v1/audio"],"action":"passthrough","priority":0,"enabled":false}
			]
		}]
	}`))
	got := Build(acc, nil).InterceptionDomains
	if len(got) != 1 || len(got[0].Paths) != 2 {
		t.Fatalf("paths not parsed, got %+v", got)
	}
	if got[0].Paths[0].ID != "p1" || got[0].Paths[0].Action != "intercept" || got[0].Paths[0].Priority != 10 {
		t.Errorf("path 0: %+v", got[0].Paths[0])
	}
	if got[0].Paths[1].Enabled {
		t.Errorf("path 1 should be disabled")
	}
	// Priority=0 must round-trip on the parent too (no-omitempty contract).
	if got[0].Priority != 0 {
		t.Errorf("parent Priority should preserve 0, got %d", got[0].Priority)
	}
}

// --- parseHooks gap branches ---

func TestParseHooks_MalformedJSONReturnsEmpty(t *testing.T) {
	acc := accessorWithRaw("hooks", []byte(`{"hookConfigs":`))
	got := Build(acc, nil).Hooks
	if len(got) != 0 {
		t.Errorf("malformed JSON must yield empty slice, got %+v", got)
	}
}

func TestParseHooks_LegacyHooksKeyUsedWhenNewKeyEmpty(t *testing.T) {
	acc := accessorWithRaw("hooks", []byte(`{
		"hookConfigs":[],
		"hooks":[{"id":"legacy","name":"Legacy","enabled":true,"stage":"preOutbound","priority":2,"timeoutMs":250}]
	}`))
	got := Build(acc, nil).Hooks
	if len(got) != 1 || got[0].ID != "legacy" || got[0].TimeoutMs != 250 {
		t.Errorf("legacy hooks key not honoured, got %+v", got)
	}
}

func TestParseHooks_NewKeyWinsOverLegacy(t *testing.T) {
	acc := accessorWithRaw("hooks", []byte(`{
		"hookConfigs":[{"id":"new","name":"New","enabled":true}],
		"hooks":[{"id":"legacy","name":"Legacy","enabled":true}]
	}`))
	got := Build(acc, nil).Hooks
	if len(got) != 1 || got[0].ID != "new" {
		t.Errorf("hookConfigs should win over legacy hooks; got %+v", got)
	}
}

// --- parseExemptions gap branches ---

func TestParseExemptions_MalformedJSONReturnsEmpty(t *testing.T) {
	acc := accessorWithRaw("exemptions", []byte(`{"admin_exemptions":`))
	got := Build(acc, nil).Exemptions
	if len(got) != 0 {
		t.Errorf("malformed JSON must yield empty slice, got %+v", got)
	}
}

func TestParseExemptions_AdminExemptionsBareHostShape(t *testing.T) {
	acc := accessorWithRaw("exemptions", []byte(`{
		"admin_exemptions":["host-a.com","host-b.com"],
		"denylist":["bad.example"]
	}`))
	got := Build(acc, nil).Exemptions
	if len(got) != 2 {
		t.Fatalf("expected 2 admin exemptions (denylist ignored), got %d: %+v", len(got), got)
	}
	if got[0].ID != "admin:host-a.com" || got[0].Host != "host-a.com" || got[0].Reason != "admin grant" {
		t.Errorf("row 0 = %+v", got[0])
	}
}

func TestParseExemptions_EntriesKey(t *testing.T) {
	acc := accessorWithRaw("exemptions", []byte(`{
		"entries":[{"id":"e1","host":"x.com","user":"u1","reason":"audit"}]
	}`))
	got := Build(acc, nil).Exemptions
	if len(got) != 1 || got[0].ID != "e1" || got[0].User != "u1" {
		t.Errorf("entries key not honoured, got %+v", got)
	}
}

func TestParseExemptions_EmptyPayloadShortCircuits(t *testing.T) {
	// Empty raw input → empty slice (the if len==0 returns).
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{}}
	got := Build(acc, nil).Exemptions
	if got == nil || len(got) != 0 {
		t.Errorf("empty payload should return empty (not nil) slice, got %#v", got)
	}
}

// --- parseKillSwitch gap ---

func TestParseKillSwitch_MalformedJSONReturnsDisengagedDefault(t *testing.T) {
	// Malformed payload defaults to Engaged=false (fail-open) so the
	// agent doesn't synthesize a "killswitch engaged" state out of
	// thin air when the shadow JSON is corrupt — normal interception
	// continues.
	acc := accessorWithRaw("killswitch", []byte(`{"engaged":`))
	got := Build(acc, nil).KillSwitch
	if got.Engaged || got.Reason != "" {
		t.Errorf("malformed kill-switch should default to Engaged=false (fail-open), got %+v", got)
	}
}

// --- parseDeviceDefaults gap ---

func TestParseDeviceDefaults_MalformedJSONReturnsZero(t *testing.T) {
	acc := accessorWithRaw("agent_settings", []byte(`{"quitAllowed":`))
	got := Build(acc, nil).DeviceDefaults
	if got.QuitAllowed != nil || got.HeartbeatIntervalSec != 0 {
		t.Errorf("malformed agent_settings should default to zero, got %+v", got)
	}
}

func TestParseDeviceDefaults_FullPayloadIncludingThemeAndQUICBundles(t *testing.T) {
	acc := accessorWithRaw("agent_settings", []byte(`{
		"trafficUploadLevel":"processed",
		"themeId":"corporate-dark",
		"forceQUICFallbackBundles":["com.apple.Safari","com.google.Chrome"],
		"logLevel":"debug"
	}`))
	got := Build(acc, nil).DeviceDefaults
	if got.TrafficUploadLevel != "processed" {
		t.Errorf("TrafficUploadLevel = %q", got.TrafficUploadLevel)
	}
	if got.ThemeID != "corporate-dark" {
		t.Errorf("ThemeID = %q", got.ThemeID)
	}
	if len(got.ForceQUICFallbackBundles) != 2 || got.ForceQUICFallbackBundles[0] != "com.apple.Safari" {
		t.Errorf("ForceQUICFallbackBundles = %+v", got.ForceQUICFallbackBundles)
	}
	if got.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", got.LogLevel)
	}
}

// --- parseRulePacks: 42.9% baseline — full coverage ---

func TestParseRulePacks_EmptyPayloadReturnsEmptySlice(t *testing.T) {
	// No installed_rule_packs key in snap.
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{}}
	got := Build(acc, nil).RulePacks
	if got == nil || len(got) != 0 {
		t.Errorf("empty rule-packs payload must be empty slice (not nil), got %#v", got)
	}
}

func TestParseRulePacks_MalformedJSONReturnsEmpty(t *testing.T) {
	acc := accessorWithRaw("installed_rule_packs", []byte(`{"installedRulePacks":`))
	got := Build(acc, nil).RulePacks
	if len(got) != 0 {
		t.Errorf("malformed JSON must yield empty slice, got %+v", got)
	}
}

func TestParseRulePacks_FullPayloadWithNestedRules(t *testing.T) {
	acc := accessorWithRaw("installed_rule_packs", []byte(`{
		"installedRulePacks":[
			{
				"id":"inst-1","packId":"pack-pii","name":"PII Pack","version":"1.0.0",
				"maintainer":"nexus","description":"Detects PII","boundHookId":"hook-pii",
				"enabled":true,"ruleCount":2,"installedAt":"2026-05-13T10:00:00Z",
				"rules":[
					{"id":"r1","ruleId":"ssn","category":"identity","severity":"high","pattern":"\\d{3}-\\d{2}-\\d{4}","flags":"i","description":"US SSN","labels":["pii","us"]},
					{"id":"r2","ruleId":"ccn","category":"financial","severity":"critical","pattern":"\\d{16}","description":"Credit card"}
				]
			},
			{"id":"inst-2","packId":"pack-secret","name":"Secrets Pack","enabled":false,"ruleCount":0}
		]
	}`))
	got := Build(acc, nil).RulePacks
	if len(got) != 2 {
		t.Fatalf("want 2 rule packs, got %d: %+v", len(got), got)
	}
	if got[0].ID != "inst-1" || got[0].PackID != "pack-pii" || !got[0].Enabled || got[0].RuleCount != 2 {
		t.Errorf("pack 0: %+v", got[0])
	}
	if len(got[0].Rules) != 2 || got[0].Rules[0].RuleID != "ssn" || got[0].Rules[0].Severity != "high" {
		t.Errorf("pack 0 rules: %+v", got[0].Rules)
	}
	if len(got[0].Rules[0].Labels) != 2 || got[0].Rules[0].Labels[0] != "pii" {
		t.Errorf("pack 0 rule 0 labels: %+v", got[0].Rules[0].Labels)
	}
	// Disabled pack still surfaces with RuleCount=0 visible.
	if got[1].Enabled || got[1].RuleCount != 0 || got[1].Name != "Secrets Pack" {
		t.Errorf("pack 1 (disabled): %+v", got[1])
	}
}

// --- parseUserContext: 33.3% baseline — full coverage ---

func TestParseUserContext_EmptyPayloadReturnsNilNil(t *testing.T) {
	user, orgs := parseUserContext(nil)
	if user != nil || orgs != nil {
		t.Errorf("nil raw should yield (nil, nil), got (%+v, %+v)", user, orgs)
	}
	user, orgs = parseUserContext(json.RawMessage{})
	if user != nil || orgs != nil {
		t.Errorf("zero-len raw should yield (nil, nil), got (%+v, %+v)", user, orgs)
	}
}

func TestParseUserContext_MalformedJSONReturnsNilNil(t *testing.T) {
	user, orgs := parseUserContext(json.RawMessage(`{"user":`))
	if user != nil || orgs != nil {
		t.Errorf("malformed JSON should yield (nil, nil), got (%+v, %+v)", user, orgs)
	}
}

func TestParseUserContext_FullPayload(t *testing.T) {
	raw := json.RawMessage(`{
		"user":{"id":"u-1","displayName":"Alice","email":"alice@corp.example","status":"ACTIVE","source":"SSO_OKTA","organizationId":"org-leaf"},
		"organizations":[
			{"id":"org-root","name":"Acme","code":"ACME","path":"/acme","timezone":"UTC"},
			{"id":"org-leaf","name":"Engineering","code":"ENG","parentId":"org-root","path":"/acme/eng","description":"Eng org","timezone":"America/New_York"}
		]
	}`)
	user, orgs := parseUserContext(raw)
	if user == nil || user.ID != "u-1" || user.DisplayName != "Alice" || user.OrganizationID != "org-leaf" {
		t.Errorf("user = %+v", user)
	}
	if len(orgs) != 2 || orgs[0].ID != "org-root" || orgs[1].ParentID != "org-root" {
		t.Errorf("orgs = %+v", orgs)
	}
	if orgs[1].Timezone != "America/New_York" || orgs[1].Description != "Eng org" {
		t.Errorf("orgs[1] details = %+v", orgs[1])
	}
}

func TestBuild_PopulatesUserContextEndToEnd(t *testing.T) {
	// Wires UserContext via Build, exercising the assignment + the cache pick.
	raw := json.RawMessage(`{"user":{"id":"u-2","displayName":"Bob","organizationId":"org-x"},"organizations":[{"id":"org-x","name":"X","code":"X","path":"/x"}]}`)
	acc := &fakeAccessor{snap: map[string]thingclient.ConfigState{
		"user_context": {State: raw},
	}}
	got := Build(acc, nil)
	if got.UserContext == nil || got.UserContext.ID != "u-2" {
		t.Errorf("UserContext: %+v", got.UserContext)
	}
	if len(got.OrganizationTree) != 1 || got.OrganizationTree[0].ID != "org-x" {
		t.Errorf("OrganizationTree: %+v", got.OrganizationTree)
	}
}

// --- parseDiagMode gap ---

func TestParseDiagMode_UntilShape(t *testing.T) {
	// The per-thing diag_mode override carries {until}.
	acc := accessorWithRaw("diag_mode", []byte(`{"until":"2026-06-01T12:00:00Z"}`))
	got := Build(acc, nil).DiagMode
	if got == nil || !got.Active || got.Until != "2026-06-01T12:00:00Z" {
		t.Errorf("until shape: got %+v", got)
	}
}

func TestParseDiagMode_EmptyUntilReturnsNil(t *testing.T) {
	// An override with an empty until is not a window — there is nothing to
	// schedule an auto-restore against, so it reads as off.
	acc := accessorWithRaw("diag_mode", []byte(`{"until":""}`))
	got := Build(acc, nil).DiagMode
	if got != nil {
		t.Errorf("empty until should yield nil, got %+v", got)
	}
}

func TestParseDiagMode_NoUntilReturnsNil(t *testing.T) {
	// Payload parses cleanly but carries no until at all.
	acc := accessorWithRaw("diag_mode", []byte(`{"unrelated":42}`))
	got := Build(acc, nil).DiagMode
	if got != nil {
		t.Errorf("no until should yield nil, got %+v", got)
	}
}

func TestParseDiagMode_MalformedJSONReturnsNil(t *testing.T) {
	acc := accessorWithRaw("diag_mode", []byte(`{"until":`))
	got := Build(acc, nil).DiagMode
	if got != nil {
		t.Errorf("malformed JSON should yield nil, got %+v", got)
	}
}
