package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// stubAppliedConfigStore is an in-memory implementation of appliedConfigStore
// used by the /things/:id/applied-config handler tests. It lets each test seed
// the Thing row, the per-(type,key) templates, and the latest audit row per
// config_key so the merge handler can run without a live Postgres.
type stubAppliedConfigStore struct {
	thing          *store.ThingRegistry
	thingErr       error
	templates      []store.ThingConfigTemplate
	templatesErr   error
	latestByKey    map[string]*store.ConfigChangeEvent
	latestErrByKey map[string]error
}

// stubAppliedConfigOverrideFetcher is an in-memory appliedConfigOverrideFetcher
// used by override-aware applied-config tests. Lets each test seed the per-
// configKey rows the handler should weave into entries, plus optionally a
// fatal error to exercise the degrade-gracefully path.
type stubAppliedConfigOverrideFetcher struct {
	overrides []appliedConfigOverrideMeta
	err       error
}

func (s *stubAppliedConfigOverrideFetcher) FetchOverridesForThing(_ context.Context, _ string) ([]appliedConfigOverrideMeta, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.overrides, nil
}

func (s *stubAppliedConfigStore) GetThing(_ context.Context, _ string) (*store.ThingRegistry, error) {
	if s.thingErr != nil {
		return nil, s.thingErr
	}
	return s.thing, nil
}

func (s *stubAppliedConfigStore) ListTemplatesByType(_ context.Context, _ string) ([]store.ThingConfigTemplate, error) {
	if s.templatesErr != nil {
		return nil, s.templatesErr
	}
	return s.templates, nil
}

func (s *stubAppliedConfigStore) GetLatestConfigChangeEvent(_ context.Context, _ string, configKey string) (*store.ConfigChangeEvent, error) {
	if err, ok := s.latestErrByKey[configKey]; ok {
		return nil, err
	}
	if ev, ok := s.latestByKey[configKey]; ok {
		return ev, nil
	}
	return nil, nil
}

// newAdminHandlerForAppliedConfig wires an AdminHandler with the narrow
// applied-config store override. Mirrors the sibling handler test setup.
func newAdminHandlerForAppliedConfig(t *testing.T) (*AdminHandler, *stubAppliedConfigStore) {
	t.Helper()
	h, _, _ := newAdminHandlerWithHubSpy(t)
	stub := &stubAppliedConfigStore{}
	h.AppliedConfigStore = stub
	return h, stub
}

