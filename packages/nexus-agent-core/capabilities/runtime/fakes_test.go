package runtime

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/url"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func sp(s string) *string { return &s }

var errCanvasTest = errors.New("canvas boom")

// fakeCanvas records the calls the canvas tools make.
type fakeCanvas struct {
	view        string
	filter      core.TrafficFilter
	shownEvent  string
	highlighted string
	err         error
}

func (f *fakeCanvas) Navigate(view string, filter core.TrafficFilter) error {
	if f.err != nil {
		return f.err
	}
	f.view, f.filter = view, filter
	return nil
}
func (f *fakeCanvas) ShowEvent(id string) error {
	if f.err != nil {
		return f.err
	}
	f.shownEvent = id
	return nil
}
func (f *fakeCanvas) Highlight(ref string) error {
	if f.err != nil {
		return f.err
	}
	f.highlighted = ref
	return nil
}

var _ Canvas = (*fakeCanvas)(nil)

// fakeRunner scripts a command's output without spawning a process.
type fakeRunner struct {
	stdout, stderr string
	exit           int
	err            error
	gotCommand     string
}

func (f *fakeRunner) Run(_ context.Context, command string) (runResult, error) {
	f.gotCommand = command
	if f.err != nil {
		return runResult{}, f.err
	}
	return runResult{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exit}, nil
}

var _ runner = (*fakeRunner)(nil)

// fakeChatStreamer scripts a ChatResult and records the request it received.
type fakeChatStreamer struct {
	res       *core.ChatResult
	err       error
	gotVK     string
	gotReq    core.ChatRequest
	stream    string // text to replay through onDelta
	reasoning string // text to replay through onReasoning
}

