package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
)

func findTool(tools []agent.Tool, name string) agent.Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func runResourceTool(t *testing.T, tool agent.Tool, in map[string]any) agent.Result {
	t.Helper()
	if tool == nil {
		t.Fatal("tool not found")
	}
	raw, _ := json.Marshal(in)
	res, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("tool %s returned a hard error: %v", tool.Name(), err)
	}
	return res
}

func TestStringMap(t *testing.T) {
	m := stringMap(json.RawMessage(`{"s":"x","n":42,"big":9007199254740993,"b":true,"z":null}`))
	if m["s"] != "x" || m["n"] != "42" || m["b"] != "true" {
		t.Fatalf("stringMap coercion wrong: %v", m)
	}
	if m["big"] != "9007199254740993" {
		t.Fatalf("stringMap must preserve large integer ids exactly, got %q", m["big"])
	}
	if _, ok := m["z"]; ok {
		t.Fatalf("a null value must be omitted, got %v", m)
	}
	if stringMap(nil) != nil || stringMap(json.RawMessage(`not json`)) != nil {
		t.Fatal("nil/invalid input must yield a nil map")
	}
}

// --- search-first agent tools ---

func TestResourceSearchTool(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_search"),
		map[string]any{"query": "virtual-keys", "limit": 5})
	if res.IsError {
		t.Fatalf("resource_search errored: %s", res.Content)
	}
	// The top candidates are full executable cards: the structural identity PLUS
	// the spec semantics (summary) the model previously never saw — the blind
	// re-rank fix. "List virtual keys" is listVirtualKeys' OpenAPI summary.
	for _, want := range []string{`"cards"`, "listVirtualKeys", `"operationId"`, `"summary"`, "List virtual keys"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("resource_search missing %q:\n%s", want, res.Content)
		}
	}
	if len(gw.adminCalls) != 0 {
		t.Fatal("search is local — it must not hit the admin API")
	}
}

// TestResourceSearchToolCardIsExecutable asserts the one-step contract end to
// end: a write card returned by resource_search carries the body skeleton
// (field names + required markers) the model needs to resource_invoke it in
// the same turn, without a resource_describe round trip.
func TestResourceSearchToolCardIsExecutable(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_search"),
		map[string]any{"query": "create virtual key"})
	var out struct {
		Cards []struct {
			OperationID string `json:"operationId"`
			Write       bool   `json:"write"`
			Body        []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"body"`
		} `json:"cards"`
		More []map[string]any `json:"more"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("search result is not the two-segment shape: %v\n%s", err, res.Content)
	}
	var card *struct {
		OperationID string `json:"operationId"`
		Write       bool   `json:"write"`
		Body        []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
		} `json:"body"`
	}
	for i := range out.Cards {
		if out.Cards[i].OperationID == "createVirtualKey" {
			card = &out.Cards[i]
			break
		}
	}
	if card == nil {
		t.Fatalf("createVirtualKey not in the card window for its own words:\n%s", res.Content)
	}
	if !card.Write {
		t.Fatal("createVirtualKey card must be marked write")
	}
	if len(card.Body) == 0 {
		t.Fatal("a write card must carry its body skeleton — one-step execution is the point")
	}
	// thin tail entries stay thin: no summary/params/body keys
	for _, m := range out.More {
		for _, k := range []string{"summary", "params", "body", "write"} {
			if _, ok := m[k]; ok {
				t.Fatalf("thin tail entry leaked %q — tail must stay 4-field", k)
			}
		}
	}
}

func TestResourceReadBuildsPathAndForwardsQuery(t *testing.T) {
	gw := &fakeGateway{adminBody: json.RawMessage(`[{"id":"vk-1"}]`)}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_read"),
		map[string]any{"kind": "virtual-keys", "operationId": "listVirtualKeys", "query": map[string]any{"status": "active"}})
	if res.IsError || !strings.Contains(res.Content, "vk-1") || !strings.Contains(res.Content, "200") {
		t.Fatalf("resource_read must relay status + body, got: %s", res.Content)
	}
	if c := gw.adminCalls[0]; c.method != "GET" || c.path != "/api/admin/virtual-keys" || c.query.Get("status") != "active" {
		t.Fatalf("resource_read built %s %s q=%v", c.method, c.path, c.query)
	}
}

func TestResourceReadSubstitutesParam(t *testing.T) {
	gw := &fakeGateway{}
	runResourceTool(t, findTool(resourceReadTools(gw), "resource_read"),
		map[string]any{"kind": "identity-providers", "operationId": "getIdentityProvider", "params": map[string]any{"idpId": "idp-1"}})
	// identity-providers uses {idpId}; the engine fills it by name regardless.
	if len(gw.adminCalls) == 0 {
		t.Fatal("resource_read must reach the admin API")
	}
	if c := gw.adminCalls[0]; c.method != "GET" || !strings.HasSuffix(c.path, "/identity-providers/idp-1") {
		t.Fatalf("get on a {idpId}-keyed kind built %s %s", c.method, c.path)
	}
}

