package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
)

// tools_resource.go wires the pure OpenAPI engine (internal/capabilities/resource)
// to the agent kernel: the search-first resource_* tools. The engine owns the
// catalog, search, distill, and path substitution; this file owns only the agent.Tool
// adapters (funcTool), the confirm gate plumbing, and the admin-call execution over
// the Gateway. The split keeps the engine a reusable, dependency-free leaf.

// stringMap coerces a JSON object (path params or query) into a string map. Path
// and query values are strings on the wire, but a model may emit a JSON number or
// bool for an id; json.Number preserves integer ids exactly (no float64 rounding).
// A nil/invalid object yields a nil map (no params).
func stringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case json.Number:
			out[k] = t.String()
		case bool:
			out[k] = fmt.Sprintf("%t", t)
		case nil:
			// omit — a null param is "unset"
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	return out
}

// bodyArg returns the raw JSON body for an AdminRequest, or untyped nil when empty
// so roundtrip sends no body (and no JSON content-type) — a no-body action must not
// transmit a literal "null".
func bodyArg(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// adminResult relays an AdminRequest outcome to the model: a transport/IAM/validation
// error is surfaced verbatim (status + message) so it self-corrects; success returns
// status + the raw body.
func adminResult(method, path string, raw json.RawMessage, status int, err error) (agent.Result, error) {
	if err != nil {
		return errResult("%s %s → %d: %s", method, path, status, err), nil
	}
	out := map[string]any{"status": status}
	if len(raw) > 0 {
		out["body"] = raw
	}
	return jsonResult(out)
}

// resourceSearchMatch is one candidate the resource_search tool returns: enough
// for the model to choose an operation and call resource_read / resource_invoke,
// without the full per-kind schema (that comes from resource_describe).
type resourceSearchMatch struct {
	Kind        string `json:"kind"`
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Label       string `json:"label"`
	Write       bool   `json:"write"`
}

// executeOp is the single execution path for every resource operation (searched
// reads and writes alike): substitute the named path params, attach the query,
// send the body, and relay the HTTP outcome verbatim so the model self-corrects on
// a 400/403. Tier is enforced by the agent's permission gate off the tool's
// declared tier, not here.
func executeOp(ctx context.Context, gw Gateway, op resource.Operation, params, query map[string]string, body json.RawMessage) (agent.Result, error) {
	path, err := resource.SubstituteParams(op.Path, params)
	if err != nil {
		return errResult("%s %s: %s", op.Method, op.OperationID, err), nil
	}
	var q url.Values
	if len(query) > 0 {
		q = url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
	}
	raw, status, err := gw.AdminRequest(ctx, op.Method, path, q, bodyArg(body))
	return adminResult(op.Method, path, raw, status, err)
}

// resourceReadTools are the auto-tier discovery + read tools (no write risk):
// search (find candidate operations), describe (one kind's schema), and read
// (execute a GET). The set is search-first by design — the model is handed a small
// ranked candidate list, never the whole catalog — so token cost stays flat as the
// catalog (or any external OpenAPI surface) grows.
func resourceReadTools(gw Gateway) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "resource_search", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"free text matched against kind/operationId/path/label, e.g. \"node override\" or \"cache stats\""},"limit":{"type":"integer","description":"max candidates (default 20)"}},"required":["query"]}`),
			desc:   "Search the Control Plane admin API for operations matching a query (ranked, code-level match over kind, operationId, path, label, and each op's summary). Use this for ANY admin resource or entity named by the operator — hooks, routing rules, providers, virtual keys, IAM, caches, jobs, and more. Call it FIRST to find the operationId you need — it returns a short candidate list, not the whole catalog. Then resource_describe the kind for its body schema, and resource_read (GET) or resource_invoke (write) to act.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				_ = json.Unmarshal(in, &a)
				cands := resource.Search(a.Query, a.Limit)
				matches := make([]resourceSearchMatch, 0, len(cands))
				for _, op := range cands {
					matches = append(matches, resourceSearchMatch{
						Kind: op.Kind, OperationID: op.OperationID, Method: op.Method,
						Path: op.Path, Label: op.Label(), Write: op.Mutating(),
					})
				}
				return jsonResult(map[string]any{"query": a.Query, "matches": matches})
			}},

		&funcTool{name: "resource_describe", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"]}`),
			desc:   "Return a compact schema for a resource kind: every operation with its operationId, label, method, path, params (path + query), and request-body fields (name/type/required/enum). Read this to learn the operationId + exact body before a resource_read / resource_invoke.",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Kind string `json:"kind"`
				}
				_ = json.Unmarshal(in, &a)
				// Distill to the fields the model acts on rather than echoing the whole
				// OpenAPI YAML — the raw spec is several KB that would persist as a tool
				// result and bloat the context (see the context-cost optimization spec).
				d, ok := resource.Distill(a.Kind)
				if !ok {
					return errResult("unknown kind %q — valid kinds: %s", a.Kind, strings.Join(resource.KindNames(), ", ")), nil
				}
				return jsonResult(d)
			}},

		&funcTool{name: "resource_read", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"},"operationId":{"type":"string"},"params":{"type":"object","description":"path placeholders by name, e.g. {\"id\":\"…\",\"configKey\":\"…\"}"},"query":{"type":"object","description":"optional query params (paging/filters) per resource_describe"}},"required":["kind","operationId"]}`),
			desc:   "Execute a read (GET) operation by operationId (from resource_search / resource_describe). params fills the path placeholders by name; query adds paging/filters. Read-only — a write operationId is rejected (use resource_invoke).",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Kind        string          `json:"kind"`
					OperationID string          `json:"operationId"`
					Params      json.RawMessage `json:"params"`
					Query       json.RawMessage `json:"query"`
				}
				_ = json.Unmarshal(in, &a)
				op, ok := resource.FindOp(a.Kind, a.OperationID)
				if !ok {
					return errResult("no operation %q on kind %q — use resource_search to find it", a.OperationID, a.Kind), nil
				}
				if op.Mutating() {
					return errResult("%q is a %s write — use resource_invoke", a.OperationID, op.Method), nil
				}
				return executeOp(ctx, gw, op, stringMap(a.Params), stringMap(a.Query), nil)
			}},
	}
}

// resourceWriteTools are the confirm-tier write tools (the agent loop's confirm
// gate authorizes them; MCP includes them only behind --enable-mitigate). One
// generic tool reaches every mutating operation in the catalog by operationId.
func resourceWriteTools(gw Gateway) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "resource_invoke", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"},"operationId":{"type":"string"},"params":{"type":"object","description":"path placeholders by name, e.g. {\"id\":\"…\"}"},"query":{"type":"object"},"body":{"type":"object","description":"request payload; fields from resource_describe"}},"required":["kind","operationId"]}`),
			desc:   "Execute a write (POST/PUT/PATCH/DELETE) operation by operationId (from resource_search / resource_describe) — create, update, delete, singleton config PUT, an item action, or an RPC. params fills path placeholders by name; body is the payload. Confirm-gated; the server 400 is authoritative on a bad body. For reads use resource_read.",
			// confirmDetail resolves the concrete request so the operator confirms
			// the exact METHOD + substituted path + operationId, not a generic reason.
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					Kind        string          `json:"kind"`
					OperationID string          `json:"operationId"`
					Params      json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(in, &a)
				op, ok := resource.FindOp(a.Kind, a.OperationID)
				if !ok {
					return ""
				}
				path := op.Path
				if sub, err := resource.SubstituteParams(op.Path, stringMap(a.Params)); err == nil {
					path = sub
				}
				return fmt.Sprintf("%s %s (%s)", op.Method, path, op.OperationID)
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Kind        string          `json:"kind"`
					OperationID string          `json:"operationId"`
					Params      json.RawMessage `json:"params"`
					Query       json.RawMessage `json:"query"`
					Body        json.RawMessage `json:"body"`
				}
				_ = json.Unmarshal(in, &a)
				op, ok := resource.FindOp(a.Kind, a.OperationID)
				if !ok {
					return errResult("no operation %q on kind %q — use resource_search to find it", a.OperationID, a.Kind), nil
				}
				if !op.Mutating() {
					return errResult("%q is a GET read — use resource_read", a.OperationID), nil
				}
				return executeOp(ctx, gw, op, stringMap(a.Params), stringMap(a.Query), a.Body)
			}},
	}
}
