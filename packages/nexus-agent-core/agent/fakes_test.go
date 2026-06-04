package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
)

// fakeModel replays scripted responses and records the requests it received.
type fakeModel struct {
	resps      []*ModelResponse
	errs       []error  // optional, parallel to resps
	reasonings []string // optional, parallel to resps — emitted on onReasoning
	calls      int
	gotReqs    []ModelRequest
}

func newFakeModel(resps ...*ModelResponse) *fakeModel { return &fakeModel{resps: resps} }

func (f *fakeModel) Generate(ctx context.Context, req ModelRequest, onText, onReasoning func(string)) (*ModelResponse, error) {
	i := f.calls
	f.calls++
	f.gotReqs = append(f.gotReqs, req)
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.reasonings) && f.reasonings[i] != "" && onReasoning != nil {
		onReasoning(f.reasonings[i])
	}
	if i >= len(f.resps) {
		// Default: end the turn with no tool use (prevents accidental runaway).
		return &ModelResponse{Message: TextMessage(RoleAssistant, ""), StopReason: StopEndTurn}, nil
	}
	r := f.resps[i]
	if onText != nil {
		onText(r.Message.Text())
	}
	return r, nil
}

// blockingModel blocks in Generate until the context is cancelled (or released),
// so a test can assert ctx-cancel tears a call down promptly (interruptibility).
type blockingModel struct{ released chan struct{} }

func (b *blockingModel) Generate(ctx context.Context, _ ModelRequest, _, _ func(string)) (*ModelResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.released:
		return &ModelResponse{Message: TextMessage(RoleAssistant, "ok"), StopReason: StopEndTurn}, nil
	}
}

// densityModel simulates a model whose ACTUAL prompt tokens are `density`× the chars/4
// estimate of the request — i.e. dense JSON/ID content that chars/4 under-counts. It
// reports that exact count in Usage so the Compactor's calibration can learn it, and
// issues `toolRounds` tool calls before ending. Records every request for assertions.
type densityModel struct {
	density    float64
	toolRounds int
	calls      int
	gotReqs    []ModelRequest
}

func (d *densityModel) Generate(_ context.Context, req ModelRequest, _, _ func(string)) (*ModelResponse, error) {
	d.gotReqs = append(d.gotReqs, req)
	i := d.calls
	d.calls++
	usage := &Usage{PromptTokens: int(float64(estimateMessages(req.Messages)) * d.density)}
	if i < d.toolRounds {
		r := asstToolUse("u", "read", `{}`)
		r.Usage = usage
		return r, nil
	}
	return &ModelResponse{Message: TextMessage(RoleAssistant, "done"), StopReason: StopEndTurn, Usage: usage}, nil
}

// overflowModel returns a context-overflow error for the first `overflowTimes` calls,
// then succeeds — simulating a prompt the budget recompacts down to fit on retry.
type overflowModel struct {
	overflowTimes int
	err           error
	calls         int
}

func (m *overflowModel) Generate(_ context.Context, _ ModelRequest, _, _ func(string)) (*ModelResponse, error) {
	m.calls++
	if m.calls <= m.overflowTimes {
		return nil, m.err
	}
	return &ModelResponse{Message: TextMessage(RoleAssistant, "recovered"), StopReason: StopEndTurn, Usage: &Usage{PromptTokens: 100}}, nil
}

// asstToolUse builds an assistant ModelResponse that calls one tool.
func asstToolUse(id, name, args string) *ModelResponse {
	return &ModelResponse{
		Message: Message{Role: RoleAssistant, Blocks: []Block{
			{Type: BlockToolUse, ID: id, ToolName: name, Input: json.RawMessage(args)},
		}},
		StopReason: StopToolUse,
	}
}

// asstText builds an assistant ModelResponse that ends the turn with text.
func asstText(text string) *ModelResponse {
	return &ModelResponse{Message: TextMessage(RoleAssistant, text), StopReason: StopEndTurn}
}

// stubTool is a configurable Tool double that records inputs and call count.
type stubTool struct {
	name   string
	tier   Tier
	schema json.RawMessage
	run    func(json.RawMessage) (Result, error)
	calls  int32
	mu     sync.Mutex
	inputs []json.RawMessage
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.name + " (stub)" }

func (s *stubTool) Schema() json.RawMessage {
	if s.schema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return s.schema
}

func (s *stubTool) Tier() Tier { return s.tier }

func (s *stubTool) Run(ctx context.Context, in json.RawMessage) (Result, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	s.mu.Unlock()
	if s.run != nil {
		return s.run(in)
	}
	return Result{Content: "ok"}, nil
}