// TestGetNodeAppliedConfig_MergesDesiredReportedHistory locks the happy-path
// merge: one template, one matching reported blob, and one audit row land in
// the response with inSync=true.
func TestGetNodeAppliedConfig_MergesDesiredReportedHistory(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	desiredKS := json.RawMessage(`{"engaged":false}`)
	reportedBlob := json.RawMessage(`{"killswitch":{"engaged":false}}`)
	stub.thing = &store.ThingRegistry{
		ID:          "proxy-1",
		Type:        "compliance-proxy",
		Reported:    reportedBlob,
		ReportedVer: 3,
		Desired:     json.RawMessage(`{"killswitch":{"engaged":false}}`),
		DesiredVer:  3,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: desiredKS, Version: 3},
	}
	ts, _ := time.Parse(time.RFC3339, "2026-04-20T12:34:56Z")
	stub.latestByKey = map[string]*store.ConfigChangeEvent{
		"killswitch": {
			ID:                "ev-1",
			Timestamp:         ts,
			ThingType:         "compliance-proxy",
			ConfigKey:         "killswitch",
			Action:            "disable",
			ActorID:           "user-1",
			ActorName:         "alice",
			NewState:          desiredKS,
			NewVersion:        3,
			EmergencyOverride: false,
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		NodeID         string `json:"nodeId"`
		NodeType       string `json:"nodeType"`
		TargetVersion  int64  `json:"targetVersion"`
		AppliedVersion int64  `json:"appliedVersion"`
		Configs        map[string]struct {
			TargetConfig   json.RawMessage `json:"targetConfig"`
			TargetVersion  int64           `json:"targetVersion"`
			AppliedConfig  json.RawMessage `json:"appliedConfig"`
			AppliedVersion int64           `json:"appliedVersion"`
			TemplateState  json.RawMessage `json:"templateState"`
			TemplateVer    int64           `json:"templateVer"`
			InSync         bool            `json:"inSync"`
			LastChange     *struct {
				Timestamp         string `json:"timestamp"`
				Actor             string `json:"actor"`
				Action            string `json:"action"`
				EmergencyOverride bool   `json:"emergencyOverride"`
			} `json:"lastChange,omitempty"`
			Override *appliedConfigOverrideMeta `json:"override,omitempty"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.NodeID != "proxy-1" {
		t.Errorf("thingId = %q, want proxy-1", resp.NodeID)
	}
	if resp.NodeType != "compliance-proxy" {
		t.Errorf("thingType = %q, want compliance-proxy", resp.NodeType)
	}
	if resp.TargetVersion != 3 || resp.AppliedVersion != 3 {
		t.Errorf("shadow versions = (%d,%d), want (3,3)", resp.TargetVersion, resp.AppliedVersion)
	}
	ks, ok := resp.Configs["killswitch"]
	if !ok {
		t.Fatalf("killswitch missing from configs: %+v", resp.Configs)
	}
	if ks.TargetVersion != 3 {
		t.Errorf("desiredVer = %d, want 3", ks.TargetVersion)
	}
	if ks.AppliedVersion != 3 {
		t.Errorf("reportedVer = %d, want 3", ks.AppliedVersion)
	}
	if ks.TemplateVer != 3 {
		t.Errorf("templateVer = %d, want 3", ks.TemplateVer)
	}
	if string(ks.TemplateState) != string(desiredKS) {
		t.Errorf("templateState = %s, want %s", string(ks.TemplateState), string(desiredKS))
	}
	if !ks.InSync {
		t.Errorf("inSync = false, want true (desired bytes == reported bytes)")
	}
	if ks.LastChange == nil {
		t.Fatalf("lastChange missing")
	}
	if ks.LastChange.Actor != "alice" {
		t.Errorf("lastChange.actor = %q, want alice", ks.LastChange.Actor)
	}
	if ks.LastChange.Action != "disable" {
		t.Errorf("lastChange.action = %q, want disable", ks.LastChange.Action)
	}
	if ks.LastChange.EmergencyOverride {
		t.Errorf("lastChange.emergencyOverride = true, want false")
	}
	// Override is omitted when no override exists for the key.
	if ks.Override != nil {
		t.Errorf("override = %+v, want nil (no override fetcher wired)", ks.Override)
	}
}

// TestGetNodeAppliedConfig_UnknownID_Returns404 surfaces the not-found branch
// when GetThing returns (nil, nil).
func TestGetNodeAppliedConfig_UnknownID_Returns404(t *testing.T) {
	h, _ := newAdminHandlerForAppliedConfig(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/ghost/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("ghost")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "NOT_FOUND", "not_found")
}

// TestGetNodeAppliedConfig_NoReportedYet_MarksOutOfSync exercises the path
// where the Thing has enrolled but not yet reported a given config_key. The
// response must carry the desired state + desiredVer, a nil reported payload,
// and inSync=false.
func TestGetNodeAppliedConfig_NoReportedYet_MarksOutOfSync(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	desiredKS := json.RawMessage(`{"engaged":true}`)
	stub.thing = &store.ThingRegistry{
		ID:          "proxy-1",
		Type:        "compliance-proxy",
		Reported:    json.RawMessage(`{}`),
		ReportedVer: 0,
		Desired:     json.RawMessage(`{"killswitch":{"engaged":true}}`),
		DesiredVer:  1,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: desiredKS, Version: 1},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Configs map[string]struct {
			TargetConfig   json.RawMessage `json:"targetConfig"`
			TargetVersion  int64           `json:"targetVersion"`
			AppliedConfig  json.RawMessage `json:"appliedConfig"`
			AppliedVersion int64           `json:"appliedVersion"`
			InSync         bool            `json:"inSync"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	ks, ok := resp.Configs["killswitch"]
	if !ok {
		t.Fatalf("killswitch missing")
	}
	if ks.TargetVersion != 1 {
		t.Errorf("desiredVer = %d, want 1", ks.TargetVersion)
	}
	if ks.AppliedVersion != 0 {
		t.Errorf("reportedVer = %d, want 0", ks.AppliedVersion)
	}
	if ks.InSync {
		t.Errorf("inSync = true, want false (nothing reported yet)")
	}
	if !bytesIsJSONNull(ks.AppliedConfig) {
		t.Errorf("reported = %s, want null", string(ks.AppliedConfig))
	}
}

// TestGetNodeAppliedConfig_EmptyID_Returns400 locks the validation branch so
// the route's path param cannot be silently empty.
func TestGetNodeAppliedConfig_EmptyID_Returns400(t *testing.T) {
	h, _ := newAdminHandlerForAppliedConfig(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes//applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "VALIDATION_ERROR", "validation_error")
}

// TestGetNodeAppliedConfig_HistoryErrorIsSkipped confirms a store error on a
// single config_key's lastChange lookup is logged and skipped rather than
// aborting the whole response.
func TestGetNodeAppliedConfig_HistoryErrorIsSkipped(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	desiredKS := json.RawMessage(`{"engaged":false}`)
	stub.thing = &store.ThingRegistry{
		ID:          "proxy-1",
		Type:        "compliance-proxy",
		Reported:    json.RawMessage(`{"killswitch":{"engaged":false}}`),
		ReportedVer: 1,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: desiredKS, Version: 1},
	}
	stub.latestErrByKey = map[string]error{
		"killswitch": errors.New("boom"),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// lastChange is omitted when history lookup fails; the rest of the entry
	// still lands in the response.
	var resp struct {
		Configs map[string]map[string]json.RawMessage `json:"configs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, has := resp.Configs["killswitch"]["lastChange"]; has {
		t.Errorf("lastChange should be omitted on history error, got: %s", string(resp.Configs["killswitch"]["lastChange"]))
	}
	if _, has := resp.Configs["killswitch"]["targetConfig"]; !has {
		t.Errorf("desired must still be present even when history lookup fails")
	}
}

// TestGetNodeAppliedConfig_UsesShadowDesiredVersion ensures the endpoint uses
// Thing shadow desired/version semantics instead of the latest template
// version, so it stays consistent with /nodes/:id/config-sync.
func TestGetNodeAppliedConfig_UsesShadowDesiredVersion(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"providers":{"items":["a"]}}`),
		DesiredVer:  3,
		Reported:    json.RawMessage(`{"providers":{"items":["a"]}}`),
		ReportedVer: 3,
	}
	// Template version is ahead, but Applied Config should still render shadow desiredVer.
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "providers", State: json.RawMessage(`{"items":["a","b"]}`), Version: 10},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Configs map[string]struct {
			TargetConfig   json.RawMessage `json:"targetConfig"`
			TargetVersion  int64           `json:"targetVersion"`
			AppliedVersion int64           `json:"appliedVersion"`
			InSync         bool            `json:"inSync"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	providers, ok := resp.Configs["providers"]
	if !ok {
		t.Fatalf("providers missing")
	}
	if providers.TargetVersion != 3 {
		t.Errorf("desiredVer = %d, want 3 (shadow desired_ver)", providers.TargetVersion)
	}
	if providers.AppliedVersion != 3 {
		t.Errorf("reportedVer = %d, want 3", providers.AppliedVersion)
	}
	if !providers.InSync {
		t.Errorf("inSync = false, want true (shadow desired == reported)")
	}
}

// TestGetNodeAppliedConfig_BothEmptyTreatAsInSync locks the B semantics:
// when both target and applied render as empty (dash in UI), treat the row as in sync.
func TestGetNodeAppliedConfig_BothEmptyTreatAsInSync(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"credentials":null}`),
		DesiredVer:  3,
		Reported:    json.RawMessage(`{}`), // credentials key absent => not reported yet
		ReportedVer: 3,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "credentials", State: json.RawMessage(`null`), Version: 10},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Configs map[string]struct {
			InSync bool `json:"inSync"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	credentials, ok := resp.Configs["credentials"]
	if !ok {
		t.Fatalf("credentials missing")
	}
	if !credentials.InSync {
		t.Errorf("inSync = false, want true when desired is null and reported is absent")
	}
}

// bytesIsJSONNull reports whether b is the JSON null literal, allowing a few
// whitespace variants that encoding/json may emit.
func bytesIsJSONNull(b []byte) bool {
	switch string(b) {
	case "null", "":
		return true
	}
	return false
}

// decodeAppliedConfigResponse is the test-side wire shape for the
// extended response. Inlined here (rather than in the production package) so
// the tests stay self-contained — production code returns a map[string]any
// to avoid leaking JSON tag drift across packages.
type decodeAppliedConfigResponse struct {
	NodeID         string `json:"nodeId"`
	NodeType       string `json:"nodeType"`
	TargetVersion  int64  `json:"targetVersion"`
	AppliedVersion int64  `json:"appliedVersion"`
	Configs        map[string]struct {
		TargetConfig    json.RawMessage            `json:"targetConfig"`
		TargetVersion   int64                      `json:"targetVersion"`
		AppliedConfig   json.RawMessage            `json:"appliedConfig"`
		AppliedVersion  int64                      `json:"appliedVersion"`
		TemplateState   json.RawMessage            `json:"templateState"`
		TemplateVer     int64                      `json:"templateVer"`
		TemplateVersion int64                      `json:"templateVersion"`
		InSync          bool                       `json:"inSync"`
		Override        *appliedConfigOverrideMeta `json:"override,omitempty"`
	} `json:"configs"`
}

// TestGetNodeAppliedConfig_NoOverrides_TemplateMetadataPresent locks the
// Default: when no override exists for any key, every entry still
// carries templateState + templateVer (driving the editor drawer's left
// pane), and the override field is omitted across the board.
func TestGetNodeAppliedConfig_NoOverrides_TemplateMetadataPresent(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{}

	tplState := json.RawMessage(`{"items":["a"]}`)
	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"providers":{"items":["a"]}}`),
		DesiredVer:  5,
		Reported:    json.RawMessage(`{"providers":{"items":["a"]}}`),
		ReportedVer: 5,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "providers", State: tplState, Version: 5},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	providers, ok := resp.Configs["providers"]
	if !ok {
		t.Fatalf("providers missing")
	}
	if providers.TemplateVer != 5 {
		t.Errorf("templateVer = %d, want 5", providers.TemplateVer)
	}
	if string(providers.TemplateState) != string(tplState) {
		t.Errorf("templateState = %s, want %s", string(providers.TemplateState), string(tplState))
	}
	if providers.Override != nil {
		t.Errorf("override = %+v, want nil", providers.Override)
	}
}

