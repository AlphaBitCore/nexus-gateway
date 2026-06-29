package hub

import (
	"github.com/goccy/go-json"
	"testing"
)

// hubThingFixture mirrors store.Thing as emitted by GET /api/hub/things/:id.
// It lives here (rather than importing the hub module) so tests pin the shape
// they adapt against — a silent change to Thing's JSON tags would not slip by.
const hubThingFixture = `{
  "id": "gw-1",
  "type": "ai-gateway",
  "name": "gw-a",
  "version": "dev",
  "address": "127.0.0.1:3050",
  "authType": "bearer",
  "connProtocol": "http",
  "status": "online",
  "desired": {"routing_rules": {}},
  "reported": {"routing_rules": {"state": null, "version": 0}},
  "desiredVer": 3,
  "reportedVer": 2,
  "metadata": {"role": "primary", "metricsUrl": "http://127.0.0.1:3050/metrics", "pid": 1234},
  "lastSeenAt": "2026-04-21T15:00:00Z",
  "enrolledAt": "2026-04-21T12:00:00Z"
}`

func TestRenameNode_LiftsMetadataAndSnakeCases(t *testing.T) {
	out, err := RenameNode([]byte(hubThingFixture))
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// Product-facing keys the UI reads. `metadata` is kept on the wire
	// so the Overview tab can render the Metadata panel; `role` and
	// `metrics_url` are also lifted to top-level for pages that don't
	// peek inside metadata.
	for _, required := range []string{
		"id", "type", "name", "status", "version",
		"listen_address", "auth_type", "conn_protocol",
		"targetConfig", "appliedConfig", "targetVersion", "appliedVersion",
		"last_seen_at", "created_at",
		"role", "metrics_url",
		"metadata",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("required key %q missing; got=%s", required, out)
		}
	}

	// Forbidden: internal names must not leak.
	for _, forbidden := range []string{
		"address", "authType", "connProtocol",
		"desired", "reported", "desiredVer", "reportedVer",
		"lastSeenAt", "enrolledAt",
	} {
		if _, bad := got[forbidden]; bad {
			t.Errorf("forbidden key %q leaked; got=%s", forbidden, out)
		}
	}

	// metadata blob: must round-trip every key (not just role/metricsUrl)
	// so the Overview Metadata panel can render arbitrary producer fields
	// without a CP-side allow-list.
	meta, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata not an object; got=%v", got["metadata"])
	}
	if meta["pid"] != float64(1234) {
		t.Errorf("metadata.pid lost in rename; got=%v", meta["pid"])
	}
	if meta["role"] != "primary" {
		t.Errorf("metadata.role lost; got=%v", meta["role"])
	}

	// UI reads node.type (not nodeType); confirm we keep the original value.
	if got["type"] != "ai-gateway" {
		t.Errorf("type = %v; want ai-gateway", got["type"])
	}
	if got["listen_address"] != "127.0.0.1:3050" {
		t.Errorf("listen_address = %v", got["listen_address"])
	}
	if got["role"] != "primary" {
		t.Errorf("role (lifted from metadata) = %v; want primary", got["role"])
	}
	if got["metrics_url"] != "http://127.0.0.1:3050/metrics" {
		t.Errorf("metrics_url = %v", got["metrics_url"])
	}
	if got["targetVersion"] != float64(3) || got["appliedVersion"] != float64(2) {
		t.Errorf("version fields = target %v applied %v", got["targetVersion"], got["appliedVersion"])
	}
}

