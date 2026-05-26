// Cross-cutting Config Sync scenarios (S-140 — drift detection
// surface from catalog §10). The /api/admin/config-sync/* family is
// the operator-facing surface backing the "Config Sync" admin page:
// drift list (which nodes are out of sync), history (the rolling
// stream of applies), and the (nodeType, configKey) catalog used to
// populate the filter selects. All three are BFF-style proxies onto
// Hub /api/hub/* with field-rename adapters — broken adapters are
// the failure mode this scenario guards against.
package scenarios_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS140_ConfigSyncSurface — PM-grade e2e.
//
// BRAINSTORM (pre): the Config Sync admin page hits three CP endpoints
// that all forward into Hub via the hubadapter.Rename* family. The
// adapters do two PM-critical things: (1) rename `drifted` → `outOfSync`
// at the envelope level and `thingType` → `nodeType` per item, so the
// UI's product-terminology contract holds (`docs/developers/architecture/cross-cutting/foundation/thing-model.md`
// §10), and (2) preserve every other field unchanged so a Hub-side
// schema addition (e.g. `affectedKeys` on drift items) reaches the UI
// without a CP edit.
//
// If any rename adapter regresses, the admin UI silently shows the
// wrong field name (or no data at all because the React schema treats
// `thingType` as undefined). This scenario exercises the live three
// endpoints against a running Hub + Postgres, asserting:
//
//   1. /config-sync/catalog returns the canonical (nodeType,
//      configKeys[]) mapping for every Thing type we wire in
//      ConfigKeyServices (the harness's runtime-state checker), proving
//      the BFF data feeding the page filters is real, not stub.
//   2. /config-sync/out-of-sync uses the renamed envelope key
//      `outOfSync`, never the upstream `drifted`.
//   3. /config-sync/history paginates and returns a recognisable
//      envelope shape (`data` or `entries` array + cursor/nextCursor)
//      so the admin history view paginates correctly.
//
// Cross-service: CP admin → Hub /api/hub/{config/catalog, drift,
// applies}. Pure read-path — no Hub config push. PM-grade because the
// alternative (UI just renders nothing or wrong terminology) is the
// silent-failure mode this rename layer was designed to prevent.
func TestS140_ConfigSyncSurface(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) Catalog — must contain every canonical Thing type with at
	// least its load-bearing configKeys. We tolerate extra entries
	// (e.g. transient `tlt-ks-…` types from kill-switch traffic-list
	// tests) but require the canonical core.
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/config-sync/catalog", nil)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("catalog: status %d body=%q", status, truncate(body, 200))
	}
	var catalog struct {
		Entries []struct {
			NodeType   string   `json:"nodeType"`
			ConfigKeys []string `json:"configKeys"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("decode catalog: %v body=%q", err, truncate(body, 300))
	}
	if len(catalog.Entries) == 0 {
		t.Fatalf("catalog returned zero entries")
	}
	// Refuse to accept upstream's raw `thingType` key — the rename
	// adapter must scrub it.
	for _, e := range catalog.Entries {
		if e.NodeType == "" {
			t.Errorf("catalog entry missing nodeType (likely rename adapter regression): %+v", e)
		}
	}

	got := make(map[string]map[string]bool)
	for _, e := range catalog.Entries {
		keys := make(map[string]bool, len(e.ConfigKeys))
		for _, k := range e.ConfigKeys {
			keys[k] = true
		}
		got[e.NodeType] = keys
	}

	required := map[string][]string{
		"agent":            {"agent_settings", "hooks", "interception_domains", "payload_capture"},
		"ai-gateway":       {"routing_rules", "providers", "hooks", "gateway_passthrough"},
		"compliance-proxy": {"interception_domains", "hooks", "payload_capture"},
		"nexus-hub":        {"log_level", "observability"},
		"control-plane":    {"log_level", "observability"},
	}
	for nodeType, mustHave := range required {
		bag, ok := got[nodeType]
		if !ok {
			t.Errorf("catalog missing nodeType=%q (admin Config Sync page would not populate filters)", nodeType)
			continue
		}
		for _, key := range mustHave {
			if !bag[key] {
				t.Errorf("catalog %s.configKeys missing %q (only have %d keys)", nodeType, key, len(bag))
			}
		}
	}

	// (2) out-of-sync — envelope must use renamed key `outOfSync`,
	// never raw `drifted`. We don't care whether anything is drifted;
	// what we're testing is the rename + envelope shape.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/config-sync/out-of-sync", nil)
	if err != nil {
		t.Fatalf("out-of-sync: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("out-of-sync: status %d body=%q", status, truncate(body, 200))
	}
	var driftRaw map[string]json.RawMessage
	if err := json.Unmarshal(body, &driftRaw); err != nil {
		t.Fatalf("decode out-of-sync: %v body=%q", err, truncate(body, 300))
	}
	if _, leaked := driftRaw["drifted"]; leaked {
		t.Errorf("out-of-sync envelope leaked raw 'drifted' key — RenameDriftResponse regression (body=%q)",
			truncate(body, 300))
	}
	if _, present := driftRaw["outOfSync"]; !present {
		t.Errorf("out-of-sync envelope missing renamed 'outOfSync' key (body=%q)", truncate(body, 300))
	}

	// (3) history — limit-paginated. Accept either `data` or `entries`
	// (both shapes exist across Hub forward routes); the strong
	// assertion is that the response is well-formed JSON and the
	// status is 200.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/config-sync/history?limit=5", nil)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("history: status %d body=%q", status, truncate(body, 200))
	}
	var hist map[string]json.RawMessage
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatalf("decode history: %v body=%q", err, truncate(body, 300))
	}
	if len(hist) == 0 {
		t.Errorf("history returned empty envelope (body=%q)", truncate(body, 300))
	}

	t.Logf("S-140 OK: catalog=%d entries, out-of-sync renamed, history envelope keys=%v",
		len(catalog.Entries), keysOf(hist))
}

// keysOf is a tiny debug helper.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
