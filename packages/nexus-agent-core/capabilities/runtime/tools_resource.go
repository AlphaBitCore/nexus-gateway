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

// executeOp is the single execution path for every resource operation (searched
// reads and writes alike): substitute the named path params, attach the query,
// send the body, and relay the HTTP outcome verbatim so the model self-corrects on
// a 400/403. Tier is enforced by the agent's permission gate off the tool's
// declared tier, not here.
//
// The assistant's own surface is refused: the catalog documents the
// /api/admin/assistant/* endpoints for human consumers, but an agent operating
// its own chat plumbing is self-referential — and the GET stream endpoint is a
// long-lived SSE response the in-process self-call transport must never relay
// (it assumes single-JSON bodies; relaying the agent's own stream would also
// supersede the human's live subscriber and park the turn inside its own tool
// call until the turn deadline).
func executeOp(ctx context.Context, gw Gateway, op resource.Operation, params, query map[string]string, body json.RawMessage) (agent.Result, error) {
	if strings.HasPrefix(op.Path, "/api/admin/assistant/") {
		return errResult("%s operates the assistant's own chat surface — not available as an agent tool (ask the operator to use the chat UI instead)", op.OperationID), nil
	}
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
			schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"free text matched against kind/operationId/path/label and each op's summary, e.g. \"node override\" or \"cache stats\""},"limit":{"type":"integer","description":"max candidates (default 20)"}},"required":["query"]}`),
			desc:   "Search the Control Plane admin API for operations matching a query (ranked, code-level match over kind, operationId, path, label, and each op's summary). Use this for ANY admin resource or entity named by the operator — hooks, routing rules, providers, virtual keys, IAM, caches, jobs, and more. Call it FIRST. The top candidates come back as full cards — summary, params, and body skeleton — so a card that matches is DIRECTLY executable with resource_read (GET) or resource_invoke (write) in the same turn; resource_describe is only needed when no card covers the target (e.g. a thin `more` entry looks right).",
			run: func(_ context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				// A malformed or empty call must teach the schema, never search
				// for "" (which silently returns an arbitrary ranking the model
				// then trusts).
				if err := json.Unmarshal(in, &a); err != nil {
					return errResult(`resource_search takes {"query":"free text","limit":20} — could not parse the input: %v`, err), nil
				}
				if strings.TrimSpace(a.Query) == "" {
					return errResult(`resource_search needs a non-empty "query" (free text matched against kind/operationId/path/label/summary, e.g. "node override" or "cache stats")`), nil
				}
				res := resource.SearchCards(a.Query, 0, a.Limit)
				if len(res.Cards) == 0 && len(res.More) == 0 {
					// A dead-end answer strands the model; name the recovery
					// paths and the searchable kind space instead.
					return errResult("no operations matched %q. Try broader words (the match is code-level over kind/operationId/path/label/summary), or read one kind directly with resource_describe. Available kinds: %s",
						a.Query, strings.Join(resource.KindNames(), ", ")), nil
				}
				out := struct {
					Query string `json:"query"`
					resource.SearchResult
				}{a.Query, res}
				return jsonResult(out)
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
//
// Per-mutation confirmation: resource_invoke is TierConfirm, and the
// agent loop's permission Gate maps EVERY TierConfirm tool to a mandatory human
// authorization (agent.Gate.Decide → Ask) before execution — fail-closed: a nil
// Confirm handler, a Confirm error, or a ctx timeout all DENY and the operation
// never runs (see agent/loop.go and the loop_confirm_test.go suite). The single
// generic tool does NOT weaken this: it reaches every mutation by operationId,
// but each individual call is still gated, and confirmDetail resolves the exact
// METHOD + substituted path + operationId so the operator confirms the concrete
// mutation, not a generic prompt. The run handler additionally rejects any
// non-mutating op (use resource_read), so nothing slips through as a "read".
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
