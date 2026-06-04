package agent

import (
	"context"
	"encoding/json"
)

// Tier classifies a tool by side effect. The permission Gate uses it as the
// base risk; the danger classifier can escalate an auto tool for a specific
// dangerous input.
type Tier int

const (
	// TierAuto runs immediately: reads (observe/analyze), TUI-driving
	// (navigate/show/highlight), and simulate (no production side effect).
	TierAuto Tier = iota
	// TierConfirm requires human authorization: mitigations (kill-switch,
	// passthrough, provider toggle, cache flush, routing-rule toggle, VK revoke).
	TierConfirm
)

// Result is a tool's output, returned to the model as tool_result content.
type Result struct {
	Content string
	IsError bool
}

// Tool is a typed, named, schema-described capability the agent can call.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage // JSON Schema for the input object
	Tier() Tier
	Run(ctx context.Context, input json.RawMessage) (Result, error)
}

// Registry holds tools by name and renders their schemas for a model request.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty tool registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds or replaces a tool by name, preserving first-seen order.
func (r *Registry) Register(t Tool) {
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Remove deletes a tool by name (no-op if absent) so it is neither advertised in
// Schemas nor callable. Hosts use it to enforce an operator policy that withholds
// a capability — e.g. disabling the raw-body read tools for data governance.
func (r *Registry) Remove(name string) {
	if _, ok := r.tools[name]; !ok {
		return
	}
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Names returns registered tool names in registration order.
func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

// Schemas renders ToolSchema for the model. A nil allow-list exposes every
// tool; a non-nil allow-list narrows to the named subset (skill narrowing),
// skipping names that are not registered.
func (r *Registry) Schemas(allow []string) []ToolSchema {
	names := r.order
	if allow != nil {
		names = allow
	}
	out := make([]ToolSchema, 0, len(names))
	for _, n := range names {
		t, ok := r.tools[n]
		if !ok {
			continue
		}
		out = append(out, ToolSchema{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()})
	}
	return out
}