// TestGetNodeAppliedConfig_OverridePresent_FreshTemplate locks the
// override-injection path: a single key has an active override whose
// templateVerAtSet matches currentTemplateVer (i.e. fresh, not stale).
// The entry must surface the override block with stale=false and the
// override state distinct from the template.
func TestGetNodeAppliedConfig_OverridePresent_FreshTemplate(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	tplState := json.RawMessage(`{"rules":["default"]}`)
	overrideState := json.RawMessage(`{"rules":["custom"]}`)
	setAt, _ := time.Parse(time.RFC3339, "2026-04-25T08:00:00Z")
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{
		overrides: []appliedConfigOverrideMeta{
			{
				ConfigKey:          "routing_rules",
				State:              overrideState,
				TemplateVerAtSet:   7,
				CurrentTemplateVer: 7,
				Stale:              false,
				SetBy:              "alice@nexus.ai",
				SetAt:              setAt,
				EmergencyOverride:  false,
			},
		},
	}

	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"routing_rules":{"rules":["custom"]}}`),
		DesiredVer:  9,
		Reported:    json.RawMessage(`{"routing_rules":{"rules":["custom"]}}`),
		ReportedVer: 9,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "routing_rules", State: tplState, Version: 7},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	rr, ok := resp.Configs["routing_rules"]
	if !ok {
		t.Fatalf("routing_rules missing")
	}
	if rr.Override == nil {
		t.Fatalf("override missing on routing_rules entry")
	}
	if rr.Override.Stale {
		t.Errorf("override.stale = true, want false (fresh template)")
	}
	if string(rr.Override.State) != string(overrideState) {
		t.Errorf("override.state = %s, want %s", string(rr.Override.State), string(overrideState))
	}
	if rr.Override.TemplateVerAtSet != 7 || rr.Override.CurrentTemplateVer != 7 {
		t.Errorf("override versions = (atSet=%d, current=%d), want (7,7)",
			rr.Override.TemplateVerAtSet, rr.Override.CurrentTemplateVer)
	}
	if rr.Override.SetBy != "alice@nexus.ai" {
		t.Errorf("override.setBy = %q, want alice@nexus.ai", rr.Override.SetBy)
	}
	if string(rr.TemplateState) != string(tplState) {
		t.Errorf("templateState = %s, want %s (read-only template default)", string(rr.TemplateState), string(tplState))
	}
}

// TestGetNodeAppliedConfig_OverrideStale_TemplateBumped exercises the
// stale-flag path: the override was set at templateVer=7 but the template
// has since moved to version 9, so the override carries stale=true and the
// UI will render the "out of date" badge.
func TestGetNodeAppliedConfig_OverrideStale_TemplateBumped(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	overrideState := json.RawMessage(`{"rules":["custom"]}`)
	setAt, _ := time.Parse(time.RFC3339, "2026-04-20T08:00:00Z")
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{
		overrides: []appliedConfigOverrideMeta{
			{
				ConfigKey:          "routing_rules",
				State:              overrideState,
				TemplateVerAtSet:   7,
				CurrentTemplateVer: 9,
				Stale:              true,
				SetBy:              "alice@nexus.ai",
				SetAt:              setAt,
				EmergencyOverride:  false,
			},
		},
	}

	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"routing_rules":{"rules":["custom"]}}`),
		DesiredVer:  10,
		Reported:    json.RawMessage(`{"routing_rules":{"rules":["custom"]}}`),
		ReportedVer: 10,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "routing_rules", State: json.RawMessage(`{"rules":["new-default"]}`), Version: 9},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	rr, ok := resp.Configs["routing_rules"]
	if !ok {
		t.Fatalf("routing_rules missing")
	}
	if rr.Override == nil {
		t.Fatalf("override missing on routing_rules entry")
	}
	if !rr.Override.Stale {
		t.Errorf("override.stale = false, want true (template moved 7 → 9)")
	}
	if rr.Override.TemplateVerAtSet != 7 {
		t.Errorf("override.templateVerAtSet = %d, want 7", rr.Override.TemplateVerAtSet)
	}
	if rr.Override.CurrentTemplateVer != 9 {
		t.Errorf("override.currentTemplateVer = %d, want 9", rr.Override.CurrentTemplateVer)
	}
	if rr.TemplateVer != 9 {
		t.Errorf("entry.templateVer = %d, want 9 (latest template)", rr.TemplateVer)
	}
}

