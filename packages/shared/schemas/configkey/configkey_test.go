package configkey

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"reflect"
	"testing"
)

// TestConstants_NonEmpty confirms every exported constant has a non-empty
// value — guards against a typo that would silently demote a constant
// to "" and orphan every consumer that uses it.
func TestConstants_NonEmpty(t *testing.T) {
	cases := map[string]string{
		"LogLevel":              LogLevel,
		"Killswitch":            Killswitch,
		"AIGuard":               AIGuard,
		"Cache":                 Cache,
		"GatewayPassthrough":    GatewayPassthrough,
		"AgentSettings":         AgentSettings,
		"DiagMode":              DiagMode,
		"Onboarding":            Onboarding,
		"PayloadCapture":        PayloadCapture,
		"Observability":         Observability,
		"Providers":             Providers,
		"Models":                Models,
		"Credentials":           Credentials,
		"RoutingRules":          RoutingRules,
		"VirtualKeys":           VirtualKeys,
		"QuotaPolicies":         QuotaPolicies,
		"QuotaOverrides":        QuotaOverrides,
		"Organizations":         Organizations,
		"InterceptionDomains":   InterceptionDomains,
		"Hooks":                 Hooks,
		"Exemptions":            Exemptions,
		"StreamingCompliance":   StreamingCompliance,
		"CredentialReliability": CredentialReliability,
		"SIEM":                  SIEM,
		"InstalledRulePacks":    InstalledRulePacks,
		"UserContext":           UserContext,
		// Dual-tier response-cache keys.
		"ResponseCacheTimeSensitivePatterns": ResponseCacheTimeSensitivePatterns,
		"SemanticCacheConfig":                SemanticCacheConfig,
	}
	for name, val := range cases {
		if val == "" {
			t.Errorf("%s constant is empty", name)
		}
	}
}

// TestValidByThingType_HasFiveThingTypes pins the closed set of Thing
// types so an accidental deletion of one shows up here, not as silent
// "orphan everything for type X" warnings in prod.
func TestValidByThingType_HasFiveThingTypes(t *testing.T) {
	want := map[string]bool{
		"nexus-hub":        true,
		"control-plane":    true,
		"ai-gateway":       true,
		"compliance-proxy": true,
		"agent":            true,
	}
	if len(ValidByThingType) != len(want) {
		t.Errorf("ValidByThingType has %d entries, want %d", len(ValidByThingType), len(want))
	}
	for k := range ValidByThingType {
		if !want[k] {
			t.Errorf("unexpected thing-type %q in ValidByThingType", k)
		}
	}
	for k := range want {
		if _, ok := ValidByThingType[k]; !ok {
			t.Errorf("missing thing-type %q in ValidByThingType", k)
		}
	}
}

// TestValidByThingType_KeysNonEmpty checks every slice has entries
// (an empty list would silently mark every row as orphan).
func TestValidByThingType_KeysNonEmpty(t *testing.T) {
	for tt, keys := range ValidByThingType {
		if len(keys) == 0 {
			t.Errorf("ValidByThingType[%q] is empty", tt)
		}
		for _, k := range keys {
			if k == "" {
				t.Errorf("ValidByThingType[%q] contains empty string", tt)
			}
		}
	}
}

// TestTypedRegistry_OnlyTypeAKeys checks that every key in TypedRegistry
// is a known Type A constant (Type B keys must not be registered).
func TestTypedRegistry_OnlyTypeAKeys(t *testing.T) {
	typeA := map[string]bool{
		LogLevel: true, Killswitch: true, AIGuard: true, Cache: true,
		GatewayPassthrough: true, AgentSettings: true, DiagMode: true,
		Onboarding: true, PayloadCapture: true,
		Observability: true,
		// Type A keys for the dual-tier response-cache feature.
		ResponseCacheTimeSensitivePatterns: true,
		SemanticCacheConfig:                true,
		// Extract (L1) cache fleet config.
		ResponseCacheExtractConfig: true,
	}
	for k, rt := range TypedRegistry {
		if !typeA[k] {
			t.Errorf("TypedRegistry has non-Type-A key %q", k)
		}
		if rt == nil {
			t.Errorf("TypedRegistry[%q] has nil type", k)
		}
	}
	// Every Type A key currently lives in the registry — until later
	// PRs prune entries, the registry mirrors the Type A set.
	for k := range typeA {
		if _, ok := TypedRegistry[k]; !ok {
			t.Errorf("TypedRegistry missing Type A key %q", k)
		}
	}
}

