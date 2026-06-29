package resource

import (
	"github.com/goccy/go-json"
	"net/url"
	"sort"
	"strings"

	capres "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/restable"
)

func buildFrame(msg resDataMsg) *resFrame {
	if rows, ok := restable.ExtractRows(msg.raw); ok {
		return &resFrame{
			kind: msg.kind, title: msg.title, mode: frameTable, params: msg.params,
			op: msg.op, rows: rows, raw: msg.raw, cols: restable.InferColumns(rows, resTableCol), query: msg.query,
		}
	}
	return &resFrame{
		kind: msg.kind, title: msg.title, mode: frameRecord, params: msg.params,
		op: msg.op, raw: msg.raw, menu: msg.menu, query: msg.query,
	}
}

func (r *resourceView) pushRecord(kind string, params map[string]string, raw json.RawMessage, menu []capres.OperationInfo, title string) {
	r.stack = append(r.stack, &resFrame{kind: kind, title: title, mode: frameRecord, params: params, raw: raw, menu: menu})
}

func (r *resourceView) pushMenu(kind string, params map[string]string, menu []capres.OperationInfo, title string) {
	r.stack = append(r.stack, &resFrame{kind: kind, title: title, mode: frameMenu, params: params, menu: menu})
}

func (fr *resFrame) pageRows() []restable.Row {
	return restable.Paginate(fr.rows, fr.page, resPageSize).Rows
}

// back pops one frame (or closes an open form), so ←/esc steps up the cascade
// before the root pops the nav stack.
func (r *resourceView) Back() bool {
	if r.form != nil {
		r.form = nil
		return true
	}
	if len(r.stack) > 0 {
		r.stack = r.stack[:len(r.stack)-1]
		r.err, r.note = nil, ""
		return true
	}
	return false
}

// --- catalog navigation helpers ---

// childParamOf returns the next path-parameter name after basePath among the
// kind's operations (the id of the collection a row drills into), or "" when no
// operation nests under basePath.
func childParamOf(kind, basePath string) string {
	prefix := basePath + "/"
	for _, op := range capres.Operations(kind) {
		if !strings.HasPrefix(op.Path, prefix) {
			continue
		}
		seg := strings.TrimPrefix(op.Path, prefix)
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			return seg[1 : len(seg)-1]
		}
	}
	return ""
}

// childOps are the operations reachable from a record at basePath: writes on the
// record itself (PUT/PATCH/DELETE basePath) and everything nested under basePath/
// (sub-collections, item actions, deep writes). The GET on basePath is excluded —
// it is the record being shown.
func childOps(kind, basePath string) []capres.OperationInfo {
	var out []capres.OperationInfo
	for _, op := range capres.Operations(kind) {
		switch {
		case op.Path == basePath && op.Mutating:
			out = append(out, op)
		case strings.HasPrefix(op.Path, basePath+"/"):
			out = append(out, op)
		}
	}
	return out
}

// findGet returns the GET operation whose path is exactly path (the canonical
// fetch of a record), if any.
func findGet(kind, path string) (capres.OperationInfo, bool) {
	for _, op := range capres.Operations(kind) {
		if op.Path == path && op.Method == "GET" {
			return op, true
		}
	}
	return capres.OperationInfo{}, false
}

func missingParams(op capres.OperationInfo, bound map[string]string) []string {
	var miss []string
	for _, p := range op.Params {
		if _, ok := bound[p]; !ok {
			miss = append(miss, p)
		}
	}
	return miss
}

func cloneParams(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// toValues converts a name=value map into url.Values (nil for an empty map), with
// deterministic key ordering.
func toValues(m map[string]string) url.Values {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := url.Values{}
	for _, k := range keys {
		v.Set(k, m[k])
	}
	return v
}

// --- rendering ---