// TestGetNodeAppliedConfig_OverrideOnKillswitch_EmergencyFlag locks the
// emergency-override surfacing: an override whose configKey is `killswitch`
// (or whose reason starts with `break-glass:`) must be flagged
// emergencyOverride=true so the UI can render the red badge.
func TestGetNodeAppliedConfig_OverrideOnKillswitch_EmergencyFlag(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	overrideState := json.RawMessage(`{"engaged":true}`)
	setAt, _ := time.Parse(time.RFC3339, "2026-04-26T18:00:00Z")
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{
		overrides: []appliedConfigOverrideMeta{
			{
				ConfigKey:          "killswitch",
				State:              overrideState,
				TemplateVerAtSet:   2,
				CurrentTemplateVer: 2,
				Stale:              false,
				SetBy:              "carol@nexus.ai",
				SetAt:              setAt,
				EmergencyOverride:  true,
			},
		},
	}

	stub.thing = &store.ThingRegistry{
		ID:          "proxy-1",
		Type:        "compliance-proxy",
		Desired:     json.RawMessage(`{"killswitch":{"engaged":true}}`),
		DesiredVer:  4,
		Reported:    json.RawMessage(`{"killswitch":{"engaged":true}}`),
		ReportedVer: 4,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: json.RawMessage(`{"engaged":false}`), Version: 2},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "carol", "user-2")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	ks, ok := resp.Configs["killswitch"]
	if !ok {
		t.Fatalf("killswitch missing")
	}
	if ks.Override == nil {
		t.Fatalf("override missing on killswitch entry")
	}
	if !ks.Override.EmergencyOverride {
		t.Errorf("override.emergencyOverride = false, want true (killswitch override)")
	}
}