// TestTypedRegistry_PlaceholdersAreRawMessage pins the documented
// invariant that today every entry is a json.RawMessage placeholder.
// When later PRs swap a concrete struct in, this test should be
// updated to allow that specific key to escape the check.
func TestTypedRegistry_PlaceholdersAreRawMessage(t *testing.T) {
	rawType := reflect.TypeOf(json.RawMessage(nil))
	for k, rt := range TypedRegistry {
		if rt != rawType {
			t.Errorf("TypedRegistry[%q] is %v, want json.RawMessage", k, rt)
		}
	}
}

// AuditTemplateRows fakes

// fakeRows implements Rows from a fixed [][]string of (type, key).
type fakeRows struct {
	rows   [][2]string
	pos    int
	closed bool
}

func (f *fakeRows) Next() bool {
	if f.pos >= len(f.rows) {
		return false
	}
	f.pos++
	return true
}

func (f *fakeRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("fakeRows.Scan: want 2 dests")
	}
	row := f.rows[f.pos-1]
	tp, ok1 := dest[0].(*string)
	kp, ok2 := dest[1].(*string)
	if !ok1 || !ok2 {
		return errors.New("fakeRows.Scan: dest must be *string")
	}
	*tp = row[0]
	*kp = row[1]
	return nil
}

func (f *fakeRows) Close() { f.closed = true }

// fakeDB returns the configured rows / error from Query.
type fakeDB struct {
	rows   *fakeRows
	err    error
	called bool
}

func (f *fakeDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestAuditTemplateRows_ReturnsOrphans(t *testing.T) {
	db := &fakeDB{
		rows: &fakeRows{rows: [][2]string{
			// Valid rows — should NOT appear in the orphan output.
			{"ai-gateway", Cache},
			{"agent", AgentSettings},
			{"compliance-proxy", Killswitch},
			// Orphan rows — should appear.
			{"ai-gateway", "gateway_settings"}, // confirmed orphan per migration plan
			{"agent", "agent-desktop-target"},  // bogus key
			{"unknown-thing", LogLevel},        // unknown type
		}},
	}
	got, err := AuditTemplateRows(context.Background(), db)
	if err != nil {
		t.Fatalf("AuditTemplateRows error: %v", err)
	}
	want := []OrphanRow{
		{"ai-gateway", "gateway_settings"},
		{"agent", "agent-desktop-target"},
		{"unknown-thing", LogLevel},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %#v, want %#v", got, want)
	}
	if !db.rows.closed {
		t.Errorf("rows.Close() not called (defer missing)")
	}
}

func TestAuditTemplateRows_AllValidReturnsNil(t *testing.T) {
	db := &fakeDB{
		rows: &fakeRows{rows: [][2]string{
			{"ai-gateway", Cache},
			{"agent", AgentSettings},
		}},
	}
	got, err := AuditTemplateRows(context.Background(), db)
	if err != nil {
		t.Fatalf("AuditTemplateRows error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("orphans = %#v, want empty", got)
	}
}

func TestAuditTemplateRows_QueryError(t *testing.T) {
	db := &fakeDB{err: errors.New("connection refused")}
	got, err := AuditTemplateRows(context.Background(), db)
	if err == nil {
		t.Fatal("AuditTemplateRows: expected error, got nil")
	}
	if got != nil {
		t.Errorf("on error, orphans should be nil, got %#v", got)
	}
}

// scanFailRows triggers a Scan failure on the second row.
type scanFailRows struct {
	pos    int
	closed bool
}

func (s *scanFailRows) Next() bool {
	if s.pos >= 2 {
		return false
	}
	s.pos++
	return true
}

func (s *scanFailRows) Scan(dest ...any) error {
	if s.pos == 2 {
		return errors.New("scan boom")
	}
	*(dest[0].(*string)) = "ai-gateway"
	*(dest[1].(*string)) = Cache
	return nil
}

func (s *scanFailRows) Close() { s.closed = true }

type scanFailDB struct{ rows *scanFailRows }

func (s *scanFailDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return s.rows, nil
}

func TestAuditTemplateRows_ScanError(t *testing.T) {
	r := &scanFailRows{}
	db := &scanFailDB{rows: r}
	_, err := AuditTemplateRows(context.Background(), db)
	if err == nil {
		t.Fatal("expected scan error")
	}
	if !r.closed {
		t.Errorf("rows.Close() not called on Scan failure")
	}
}

// TestIsValid_UnknownType ensures an unknown thing-type always reports
// invalid (even for a key that IS valid for some other type).
func TestIsValid_UnknownType(t *testing.T) {
	if isValid("not-a-thing", LogLevel) {
		t.Errorf("isValid(unknown type) = true")
	}
	if isValid("agent", "not-a-key") {
		t.Errorf("isValid(unknown key) = true")
	}
	if !isValid("agent", AgentSettings) {
		t.Errorf("isValid(agent, AgentSettings) = false; canonical pair rejected")
	}
}