func TestRenameThingsList_WrapsAndRenamesEach(t *testing.T) {
	in := []byte(`{"things":[` + hubThingFixture + `],"total":1,"page":1,"pageSize":50}`)
	out, err := RenameThingsList(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if _, bad := got["things"]; bad {
		t.Fatalf("'things' key leaked")
	}
	nodes, ok := got["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("nodes wrapper = %v", got["nodes"])
	}
	node := nodes[0].(map[string]any)
	if node["type"] != "ai-gateway" {
		t.Errorf("inner type = %v; want ai-gateway passthrough", node["type"])
	}
	if _, ok := node["metadata"]; !ok {
		t.Error("metadata should be passed through (Overview panel)")
	}
	if got["total"] != float64(1) {
		t.Errorf("pagination dropped: total=%v", got["total"])
	}
}

func TestRenameDriftResponse(t *testing.T) {
	t.Run("Hub provides outOfSyncKeys: passed through unchanged", func(t *testing.T) {
		in := []byte(`{"drifted":[{"id":"x","type":"agent","desiredVer":2,"reportedVer":1,"lastSeenAt":"2026-04-21T15:00:00Z","outOfSyncKeys":["hooks","routing_rules"]}],"total":1}`)
		out, err := RenameDriftResponse(in)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		var got map[string]any
		_ = json.Unmarshal(out, &got)

		if _, bad := got["drifted"]; bad {
			t.Error("'drifted' leaked")
		}
		arr, ok := got["outOfSync"].([]any)
		if !ok || len(arr) != 1 {
			t.Fatalf("outOfSync wrapper = %v", got["outOfSync"])
		}
		item := arr[0].(map[string]any)
		if item["nodeId"] != "x" || item["nodeType"] != "agent" {
			t.Errorf("id/type rename failed: %+v", item)
		}
		if item["lastSeen"] != "2026-04-21T15:00:00Z" {
			t.Errorf("lastSeenAt → lastSeen rename failed: %v", item["lastSeen"])
		}
		keys, ok := item["outOfSyncKeys"].([]any)
		if !ok {
			t.Fatalf("outOfSyncKeys not an array; got %T %v", item["outOfSyncKeys"], item["outOfSyncKeys"])
		}
		if len(keys) != 2 || keys[0] != "hooks" || keys[1] != "routing_rules" {
			t.Errorf("outOfSyncKeys = %v, want [hooks routing_rules]", keys)
		}
	})

	t.Run("Hub omits outOfSyncKeys: synthesized as empty array", func(t *testing.T) {
		in := []byte(`{"drifted":[{"id":"x","type":"agent","desiredVer":2,"reportedVer":1,"lastSeenAt":"2026-04-21T15:00:00Z"}],"total":1}`)
		out, err := RenameDriftResponse(in)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		arr := got["outOfSync"].([]any)
		item := arr[0].(map[string]any)
		keys, ok := item["outOfSyncKeys"].([]any)
		if !ok {
			// json.Unmarshal of "[]" produces []any{} not nil.
			t.Fatalf("outOfSyncKeys should be [] when Hub omits it; got %T %v", item["outOfSyncKeys"], item["outOfSyncKeys"])
		}
		if len(keys) != 0 {
			t.Errorf("outOfSyncKeys should be empty; got %v", keys)
		}
	})
}

// TestRenameConfigUpdateResponse pins the contract for POST
// /api/admin/config-sync/update responses: Hub emits internal
// thingsNotified/thingsOnline counters, the admin API must surface
// nodesNotified/nodesOnline (the UI reads the latter). ok/version pass
// through untouched.
func TestRenameConfigUpdateResponse(t *testing.T) {
	in := []byte(`{"ok":true,"version":42,"thingDesiredVer":99,"thingsNotified":3,"thingsOnline":2}`)
	out, err := RenameConfigUpdateResponse(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["nodesNotified"] != float64(3) || got["nodesOnline"] != float64(2) {
		t.Errorf("counters = notified %v online %v", got["nodesNotified"], got["nodesOnline"])
	}
	if got["ok"] != true || got["version"] != float64(42) {
		t.Errorf("passthrough fields = ok %v version %v", got["ok"], got["version"])
	}
	if got["targetShadowVersion"] != float64(99) {
		t.Errorf("targetShadowVersion = %v, want 99", got["targetShadowVersion"])
	}
	for _, forbidden := range []string{"thingsNotified", "thingsOnline", "thingDesiredVer"} {
		if _, bad := got[forbidden]; bad {
			t.Errorf("forbidden internal key %q leaked", forbidden)
		}
	}
}

// TestRename_UnmarshalErrorsAreWrapped covers every Rename* function's
// json.Unmarshal-failed branch with malformed input. Each entry pins
// the "hubadapter: ..." error prefix the BFF surfaces upstream so the
// admin UI can distinguish adapter-side failures from Hub-side failures
// in the response surface.
func TestRename_UnmarshalErrorsAreWrapped(t *testing.T) {
	cases := []struct {
		name    string
		fn      func([]byte) ([]byte, error)
		prefix  string
		payload []byte
	}{
		{"things list", RenameThingsList, "hubadapter: unmarshal list", []byte(`not json`)},
		{"node", RenameNode, "hubadapter: unmarshal node", []byte(`not json`)},
		{"config update", RenameConfigUpdateResponse, "hubadapter: unmarshal config update", []byte(`not json`)},
		{"drift", RenameDriftResponse, "hubadapter: unmarshal drift", []byte(`not json`)},
		{"catalog", RenameConfigCatalogResponse, "hubadapter: unmarshal catalog", []byte(`not json`)},
		{"history", RenameConfigHistoryResponse, "hubadapter: unmarshal history", []byte(`not json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(tc.payload)
			if err == nil {
				t.Fatalf("%s: expected unmarshal error", tc.name)
			}
			if got := err.Error(); !contains(got, tc.prefix) {
				t.Errorf("%s: error %q missing prefix %q", tc.name, got, tc.prefix)
			}
		})
	}
}

// TestRenameThingsList_NestedArrayMalformedSurfacesError covers the
// inner renameArrayOfNodes error path — a valid top-level wrapper but
// an unparseable `things` array.
func TestRenameThingsList_NestedArrayMalformedSurfacesError(t *testing.T) {
	if _, err := RenameThingsList([]byte(`{"things": "not-an-array"}`)); err == nil {
		t.Fatal("expected error for things=string instead of array")
	}
}

// TestRenameDriftResponse_NestedArrayMalformedSurfacesError mirrors
// the above for the drift wrapper.
func TestRenameDriftResponse_NestedArrayMalformedSurfacesError(t *testing.T) {
	if _, err := RenameDriftResponse([]byte(`{"drifted": "not-an-array"}`)); err == nil {
		t.Fatal("expected error for drifted=string instead of array")
	}
}

// TestLiftMetadata_EdgeCases covers the empty-raw and unmarshal-fail
// branches in liftMetadata. Both must return (nil, nil) so renameNodeMap
// keeps the original metadata blob without crashing or corrupting the
// node row.
func TestLiftMetadata_EdgeCases(t *testing.T) {
	if role, url := liftMetadata(nil); role != nil || url != nil {
		t.Errorf("empty raw: got role=%v url=%v, want (nil,nil)", role, url)
	}
	if role, url := liftMetadata([]byte(``)); role != nil || url != nil {
		t.Errorf("empty raw: got role=%v url=%v, want (nil,nil)", role, url)
	}
	if role, url := liftMetadata([]byte(`not-json`)); role != nil || url != nil {
		t.Errorf("unparseable metadata: got role=%v url=%v, want (nil,nil)", role, url)
	}
}

// TestRenameNode_MetadataUrlButNoRole covers the
// `role == nil && url != nil` branch in renameNodeMap — a node whose
// metadata has metricsUrl but no role must still surface metrics_url
// at the top level (UI links to /metrics regardless of service role).
func TestRenameNode_MetadataUrlButNoRole(t *testing.T) {
	in := []byte(`{"id": "gw-1", "metadata": {"metricsUrl": "http://x/metrics"}}`)
	out, err := RenameNode(in)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["role"]; ok {
		t.Error("role should not be lifted when absent in metadata")
	}
	mu, ok := got["metrics_url"]
	if !ok {
		t.Fatal("metrics_url should be lifted from metadata.metricsUrl even without role")
	}
	if string(mu) != `"http://x/metrics"` {
		t.Errorf("metrics_url value: got %s", mu)
	}
}

// contains is a tiny strings.Contains shim.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