func (f *fakeChatStreamer) ChatToolStream(_ context.Context, vk string, req core.ChatRequest, onDelta, onReasoning func(string)) (*core.ChatResult, error) {
	f.gotVK = vk
	f.gotReq = req
	if onReasoning != nil && f.reasoning != "" {
		onReasoning(f.reasoning)
	}
	if onDelta != nil && f.stream != "" {
		onDelta(f.stream)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

var _ chatStreamer = (*fakeChatStreamer)(nil)

// scriptedStreamer returns a sequence of ChatResults across successive calls (so
// a multi-round agent loop can be driven end-to-end).
type scriptedStreamer struct {
	steps []*core.ChatResult
	calls int
}

func (s *scriptedStreamer) ChatToolStream(_ context.Context, _ string, _ core.ChatRequest, _, _ func(string)) (*core.ChatResult, error) {
	i := s.calls
	s.calls++
	if i >= len(s.steps) {
		return &core.ChatResult{FinishReason: "stop"}, nil
	}
	return s.steps[i], nil
}

var _ chatStreamer = (*scriptedStreamer)(nil)

func rawArgs(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// fakeGateway implements Gateway with canned returns, a per-method error toggle,
// and write-call counters so tool wiring is asserted without a network.
type fakeGateway struct {
	sparkline   *core.SparklineResult
	instances   *core.InstancesResult
	providers   *core.ProvidersResult
	rules       []core.RoutingRule
	vks         []core.VirtualKey
	traffic     *core.TrafficList
	event       *core.TrafficEvent
	rawForward  json.RawMessage
	killSwitch  *core.KillSwitchState     // ontology live-data: kill-switch probe
	passthrough *core.PassthroughSnapshot // ontology live-data: passthrough probe
	errOn       string                    // method name to fail

	setProviderCalls, revokeCalls, ruleCalls, killCalls, flushCalls, passthroughCalls int

	adminCalls  []adminCall     // recorded AdminRequest invocations (resource tools)
	adminStatus int             // status AdminRequest returns (0 → 200)
	adminBody   json.RawMessage // body AdminRequest returns

	lastCostQ   url.Values         // captured Cost query (window assertions)
	lastTraffic core.TrafficFilter // captured TrafficList filter (window assertions)
}

// adminCall records one AdminRequest invocation so resource-tool tests can assert
// the exact (method, path, query, body) the tool built.
type adminCall struct {
	method string
	path   string
	query  url.Values
	body   any
}

func (f *fakeGateway) AdminRequest(_ context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error) {
	f.adminCalls = append(f.adminCalls, adminCall{method: method, path: path, query: query, body: body})
	if err := f.fail("AdminRequest"); err != nil {
		return nil, 0, err
	}
	status := f.adminStatus
	if status == 0 {
		status = 200
	}
	return f.adminBody, status, nil
}

func (f *fakeGateway) fail(m string) error {
	if f.errOn == m {
		return &core.APIError{Message: m + " failed"}
	}
	return nil
}

func (f *fakeGateway) Sparkline(context.Context, url.Values) (*core.SparklineResult, error) {
	if err := f.fail("Sparkline"); err != nil {
		return nil, err
	}
	if f.sparkline == nil {
		f.sparkline = &core.SparklineResult{}
	}
	return f.sparkline, nil
}
func (f *fakeGateway) Instances(context.Context) (*core.InstancesResult, error) {
	if err := f.fail("Instances"); err != nil {
		return nil, err
	}
	if f.instances == nil {
		f.instances = &core.InstancesResult{}
	}
	return f.instances, nil
}
func (f *fakeGateway) TrafficList(_ context.Context, filter core.TrafficFilter) (*core.TrafficList, error) {
	f.lastTraffic = filter
	if err := f.fail("TrafficList"); err != nil {
		return nil, err
	}
	if f.traffic == nil {
		f.traffic = &core.TrafficList{}
	}
	return f.traffic, nil
}
func (f *fakeGateway) TrafficEvent(context.Context, string) (*core.TrafficEvent, error) {
	if err := f.fail("TrafficEvent"); err != nil {
		return nil, err
	}
	if f.event == nil {
		f.event = &core.TrafficEvent{}
	}
	return f.event, nil
}
func (f *fakeGateway) AdminModels(context.Context) (*core.ModelCatalog, error) {
	if err := f.fail("AdminModels"); err != nil {
		return nil, err
	}
	return &core.ModelCatalog{}, nil
}
func (f *fakeGateway) Cost(_ context.Context, q url.Values) (*core.CostReport, error) {
	f.lastCostQ = q
	if err := f.fail("Cost"); err != nil {
		return nil, err
	}
	return &core.CostReport{}, nil
}
func (f *fakeGateway) LatencyPhases(context.Context, string, url.Values) (*core.LatencyPhasesResult, error) {
	if err := f.fail("LatencyPhases"); err != nil {
		return nil, err
	}
	return &core.LatencyPhasesResult{}, nil
}
func (f *fakeGateway) RoutingFallbacks(context.Context, url.Values) (*core.FallbacksResult, error) {
	if err := f.fail("RoutingFallbacks"); err != nil {
		return nil, err
	}
	return &core.FallbacksResult{}, nil
}
func (f *fakeGateway) SimulatorForward(context.Context, core.SimulatorForwardRequest) (json.RawMessage, error) {
	if err := f.fail("SimulatorForward"); err != nil {
		return nil, err
	}
	if f.rawForward == nil {
		f.rawForward = json.RawMessage(`{"choices":[]}`)
	}
	return f.rawForward, nil
}
func (f *fakeGateway) Alerts(context.Context) (*core.AlertsResult, error) {
	if err := f.fail("Alerts"); err != nil {
		return nil, err
	}
	return &core.AlertsResult{}, nil
}
func (f *fakeGateway) Nodes(context.Context) (*core.NodesResult, error) {
	if err := f.fail("Nodes"); err != nil {
		return nil, err
	}
	return &core.NodesResult{}, nil
}
func (f *fakeGateway) ComplianceOverview(context.Context, url.Values) (*core.ComplianceOverview, error) {
	if err := f.fail("ComplianceOverview"); err != nil {
		return nil, err
	}
	return &core.ComplianceOverview{}, nil
}
func (f *fakeGateway) RoutingSimulate(context.Context, core.RoutingSimulateRequest) (*core.RoutingSimulateResult, error) {
	if err := f.fail("RoutingSimulate"); err != nil {
		return nil, err
	}
	return &core.RoutingSimulateResult{}, nil
}
func (f *fakeGateway) KillSwitchStatus(context.Context) (*core.KillSwitchState, error) {
	if err := f.fail("KillSwitchStatus"); err != nil {
		return nil, err
	}
	if f.killSwitch != nil {
		return f.killSwitch, nil
	}
	return &core.KillSwitchState{}, nil
}
func (f *fakeGateway) PassthroughSnapshot(context.Context) (*core.PassthroughSnapshot, error) {
	if err := f.fail("PassthroughSnapshot"); err != nil {
		return nil, err
	}
	if f.passthrough != nil {
		return f.passthrough, nil
	}
	return &core.PassthroughSnapshot{}, nil
}
func (f *fakeGateway) ConfigSyncOutOfSync(context.Context) (*core.ConfigSyncResult, error) {
	if err := f.fail("ConfigSyncOutOfSync"); err != nil {
		return nil, err
	}
	return &core.ConfigSyncResult{}, nil
}
func (f *fakeGateway) Providers(context.Context) (*core.ProvidersResult, error) {
	if err := f.fail("Providers"); err != nil {
		return nil, err
	}
	if f.providers == nil {
		f.providers = &core.ProvidersResult{}
	}
	return f.providers, nil
}
func (f *fakeGateway) RoutingRules(context.Context) ([]core.RoutingRule, error) {
	if err := f.fail("RoutingRules"); err != nil {
		return nil, err
	}
	return f.rules, nil
}
func (f *fakeGateway) VirtualKeys(context.Context) ([]core.VirtualKey, error) {
	if err := f.fail("VirtualKeys"); err != nil {
		return nil, err
	}
	return f.vks, nil
}
func (f *fakeGateway) SetKillSwitch(_ context.Context, _ bool) (*core.KillSwitchResult, error) {
	if err := f.fail("SetKillSwitch"); err != nil {
		return nil, err
	}
	f.killCalls++
	return &core.KillSwitchResult{}, nil
}
func (f *fakeGateway) SetProviderEnabled(_ context.Context, _ string, _ bool) error {
	if err := f.fail("SetProviderEnabled"); err != nil {
		return err
	}
	f.setProviderCalls++
	return nil
}
func (f *fakeGateway) CacheFlush(context.Context) error {
	if err := f.fail("CacheFlush"); err != nil {
		return err
	}
	f.flushCalls++
	return nil
}
func (f *fakeGateway) RevokeVK(_ context.Context, _ string) error {
	if err := f.fail("RevokeVK"); err != nil {
		return err
	}
	f.revokeCalls++
	return nil
}
func (f *fakeGateway) SetRoutingRuleEnabled(_ context.Context, _ string, _ bool) error {
	if err := f.fail("SetRoutingRuleEnabled"); err != nil {
		return err
	}
	f.ruleCalls++
	return nil
}
func (f *fakeGateway) SetPassthroughGlobal(_ context.Context, _ core.PassthroughGlobalRequest) error {
	if err := f.fail("SetPassthroughGlobal"); err != nil {
		return err
	}
	f.passthroughCalls++
	return nil
}

var _ Gateway = (*fakeGateway)(nil)
