package proxy

// realtime_test_helpers_test.go — shared fixtures for the realtime relay
// tests: a scriptable fake OpenAI realtime WebSocket provider, VK auth stubs
// with the by-hash recheck seam, a realtime-priced model catalog stub, and
// deps/dial/audit-drain helpers.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func rtIngress() Ingress {
	return Ingress{WireShape: typology.WireShapeNone, BodyFormat: provcore.FormatOpenAI}
}

// setRealtimeTimers shortens the package-level session timers for a test and
// restores them at cleanup. Realtime tests must not call t.Parallel().
func setRealtimeTimers(t *testing.T, ping, pong, recheck, guard time.Duration) {
	t.Helper()
	oldPing, oldPong, oldRecheck, oldGuard :=
		realtimePingInterval, realtimePongTimeout, realtimeRecheckInterval, realtimeSessionGuard
	realtimePingInterval, realtimePongTimeout, realtimeRecheckInterval, realtimeSessionGuard =
		ping, pong, recheck, guard
	t.Cleanup(func() {
		realtimePingInterval, realtimePongTimeout, realtimeRecheckInterval, realtimeSessionGuard =
			oldPing, oldPong, oldRecheck, oldGuard
	})
}

// --- VK auth stub with the long-session seam ------------------------------

type rtVKAuth struct {
	meta    *vkauth.VKMeta
	authErr error
	hash    string

	mu           sync.Mutex
	recheckCalls int
	// recheckFn drives RecheckByHash per call number (1-based). Nil = always
	// active.
	recheckFn func(call int) (vkauth.RecheckOutcome, error)
}

func (a *rtVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return a.meta, a.authErr
}

func (a *rtVKAuth) AuthenticateWithHash(_ context.Context, _ *http.Request) (*vkauth.VKMeta, string, error) {
	if a.authErr != nil {
		return nil, "", a.authErr
	}
	return a.meta, a.hash, nil
}

func (a *rtVKAuth) RecheckByHash(_ context.Context, _ string) (vkauth.RecheckOutcome, error) {
	a.mu.Lock()
	a.recheckCalls++
	call := a.recheckCalls
	fn := a.recheckFn
	a.mu.Unlock()
	if fn == nil {
		return vkauth.RecheckActive, nil
	}
	return fn(call)
}

func (a *rtVKAuth) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recheckCalls
}

// entitledVKMeta returns a VK whose AllowedModels explicitly names the
// realtime model — the positive opt-in the dark gate requires.
func entitledVKMeta(vkID string) *vkauth.VKMeta {
	return &vkauth.VKMeta{
		ID: vkID, Name: "rt-" + vkID, OrganizationID: "org-1",
		AllowedModels: []store.AllowedModelRef{{ProviderID: "p-openai", ModelID: "m-rt"}},
	}
}

// --- model catalog stub ---------------------------------------------------

// rtStubModels serves the realtime catalog row for both the entitlement
// pre-resolve (GetModelByCodeOrAlias) and the pricing lookup (GetModel).
type rtStubModels struct {
	model      *store.Model
	catalogErr error
}

func rtPricedModel() *store.Model {
	return &store.Model{
		ID: "m-rt", Code: "gpt-realtime", Name: "GPT Realtime",
		ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-realtime-x",
		Type: "realtime", Enabled: true,
		InputPricePM:           fPtr(4),
		OutputPricePM:          fPtr(16),
		CachedInputReadPricePM: fPtr(0.4),
		AudioInputPricePM:      fPtr(32),
		AudioOutputPricePM:     fPtr(64),
		// CachedAudioInputReadPricePM deliberately nil: cached audio falls
		// back to the primary audio-input rate (the NULL-fallback arm).
	}
}

