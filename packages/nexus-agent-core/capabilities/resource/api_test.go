package resource

import (
	"sort"
	"strings"
	"testing"
)

func TestResourceKindsSortedWithSummary(t *testing.T) {
	ks := Kinds()
	if len(ks) == 0 {
		t.Fatal("Kinds must return the catalog kinds")
	}
	if !sort.SliceIsSorted(ks, func(i, j int) bool { return ks[i].Kind < ks[j].Kind }) {
		t.Fatal("Kinds must be sorted by kind name")
	}
	var vk *KindInfo
	for i := range ks {
		if ks[i].Kind == "virtual-keys" {
			vk = &ks[i]
		}
	}
	if vk == nil || vk.OpCount == 0 || len(vk.Capabilities) == 0 {
		t.Fatalf("virtual-keys must be present with an op count + capability profile, got %+v", vk)
	}
}

func TestResourceOperationsExposesFullSurface(t *testing.T) {
	ops := Operations("nodes")
	if len(ops) == 0 {
		t.Fatal("nodes must expose operations")
	}
	var sawNestedGet, sawTwoParamWrite bool
	for _, op := range ops {
		if op.OperationID == "getNodeRuntime" && op.Method == "GET" && len(op.Params) == 1 {
			sawNestedGet = true // nested sub-collection GET, previously discarded
		}
		if op.OperationID == "setNodeOverride" && op.Mutating && len(op.Params) == 2 {
			sawTwoParamWrite = true // two-param write, previously unreachable
		}
	}
	if !sawNestedGet || !sawTwoParamWrite {
		t.Fatalf("Operations(nodes) must expose nested + two-param ops, got %d ops", len(ops))
	}
	if Operations("no-such-kind") != nil {
		t.Fatal("an unknown kind must return nil operations")
	}
}

func TestResolveOperationSubstitutes(t *testing.T) {
	method, path, mutating, err := ResolveOperation("nodes", "setNodeOverride", map[string]string{"id": "n1", "configKey": "cache.ttl"})
	if err != nil || method != "PUT" || path != "/api/admin/nodes/n1/overrides/cache.ttl" || !mutating {
		t.Fatalf("ResolveOperation = %s %s mut=%v err=%v", method, path, mutating, err)
	}
	if _, _, _, err := ResolveOperation("nodes", "noSuchOp", nil); err == nil {
		t.Fatal("unknown op must error")
	}
	if _, _, _, err := ResolveOperation("nodes", "setNodeOverride", map[string]string{"id": "n1"}); err == nil {
		t.Fatal("missing param must error")
	}
}

func TestSearchCardsSurfacesKindAndOp(t *testing.T) {
	res := SearchCards("node override", 5, 20)
	for _, c := range res.Cards {
		if c.OperationID == "setNodeOverride" && c.Kind == "nodes" {
			return
		}
	}
	t.Fatalf("SearchCards must surface setNodeOverride with its kind, got %+v", res.Cards)
}

func TestDescribeOperation(t *testing.T) {
	s, ok := DescribeOperation("virtual-keys", "createVirtualKey")
	if !ok || s.Method != "POST" {
		t.Fatalf("DescribeOperation(createVirtualKey) = %+v ok=%v", s, ok)
	}
	if len(s.Body) == 0 {
		t.Fatal("createVirtualKey must describe its body fields")
	}
	if _, ok := DescribeOperation("virtual-keys", "noSuchOp"); ok {
		t.Fatal("unknown op must not describe")
	}
	if _, ok := DescribeOperation("no-such-kind", "x"); ok {
		t.Fatal("unknown kind must not describe")
	}
}

// TestEveryKindReachableViaAPI mirrors the engine completeness test at the public
// TUI/CLI boundary: every operation of every kind resolves through ResolveOperation
// with dummy params, leaving no placeholder.
func TestEveryKindReachableViaAPI(t *testing.T) {
	for _, ki := range Kinds() {
		for _, op := range Operations(ki.Kind) {
			params := map[string]string{}
			for _, p := range op.Params {
				params[p] = "x"
			}
			_, path, _, err := ResolveOperation(ki.Kind, op.OperationID, params)
			if err != nil {
				t.Errorf("%s/%s did not resolve: %v", ki.Kind, op.OperationID, err)
			}
			if strings.ContainsAny(path, "{}") {
				t.Errorf("%s/%s left a placeholder: %s", ki.Kind, op.OperationID, path)
			}
		}
	}
}