func TestResourceReadRejectsWrite(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_read"),
		map[string]any{"kind": "virtual-keys", "operationId": "createVirtualKey"})
	if !res.IsError || !strings.Contains(res.Content, "resource_invoke") {
		t.Fatalf("resource_read must refuse a write op and point at resource_invoke, got: %s", res.Content)
	}
	if len(gw.adminCalls) != 0 {
		t.Fatal("a refused read must not reach the executor")
	}
}

func TestResourceInvokeBuildsPathAndBody(t *testing.T) {
	gw := &fakeGateway{}
	wt := resourceWriteTools(gw)

	runResourceTool(t, findTool(wt, "resource_invoke"),
		map[string]any{"kind": "virtual-keys", "operationId": "createVirtualKey", "body": map[string]any{"name": "k"}})
	if c := gw.adminCalls[len(gw.adminCalls)-1]; c.method != "POST" || c.path != "/api/admin/virtual-keys" {
		t.Fatalf("create built %s %s", c.method, c.path)
	}
	if body, _ := gw.adminCalls[0].body.(json.RawMessage); !strings.Contains(string(body), `"name":"k"`) {
		t.Fatalf("create body not forwarded verbatim: %s", body)
	}

	// A 2-path-param write — BOTH placeholders must be filled.
	runResourceTool(t, findTool(wt, "resource_invoke"),
		map[string]any{"kind": "nodes", "operationId": "setNodeOverride",
			"params": map[string]any{"id": "n1", "configKey": "cache.ttl"}, "body": map[string]any{"value": true}})
	if c := gw.adminCalls[len(gw.adminCalls)-1]; c.method != "PUT" || c.path != "/api/admin/nodes/n1/overrides/cache.ttl" {
		t.Fatalf("setNodeOverride built %s %s (2-param substitution)", c.method, c.path)
	}
}

func TestResourceInvokeRejectsRead(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceWriteTools(gw), "resource_invoke"),
		map[string]any{"kind": "virtual-keys", "operationId": "getVirtualKey", "params": map[string]any{"id": "x"}})
	if !res.IsError || !strings.Contains(res.Content, "resource_read") {
		t.Fatalf("resource_invoke must refuse a GET and point at resource_read, got: %s", res.Content)
	}
	if len(gw.adminCalls) != 0 {
		t.Fatal("a refused write must not reach the executor")
	}
}

func TestResourceInvokeMissingParamRejectedLocally(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceWriteTools(gw), "resource_invoke"),
		map[string]any{"kind": "nodes", "operationId": "setNodeOverride", "params": map[string]any{"id": "n1"}})
	if !res.IsError || !strings.Contains(res.Content, "configKey") {
		t.Fatalf("a missing path param must be rejected naming it, got: %s", res.Content)
	}
	if len(gw.adminCalls) != 0 {
		t.Fatal("a half-substituted path must never reach the executor")
	}
}

func TestResourceUnknownOpRejectedLocally(t *testing.T) {
	gw := &fakeGateway{}
	for _, tc := range []struct {
		tools []agent.Tool
		tool  string
	}{
		{resourceReadTools(gw), "resource_read"},
		{resourceWriteTools(gw), "resource_invoke"},
	} {
		res := runResourceTool(t, findTool(tc.tools, tc.tool), map[string]any{"kind": "virtual-keys", "operationId": "frobnicate"})
		if !res.IsError || !strings.Contains(res.Content, "resource_search") {
			t.Fatalf("%s on an unknown operationId must point at resource_search, got: %s", tc.tool, res.Content)
		}
	}
	if len(gw.adminCalls) != 0 {
		t.Fatal("an unknown operationId must not reach the executor")
	}
}

func TestResourceForbiddenRelayedToModel(t *testing.T) {
	gw := &fakeGateway{errOn: "AdminRequest"}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_read"),
		map[string]any{"kind": "virtual-keys", "operationId": "listVirtualKeys"})
	if !res.IsError || !strings.Contains(res.Content, "AdminRequest failed") {
		t.Fatalf("an executor error (e.g. 403) must be relayed to the model, got: %s", res.Content)
	}
}

func TestResourceTiers(t *testing.T) {
	gw := &fakeGateway{}
	for _, n := range []string{"resource_search", "resource_describe", "resource_read"} {
		if findTool(resourceReadTools(gw), n).Tier() != agent.TierAuto {
			t.Fatalf("%s must be auto-tier (read)", n)
		}
	}
	if findTool(resourceWriteTools(gw), "resource_invoke").Tier() != agent.TierConfirm {
		t.Fatal("resource_invoke must be confirm-tier (write)")
	}
}

func TestResourceWriteToolsGatedByMitigate(t *testing.T) {
	gw := &fakeGateway{}
	names := func(includeMitigate bool) map[string]bool {
		m := map[string]bool{}
		for _, t := range gatewayTools(gw, "", includeMitigate) {
			m[t.Name()] = true
		}
		return m
	}
	off, on := names(false), names(true)
	if !off["resource_search"] || !off["resource_read"] || !off["resource_describe"] {
		t.Fatal("resource read tools must be present without mitigate")
	}
	if off["resource_invoke"] {
		t.Fatal("resource_invoke must be absent without mitigate (MCP safety)")
	}
	if !on["resource_invoke"] {
		t.Fatal("resource_invoke must be present with mitigate enabled")
	}
}