func (m rtStubModels) GetModel(_ context.Context, _ string) (*store.Model, error) {
	if m.model == nil {
		return nil, errors.New("no model row")
	}
	return m.model, nil
}
func (m rtStubModels) GetModelByCode(_ context.Context, _ string) (*store.Model, error) {
	return nil, errors.New("unused")
}
func (m rtStubModels) GetModelByCodeOrAlias(_ context.Context, _ string) (*store.Model, error) {
	if m.catalogErr != nil {
		return nil, m.catalogErr
	}
	if m.model == nil {
		return nil, nil
	}
	return m.model, nil
}
func (m rtStubModels) ListEnabledModels(_ context.Context) ([]store.Model, error) { return nil, nil }
func (m rtStubModels) FetchModelPricing(_ context.Context, _ []string) ([]store.ModelPricing, error) {
	return nil, nil
}

// --- resolver stub --------------------------------------------------------

type rtStubResolver struct {
	baseURL string
	err     error
}

func (s rtStubResolver) Resolve(_ context.Context, providerID, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	if s.err != nil {
		return provcore.CallTarget{}, s.err
	}
	return provcore.CallTarget{
		ProviderID: providerID, ProviderName: "openai",
		Format: provcore.FormatOpenAI, BaseURL: s.baseURL, APIKey: "sk-upstream",
		CredentialID: "cred-1", CredentialName: "test-cred",
		ProviderModelID: "gpt-realtime-x",
	}, nil
}

// --- fake realtime provider ----------------------------------------------

// rtProvider is a scriptable fake OpenAI realtime WebSocket server. It
// records every handshake's headers + query (the credential-hygiene
// assertions) and hands each accepted conn to the script.
type rtProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	headers  []http.Header
	queries  []url.Values
	accepted int
	// closedCh receives one value per provider conn whose script observed a
	// read failure (peer close / relay teardown) — the no-orphan assertions.
	closedCh chan websocket.StatusCode
}

// newRTProvider starts a fake provider. script(nil ok) runs per accepted
// conn; the default script echoes text frames until the conn dies.
func newRTProvider(t *testing.T, script func(ctx context.Context, c *websocket.Conn, p *rtProvider)) *rtProvider {
	t.Helper()
	p := &rtProvider{closedCh: make(chan websocket.StatusCode, 16)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.headers = append(p.headers, r.Header.Clone())
		p.queries = append(p.queries, r.URL.Query())
		p.accepted++
		p.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if script != nil {
			script(r.Context(), c, p)
			return
		}
		rtEchoScript(r.Context(), c, p)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// rtEchoScript echoes every text frame back until the conn dies, then
// reports the observed close status.
func rtEchoScript(ctx context.Context, c *websocket.Conn, p *rtProvider) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			p.closedCh <- websocket.CloseStatus(err)
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			p.closedCh <- websocket.CloseStatus(err)
			return
		}
	}
}

// rtHoldScript holds the conn open without sending, reporting the close.
func rtHoldScript(ctx context.Context, c *websocket.Conn, p *rtProvider) {
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			p.closedCh <- websocket.CloseStatus(err)
			return
		}
	}
}

func (p *rtProvider) handshakes() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]http.Header, len(p.headers))
	copy(out, p.headers)
	return out
}

func (p *rtProvider) query(i int) url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.queries) {
		return nil
	}
	return p.queries[i]
}

func (p *rtProvider) waitClosed(t *testing.T, within time.Duration) websocket.StatusCode {
	t.Helper()
	select {
	case st := <-p.closedCh:
		return st
	case <-time.After(within):
		t.Fatal("provider conn was not closed within the deadline (orphaned upstream session)")
		return -1
	}
}

// --- deps + server + dial -------------------------------------------------

