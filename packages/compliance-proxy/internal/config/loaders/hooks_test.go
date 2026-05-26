package loaders

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// buildHookConfig is the only piece of the loader that contains real
// branching logic (NULL handling, JSON parsing, ingress default). Testing
// it directly via HookConfigRow inputs gives full coverage of the
// interesting cases without needing a live Postgres or a SQL mock library.
//
// The rows.Scan + query path is deliberately not unit-tested here — it is
// thin boilerplate over database/sql and is exercised end-to-end whenever
// the compliance-proxy starts against a real database.

func TestBuildHookConfig_FullyPopulatedRow(t *testing.T) {
	row := HookConfigRow{
		ID:                "h1",
		Name:              "keyword-filter-prod",
		Type:              "builtin",
		ImplementationID:  "keyword-filter",
		Stage:             "request",
		Category:          sql.NullString{String: "content-safety", Valid: true},
		Endpoint:          sql.NullString{},
		Script:            sql.NullString{},
		Config:            sql.NullString{String: `{"keywords":["foo","bar"],"action":"deny"}`, Valid: true},
		Priority:          10,
		TimeoutMs:         3000,
		FailBehavior:      "fail-closed",
		Enabled:           true,
		ApplicableIngress: stringArray{"ALL"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("buildHookConfig returned unexpected error: %v", err)
	}
	if hc.ID != "h1" || hc.Name != "keyword-filter-prod" || hc.ImplementationID != "keyword-filter" {
		t.Errorf("basic fields not copied correctly: %#v", hc)
	}
	if hc.Priority != 10 || hc.TimeoutMs != 3000 {
		t.Errorf("numeric fields wrong: priority=%d timeoutMs=%d", hc.Priority, hc.TimeoutMs)
	}
	if hc.Stage != "request" || hc.FailBehavior != "fail-closed" || !hc.Enabled {
		t.Errorf("enum/bool fields wrong: stage=%s failBehavior=%s enabled=%v", hc.Stage, hc.FailBehavior, hc.Enabled)
	}
	if !reflect.DeepEqual(hc.ApplicableIngress, []string{"ALL"}) {
		t.Errorf("ApplicableIngress default wrong: got %v, want [ALL]", hc.ApplicableIngress)
	}
	if got, ok := hc.Config["action"].(string); !ok || got != "deny" {
		t.Errorf("Config jsonb not parsed: %#v", hc.Config)
	}
}

func TestBuildHookConfig_NullConfigBecomesEmptyMap(t *testing.T) {
	row := HookConfigRow{
		ID:                "h2",
		Name:              "pii-detector",
		Type:              "builtin",
		ImplementationID:  "pii-detector",
		Stage:             "request",
		FailBehavior:      "fail-open",
		Enabled:           true,
		Config:            sql.NullString{Valid: false}, // NULL
		ApplicableIngress: stringArray{"ALL"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("NULL config should not error: %v", err)
	}
	if hc.Config == nil {
		t.Errorf("Config should be an empty map, not nil — hook factories rely on non-nil map")
	}
	if len(hc.Config) != 0 {
		t.Errorf("Config should be empty, got %d entries", len(hc.Config))
	}
}

func TestBuildHookConfig_EmptyStringConfigBecomesEmptyMap(t *testing.T) {
	// Postgres jsonb can also round-trip through sql.NullString{Valid:true, String:""}
	// in edge cases — treat that the same as NULL so we do not explode.
	row := HookConfigRow{
		ID:                "h3",
		Name:              "empty-cfg",
		Type:              "builtin",
		ImplementationID:  "noop",
		Stage:             "request",
		FailBehavior:      "fail-open",
		Enabled:           true,
		Config:            sql.NullString{String: "", Valid: true},
		ApplicableIngress: stringArray{"ALL"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("empty-string config should not error: %v", err)
	}
	if hc.Config == nil || len(hc.Config) != 0 {
		t.Errorf("empty-string config should yield empty map, got %#v", hc.Config)
	}
}

func TestBuildHookConfig_JsonNullBecomesEmptyMap(t *testing.T) {
	// JSON literal "null" deserializes to a nil Go map — our normalization
	// must replace it with an empty map so callers do not panic on writes.
	row := HookConfigRow{
		ID:                "h4",
		Name:              "json-null",
		Type:              "builtin",
		ImplementationID:  "noop",
		Stage:             "request",
		FailBehavior:      "fail-open",
		Enabled:           true,
		Config:            sql.NullString{String: "null", Valid: true},
		ApplicableIngress: stringArray{"ALL"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("JSON null should not error: %v", err)
	}
	if hc.Config == nil {
		t.Errorf("JSON null should be normalized to non-nil empty map")
	}
	if len(hc.Config) != 0 {
		t.Errorf("JSON null should be normalized to empty map, got %d entries", len(hc.Config))
	}
}

func TestBuildHookConfig_MalformedJsonIsHardError(t *testing.T) {
	row := HookConfigRow{
		ID:                "h5",
		Name:              "corrupt",
		Type:              "builtin",
		ImplementationID:  "noop",
		Stage:             "request",
		FailBehavior:      "fail-open",
		Enabled:           true,
		Config:            sql.NullString{String: `{"broken":`, Valid: true},
		ApplicableIngress: stringArray{"ALL"},
	}

	if _, err := buildHookConfig(row); err == nil {
		t.Errorf("malformed jsonb must return error — silently replacing with empty would ship a hook running in an unexpected configuration")
	}
}

func TestBuildHookConfig_NullOptionalColumnsArePreservedAsEmpty(t *testing.T) {
	row := HookConfigRow{
		ID:                "h6",
		Name:              "minimal",
		Type:              "builtin",
		ImplementationID:  "noop",
		Stage:             "response",
		Priority:          0,
		TimeoutMs:         5000,
		FailBehavior:      "fail-open",
		Enabled:           true,
		Category:          sql.NullString{Valid: false},
		Endpoint:          sql.NullString{Valid: false},
		Script:            sql.NullString{Valid: false},
		Config:            sql.NullString{Valid: false},
		ApplicableIngress: stringArray{"ALL"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hc.Stage != "response" {
		t.Errorf("Stage not preserved: %s", hc.Stage)
	}
	if hc.Priority != 0 {
		t.Errorf("zero priority should be preserved, got %d", hc.Priority)
	}
}

func TestBuildHookConfig_ApplicableIngressFromDB(t *testing.T) {
	row := HookConfigRow{
		ID:                "h7",
		Name:              "per-ingress",
		Type:              "builtin",
		ImplementationID:  "noop",
		Stage:             "request",
		FailBehavior:      "fail-open",
		Enabled:           true,
		Config:            sql.NullString{Valid: false},
		ApplicableIngress: stringArray{"COMPLIANCE_PROXY", "AGENT"},
	}

	hc, err := buildHookConfig(row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(hc.ApplicableIngress, []string{"COMPLIANCE_PROXY", "AGENT"}) {
		t.Errorf("ApplicableIngress not threaded from DB: got %v", hc.ApplicableIngress)
	}
}

// stringArray.Scan tests — the custom Postgres text[] decoder. Used by
// every HookConfig row scan, so its branches must hold up against
// whatever pgx hands us.

func TestStringArray_Scan_NilSource(t *testing.T) {
	var a stringArray
	if err := a.Scan(nil); err != nil {
		t.Fatalf("nil src must scan cleanly: %v", err)
	}
	if a != nil {
		t.Errorf("nil src must map to nil slice, got %#v", a)
	}
}

func TestStringArray_Scan_EmptyArrayLiteral(t *testing.T) {
	var a stringArray
	if err := a.Scan("{}"); err != nil {
		t.Fatalf("empty array must scan: %v", err)
	}
	if len(a) != 0 {
		t.Errorf("empty array must yield zero-length slice, got %v", a)
	}
	// Important: the explicit empty []string{} (not nil) matches the
	// shared.hooks contract — buildHookConfig forwards it as
	// ApplicableIngress and shared.hooks defaults nil to ["ALL"]. Both
	// branches are valid; we only assert len == 0 here.
}

func TestStringArray_Scan_EmptyString(t *testing.T) {
	var a stringArray
	if err := a.Scan(""); err != nil {
		t.Fatalf("empty string must scan: %v", err)
	}
	if len(a) != 0 {
		t.Errorf("empty string must yield zero-length slice, got %v", a)
	}
}

func TestStringArray_Scan_SingleValue(t *testing.T) {
	var a stringArray
	if err := a.Scan("{ALL}"); err != nil {
		t.Fatalf("single value: %v", err)
	}
	if !reflect.DeepEqual([]string(a), []string{"ALL"}) {
		t.Errorf("single value: got %v", a)
	}
}

func TestStringArray_Scan_MultiValueFromString(t *testing.T) {
	var a stringArray
	if err := a.Scan("{COMPLIANCE_PROXY,AGENT,AI_GATEWAY}"); err != nil {
		t.Fatalf("multi value: %v", err)
	}
	if !reflect.DeepEqual([]string(a), []string{"COMPLIANCE_PROXY", "AGENT", "AI_GATEWAY"}) {
		t.Errorf("multi value: got %v", a)
	}
}

func TestStringArray_Scan_MultiValueFromBytes(t *testing.T) {
	// pgx sometimes hands the column as []byte rather than string.
	var a stringArray
	if err := a.Scan([]byte("{ALL,COMPLIANCE_PROXY}")); err != nil {
		t.Fatalf("bytes path: %v", err)
	}
	if !reflect.DeepEqual([]string(a), []string{"ALL", "COMPLIANCE_PROXY"}) {
		t.Errorf("bytes path: got %v", a)
	}
}

func TestStringArray_Scan_UnsupportedTypeErrors(t *testing.T) {
	var a stringArray
	err := a.Scan(int64(42))
	if err == nil {
		t.Fatal("unsupported type must error to surface a driver mismatch")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("err must call out the unsupported type; got: %v", err)
	}
}

func TestStringArray_Scan_UnexpectedFormatErrors(t *testing.T) {
	// Missing braces — defensive against a driver that ever returns the
	// "stringified" csv without the curly braces. Returning an error
	// rather than silently parsing prevents a downstream NPE.
	var a stringArray
	err := a.Scan("ALL,COMPLIANCE_PROXY")
	if err == nil {
		t.Fatal("missing braces must surface a format error")
	}
	if !strings.Contains(err.Error(), "unexpected format") {
		t.Errorf("err must call out the format mismatch; got: %v", err)
	}
}

// buildHookConfigsFromRows tests — the slice-level driver for the row
// scan loop. Covers (a) happy multi-row, (b) empty input → nil out, (c)
// malformed JSON aborts the whole load with attribution.

func TestBuildHookConfigsFromRows_EmptyInputYieldsNil(t *testing.T) {
	got, err := buildHookConfigsFromRows(nil)
	if err != nil {
		t.Fatalf("empty input must NOT error: %v", err)
	}
	if got != nil {
		t.Errorf("empty input must yield nil result; got %v", got)
	}
}

func TestBuildHookConfigsFromRows_PreservesOrderAndDecodes(t *testing.T) {
	rows := []HookConfigRow{
		{
			ID: "first", Name: "first", Type: "builtin",
			ImplementationID: "noop", Stage: "request",
			FailBehavior: "fail-open", Enabled: true,
			Priority:          5,
			TimeoutMs:         1000,
			Config:            sql.NullString{Valid: false},
			ApplicableIngress: stringArray{"ALL"},
		},
		{
			ID: "second", Name: "second", Type: "builtin",
			ImplementationID: "noop", Stage: "response",
			FailBehavior:      "fail-closed",
			Enabled:           true,
			Priority:          10,
			TimeoutMs:         2000,
			Config:            sql.NullString{String: `{"k":"v"}`, Valid: true},
			ApplicableIngress: stringArray{"COMPLIANCE_PROXY"},
		},
	}
	got, err := buildHookConfigsFromRows(rows)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Errorf("order not preserved: %v %v", got[0].ID, got[1].ID)
	}
	if got[1].Config["k"] != "v" {
		t.Errorf("second row Config not decoded: %#v", got[1].Config)
	}
}

func TestBuildHookConfigsFromRows_MalformedJSONAbortsWithAttribution(t *testing.T) {
	// Aborting on first malformed row (rather than skip + continue) is
	// the documented contract — running the proxy with a silently
	// truncated hook set is worse than failing the reload and keeping
	// the previously cached value.
	rows := []HookConfigRow{
		{
			ID: "ok", Name: "ok", Type: "builtin",
			ImplementationID: "noop", Stage: "request",
			FailBehavior: "fail-open", Enabled: true,
			Config:            sql.NullString{Valid: false},
			ApplicableIngress: stringArray{"ALL"},
		},
		{
			ID: "corrupt-id", Name: "corrupt", Type: "builtin",
			ImplementationID: "noop", Stage: "request",
			FailBehavior: "fail-open", Enabled: true,
			Config:            sql.NullString{String: `{"broken":`, Valid: true},
			ApplicableIngress: stringArray{"ALL"},
		},
	}
	got, err := buildHookConfigsFromRows(rows)
	if err == nil {
		t.Fatal("malformed config row must abort the whole load")
	}
	if got != nil {
		t.Errorf("on error, the partial slice must NOT be returned; got %v", got)
	}
	if !strings.Contains(err.Error(), "configloader: build HookConfig") {
		t.Errorf("err must carry attribution prefix; got: %q", err.Error())
	}
	if !strings.Contains(err.Error(), `"corrupt-id"`) {
		t.Errorf("err must name the failing row's ID for operator debugging; got: %q", err.Error())
	}
}