func TestResourceDescribeReturnsDistilledSchema(t *testing.T) {
	gw := &fakeGateway{}
	res := runResourceTool(t, findTool(resourceReadTools(gw), "resource_describe"), map[string]any{"kind": "virtual-keys"})
	if res.IsError {
		t.Fatalf("resource_describe errored: %s", res.Content)
	}
	// The distilled schema exposes operationId + label per op (the model needs the
	// operationId for resource_read/resource_invoke), not the raw OpenAPI YAML.
	for _, want := range []string{`"operations"`, `"operationId"`, `"label"`, "listVirtualKeys"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("resource_describe missing %q:\n%s", want, res.Content[:min(600, len(res.Content))])
		}
	}
	if strings.Contains(res.Content, "openapi:") || strings.Contains(res.Content, "x-nexus") {
		t.Fatalf("resource_describe must NOT echo the raw OpenAPI YAML:\n%s", res.Content[:min(600, len(res.Content))])
	}
	res = runResourceTool(t, findTool(resourceReadTools(gw), "resource_describe"), map[string]any{"kind": "nope"})
	if !res.IsError || !strings.Contains(res.Content, "unknown kind") {
		t.Fatalf("resource_describe on an unknown kind must error: %s", res.Content)
	}
}

// TestResourceCatalog_NoMutationReachableAtAutoTier is the binding safety guard:
// every non-GET (mutating) catalog operation MUST be refused by the only auto-tier
// execution tool (resource_read), so no write can ever run without confirmation.
// The single write path (resource_invoke) and every mitigate_* tool must be
// confirm-tier. A future write tool added at TierAuto fails this test. It enumerates
// the catalog through the resource engine's public API (Kinds/Operations).
func TestResourceCatalog_NoMutationReachableAtAutoTier(t *testing.T) {
	gw := &fakeGateway{}
	read := findTool(resourceReadTools(gw), "resource_read")
	if read.Tier() != agent.TierAuto {
		t.Fatalf("resource_read must be the auto-tier read tool, got %v", read.Tier())
	}
	mutating := 0
	for _, k := range resource.Kinds() {
		for _, op := range resource.Operations(k.Kind) {
			if !op.Mutating {
				continue
			}
			mutating++
			in, _ := json.Marshal(map[string]string{"kind": k.Kind, "operationId": op.OperationID})
			res, err := read.Run(context.Background(), in)
			if err != nil {
				t.Fatalf("resource_read errored on %s/%s: %v", k.Kind, op.OperationID, err)
			}
			if !res.IsError {
				t.Fatalf("resource_read (TierAuto) must REFUSE mutating op %s %s/%s — no write may run without confirmation",
					op.Method, k.Kind, op.OperationID)
			}
		}
	}
	if mutating == 0 {
		t.Fatal("expected the catalog to contain mutating ops to guard against")
	}
	// the only generic write path is resource_invoke, which must be confirm-tier.
	if inv := findTool(resourceWriteTools(gw), "resource_invoke"); inv.Tier() != agent.TierConfirm {
		t.Fatalf("resource_invoke must be TierConfirm, got %v", inv.Tier())
	}
	// every mitigate_* tool is confirm-tier too — no entity write auto-runs.
	for _, tl := range mitigateTools(gw) {
		if tl.Tier() != agent.TierConfirm {
			t.Fatalf("mitigate tool %q must be TierConfirm, got %v", tl.Name(), tl.Tier())
		}
	}
}

// TestResourceInvokeConfirmDetail asserts resource_invoke resolves the concrete
// "METHOD /path (operationId)" for the gate's informed-confirm prompt, and yields
// an empty detail for an unresolvable op (so the gate falls back to its generic reason).
func TestResourceInvokeConfirmDetail(t *testing.T) {
	gw := &fakeGateway{}
	inv := findTool(resourceWriteTools(gw), "resource_invoke").(*funcTool)
	var kind, opID, method string
	for _, k := range resource.Kinds() {
		for _, op := range resource.Operations(k.Kind) {
			if op.Mutating {
				kind, opID, method = k.Kind, op.OperationID, op.Method
				break
			}
		}
		if opID != "" {
			break
		}
	}
	in, _ := json.Marshal(map[string]string{"kind": kind, "operationId": opID})
	if got := inv.ConfirmDetail(in); !strings.Contains(got, method) || !strings.Contains(got, opID) {
		t.Fatalf("confirm detail should name METHOD + operationId, got %q", got)
	}
	bad, _ := json.Marshal(map[string]string{"kind": "nope", "operationId": "nope"})
	if got := inv.ConfirmDetail(bad); got != "" {
		t.Fatalf("an unresolvable op should yield an empty detail, got %q", got)
	}
}