// realtimeDeps assembles handler deps against the given provider base URL.
func realtimeDeps(t *testing.T, providerURL string, mut ...func(*Deps)) (*Deps, *captureProducer) {
	t.Helper()
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, slog.Default())
	provReg.Freeze()
	prod := &captureProducer{}
	aw := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default()).Start()
	deps := &Deps{
		VKAuth: &rtVKAuth{meta: entitledVKMeta("vk-rt"), hash: "hash-rt"},
		Router: &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID: "p-openai", ProviderName: "openai",
			ModelID: "m-rt", ModelName: "GPT Realtime",
			ProviderModelID: "gpt-realtime-x", AdapterType: "openai",
		}}},
		Resolver:       rtStubResolver{baseURL: providerURL},
		ProviderReg:    provReg,
		Models:         rtStubModels{model: rtPricedModel()},
		UpstreamClient: &http.Client{},
		AuditWriter:    aw,
		Allowlist:      forwardheader.Default(),
		Logger:         slog.Default(),
	}
	for _, m := range mut {
		m(deps)
	}
	return deps, prod
}

// rtServer mounts ServeRealtime on a live test server and returns it plus a
// channel that receives one token per COMPLETED handler invocation — the
// synchronization point for "the session row has been enqueued".
func rtServer(t *testing.T, deps *Deps) (*httptest.Server, chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 32)
	h := NewHandler(deps).ServeRealtime(rtIngress())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r)
		done <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

func waitHandlerDone(t *testing.T, done chan struct{}, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("handler invocation %d/%d did not finish", i+1, n)
		}
	}
}

// rtDial connects a test client through the relay.
func rtDial(t *testing.T, srvURL, model string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/v1/realtime"
	if model != "" {
		wsURL += "?model=" + url.QueryEscape(model)
	}
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
}

// drainAudit closes the writer (flushing) and decodes every captured
// envelope.
func drainAudit(t *testing.T, aw *audit.Writer, prod *captureProducer) []mq.TrafficEventMessage {
	t.Helper()
	aw.Close()
	prod.mu.Lock()
	defer prod.mu.Unlock()
	out := make([]mq.TrafficEventMessage, 0, len(prod.messages))
	for _, raw := range prod.messages {
		var evt mq.TrafficEventMessage
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("unmarshal audit envelope: %v; raw=%s", err, raw)
		}
		out = append(out, evt)
	}
	return out
}

// rawAudit returns the captured raw envelope bytes (for negative substring
// assertions — no audio bytes, no provider key).
func rawAudit(prod *captureProducer) [][]byte {
	prod.mu.Lock()
	defer prod.mu.Unlock()
	out := make([][]byte, len(prod.messages))
	copy(out, prod.messages)
	return out
}

// splitRealtimeRows partitions decoded envelopes into response rows (200)
// and session rows (101/other) for the realtime endpoint type.
func splitRealtimeRows(msgs []mq.TrafficEventMessage) (responses, sessions []mq.TrafficEventMessage) {
	for _, m := range msgs {
		if m.EndpointType != string(typology.EndpointKindRealtime) {
			continue
		}
		if m.StatusCode == http.StatusOK {
			responses = append(responses, m)
		} else {
			sessions = append(sessions, m)
		}
	}
	return responses, sessions
}

// readUntilClose reads client frames until the conn closes, returning the
// received text frames and the close status (-1 for a non-close failure).
func readUntilClose(t *testing.T, c *websocket.Conn, within time.Duration) ([][]byte, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var frames [][]byte
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return frames, websocket.CloseStatus(err)
		}
		frames = append(frames, append([]byte(nil), data...))
	}
}

// rtDoneFrame builds a response.done frame with the given token splits.
func rtDoneFrame(textIn, audioIn, cachedText, cachedAudio, textOut, audioOut int) string {
	return `{"type":"response.done","response":{"usage":{` +
		`"input_token_details":{"text_tokens":` + itoa(textIn+cachedText) +
		`,"audio_tokens":` + itoa(audioIn+cachedAudio) +
		`,"cached_tokens":` + itoa(cachedText+cachedAudio) +
		`,"cached_tokens_details":{"text_tokens":` + itoa(cachedText) +
		`,"audio_tokens":` + itoa(cachedAudio) + `}},` +
		`"output_token_details":{"text_tokens":` + itoa(textOut) +
		`,"audio_tokens":` + itoa(audioOut) + `}}}}`
}

func itoa(v int) string { return strconv.Itoa(v) }