// TestGetNodeAppliedConfig_OrphanOverride_Surfaces locks the orphan-override
// surfacing path: an override exists for a key with no matching template
// (template was deleted out from under it). The endpoint must include the
// orphan key in the response with templateVer=0 and templateState=null so
// the admin UI can render "no template" + offer a Clear action; without
// this iteration the orphan vanishes from the Configuration tab and the
// only way to clean it up is the global override registry page.
func TestGetNodeAppliedConfig_OrphanOverride_Surfaces(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)

	overrideState := json.RawMessage(`{"rules":["legacy"]}`)
	setAt, _ := time.Parse(time.RFC3339, "2026-04-22T10:00:00Z")
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{
		overrides: []appliedConfigOverrideMeta{
			{
				ConfigKey: "legacy_routing",
				// Hub already serves CurrentTemplateVer=0 + Stale=false for
				// orphans (LEFT JOIN with COALESCE).
				State:              overrideState,
				TemplateVerAtSet:   3,
				CurrentTemplateVer: 0,
				Stale:              false,
				SetBy:              "alice@nexus.ai",
				SetAt:              setAt,
				EmergencyOverride:  false,
			},
		},
	}

	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"legacy_routing":{"rules":["legacy"]}}`),
		DesiredVer:  4,
		Reported:    json.RawMessage(`{"legacy_routing":{"rules":["legacy"]}}`),
		ReportedVer: 4,
	}
	// No matching template — orphan branch.
	stub.templates = []store.ThingConfigTemplate{}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	entry, ok := resp.Configs["legacy_routing"]
	if !ok {
		t.Fatalf("orphan key legacy_routing missing from response: %+v", resp.Configs)
	}
	if entry.TemplateVer != 0 {
		t.Errorf("templateVer = %d, want 0 (orphan)", entry.TemplateVer)
	}
	if string(entry.TemplateState) != "null" {
		t.Errorf("templateState = %s, want null (orphan)", string(entry.TemplateState))
	}
	if entry.Override == nil {
		t.Fatalf("override missing on orphan entry")
	}
	if string(entry.Override.State) != string(overrideState) {
		t.Errorf("override.state = %s, want %s", string(entry.Override.State), string(overrideState))
	}
}

// TestGetNodeAppliedConfig_OverrideFetcherError_DegradesGracefully verifies
// the handler does not 500 when the override fetcher returns an error —
// entries still carry templateState + templateVer, just no override metadata.
// This mirrors the existing history-error-skipped semantics.
func TestGetNodeAppliedConfig_OverrideFetcherError_DegradesGracefully(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	h.AppliedConfigOverrideFetcher = &stubAppliedConfigOverrideFetcher{
		err: errors.New("hub unreachable"),
	}

	tplState := json.RawMessage(`{"items":["a"]}`)
	stub.thing = &store.ThingRegistry{
		ID:          "gw-1",
		Type:        "ai-gateway",
		Desired:     json.RawMessage(`{"providers":{"items":["a"]}}`),
		DesiredVer:  1,
		Reported:    json.RawMessage(`{"providers":{"items":["a"]}}`),
		ReportedVer: 1,
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "ai-gateway", ConfigKey: "providers", State: tplState, Version: 1},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/gw-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("gw-1")

	if err := h.GetNodeAppliedConfig(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (handler must not 500 on hub failure)", rec.Code, rec.Body.String())
	}

	var resp decodeAppliedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	providers, ok := resp.Configs["providers"]
	if !ok {
		t.Fatalf("providers missing")
	}
	if providers.TemplateVer != 1 {
		t.Errorf("templateVer = %d, want 1", providers.TemplateVer)
	}
	if string(providers.TemplateState) != string(tplState) {
		t.Errorf("templateState mismatch on hub-failure path")
	}
	if providers.Override != nil {
		t.Errorf("override = %+v, want nil on hub failure", providers.Override)
	}
}
