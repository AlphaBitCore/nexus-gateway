package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

type fakeConfirmTool struct{}

func (fakeConfirmTool) Name() string            { return "mitigate_kill_switch" }
func (fakeConfirmTool) Description() string     { return "engage/disengage the kill switch" }
func (fakeConfirmTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (fakeConfirmTool) Tier() agent.Tier        { return agent.TierConfirm }
func (fakeConfirmTool) Run(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{Content: "engaged"}, nil
}

// fakeImpactTool is a confirm-tier tool that also implements agent.ImpactDetailer,
// so makeConfirm's FR-22 preview path can be exercised. impact/err are the canned
// ImpactDetail return.
type fakeImpactTool struct {
	impact any
	err    error
	delay  time.Duration // when >0, ImpactDetail blocks for delay (or until ctx cancels)
}

func (fakeImpactTool) Name() string            { return "mitigate_kill_switch" }
func (fakeImpactTool) Description() string     { return "kill switch" }
func (fakeImpactTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (fakeImpactTool) Tier() agent.Tier        { return agent.TierConfirm }
func (fakeImpactTool) Run(context.Context, json.RawMessage) (agent.Result, error) {
	return agent.Result{Content: "ok"}, nil
}
func (f fakeImpactTool) ImpactDetail(ctx context.Context, _ json.RawMessage) (any, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.impact, f.err
}

// captureConfirmEvent runs makeConfirm for one tool and returns the emitted confirm
// SSE payload (then immediately denies so the goroutine unwinds).
func captureConfirmEvent(t *testing.T, h *Handler, tool agent.Tool) map[string]any {
	t.Helper()
	var mu sync.Mutex
	var captured map[string]any
	send := func(event string, payload any) {
		if event == "confirm" {
			mu.Lock()
			captured = payload.(map[string]any)
			mu.Unlock()
		}
	}
	cf := h.makeConfirm("u", "sess", send)
	done := make(chan struct{})
	go func() { _, _ = cf(context.Background(), tool, json.RawMessage(`{"engage":true}`), "r"); close(done) }()
	var got map[string]any
	for range 200 {
		mu.Lock()
		c := captured
		mu.Unlock()
		if c != nil {
			got = c
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("no confirm event emitted")
	}
	// Unwind the parked goroutine.
	h.confirms.decide("u:sess:"+got["callId"].(string), false, "")
	<-done
	return got
}

// TestMakeConfirm_AttachesImpactPreview covers FR-22/AC-6: a high-blast-radius tool
// (ImpactDetailer) surfaces its structured preview in the confirm event so the card
// can show it before Allow.
func TestMakeConfirm_AttachesImpactPreview(t *testing.T) {
	h := New(Config{})
	preview := map[string]any{"action": "engage", "summary": "halts the fleet"}
	got := captureConfirmEvent(t, h, fakeImpactTool{impact: preview})
	gotPreview, ok := got["preview"].(map[string]any)
	if !ok {
		t.Fatalf("confirm event must carry a preview for a high-blast tool, got %v", got["preview"])
	}
	if gotPreview["summary"] != "halts the fleet" {
		t.Errorf("preview summary = %v, want 'halts the fleet'", gotPreview["summary"])
	}
}

// TestMakeConfirm_PreviewFailsOpen covers the emergency-tool safety rule: if the
// impact read errors, the card still appears with an "unavailable" note rather than
// the preview being omitted (which would let a UI gate block an emergency mitigation).
func TestMakeConfirm_PreviewFailsOpen(t *testing.T) {
	h := New(Config{})
	got := captureConfirmEvent(t, h, fakeImpactTool{err: errTest})
	p, ok := got["preview"].(map[string]any)
	if !ok || p["unavailable"] != true {
		t.Fatalf("a failed impact read must fail open to an unavailable preview, got %v", got["preview"])
	}
}

// TestMakeConfirm_NoPreviewForOrdinaryTool: a confirm tool that is not an
// ImpactDetailer emits no preview field — ordinary confirm cards are unchanged.
func TestMakeConfirm_NoPreviewForOrdinaryTool(t *testing.T) {
	h := New(Config{})
	got := captureConfirmEvent(t, h, fakeConfirmTool{})
	if _, ok := got["preview"]; ok {
		t.Fatalf("an ordinary confirm tool must not carry a preview, got %v", got["preview"])
	}
}

// TestMakeConfirm_NoPreviewWhenImpactNil: a tool that implements ImpactDetailer but
// returns (nil, nil) for this input also emits no preview field.
func TestMakeConfirm_NoPreviewWhenImpactNil(t *testing.T) {
	h := New(Config{})
	got := captureConfirmEvent(t, h, fakeImpactTool{impact: nil})
	if _, ok := got["preview"]; ok {
		t.Fatalf("a nil ImpactDetail result must not attach a preview, got %v", got["preview"])
	}
}

// TestMakeConfirm_PreviewTimeoutFailsOpen proves the impactTimeout deadline itself
// fires: a slow impact read (longer than impactTimeout) is cut off and surfaces the
// fail-open "unavailable" marker rather than delaying or blocking the confirm card.
func TestMakeConfirm_PreviewTimeoutFailsOpen(t *testing.T) {
	h := New(Config{})
	h.impactTimeout = time.Millisecond
	got := captureConfirmEvent(t, h, fakeImpactTool{impact: map[string]any{"action": "engage"}, delay: time.Second})
	p, ok := got["preview"].(map[string]any)
	if !ok || p["unavailable"] != true {
		t.Fatalf("a slow impact read past impactTimeout must fail open to unavailable, got %v", got["preview"])
	}
}

var errTest = errorString("impact read boom")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestConfirmRegistry(t *testing.T) {
	r := newConfirmRegistry()
	ch := r.register("s:c1", false)
	go func() { r.decide("s:c1", true, "") }()
	select {
	case ok := <-ch:
		if !ok {
			t.Fatal("registered key must receive the resolved decision")
		}
	case <-time.After(time.Second):
		t.Fatal("decide did not deliver")
	}
	// Second decide of the same key is a no-op (consumed once → no double-execute).
	if o, _ := r.decide("s:c1", true, ""); o != confirmNotFound {
		t.Fatalf("a key may only resolve once, got %v", o)
	}
	// Unknown key never resolves.
	if o, _ := r.decide("nope", true, ""); o != confirmNotFound {
		t.Fatalf("unknown key must not resolve, got %v", o)
	}
	// drop removes a pending key.
	r.register("s:c2", false)
	r.drop("s:c2")
	if o, _ := r.decide("s:c2", true, ""); o != confirmNotFound {
		t.Fatalf("dropped key must not resolve, got %v", o)
	}
}

// Under genuine concurrent contention (many POST /confirm racing the same key),
// exactly one resolve wins and the decision is delivered once — never twice (no
// double-execute of a write).
func TestConfirmRegistryResolveOnceUnderContention(t *testing.T) {
	r := newConfirmRegistry()
	ch := r.register("k", false)
	const n = 32
	var wins int32
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if o, _ := r.decide("k", true, ""); o == confirmResolved {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&wins) != 1 {
		t.Fatalf("exactly one concurrent resolve must win (once-only), got %d", wins)
	}
	if !<-ch {
		t.Fatal("the single winning decision must be delivered")
	}
}

// makeConfirm emits a confirm event and parks; a registry resolve returns the
// decision (the two-phase web confirm: SSE event out, POST /confirm in).
func TestMakeConfirmTwoPhaseApprove(t *testing.T) {
	h := New(Config{})
	var mu sync.Mutex
	var captured map[string]any
	send := func(event string, payload any) {
		if event == "confirm" {
			mu.Lock()
			captured = payload.(map[string]any)
			mu.Unlock()
		}
	}
	cf := h.makeConfirm("u", "sess", send)
	result := make(chan bool, 1)
	go func() {
		ok, _ := cf(context.Background(), fakeConfirmTool{}, json.RawMessage(`{"engage":true}`), "engage kill switch")
		result <- ok
	}()

	var callID string
	for range 200 {
		mu.Lock()
		c := captured
		mu.Unlock()
		if c != nil {
			callID = c["callId"].(string)
			if c["tool"] != "mitigate_kill_switch" {
				t.Fatalf("confirm event must carry the tool name, got %v", c["tool"])
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if callID == "" {
		t.Fatal("no confirm event emitted")
	}
	if o, _ := h.confirms.decide("u:sess:"+callID, true, ""); o != confirmResolved {
		t.Fatalf("decide must resolve the parked confirm, got %v", o)
	}
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("an approved confirm must return true (tool runs)")
		}
	case <-time.After(time.Second):
		t.Fatal("confirm did not resolve after approval")
	}
}

func TestConfirmRegistryTryDrop(t *testing.T) {
	r := newConfirmRegistry()
	r.register("k", false)
	if !r.tryDrop("k") {
		t.Fatal("tryDrop of a present key must report true")
	}
	if r.tryDrop("k") {
		t.Fatal("tryDrop of an absent key must report false")
	}
}

// With no decision delivered, the confirm times out to a fail-safe deny.
func TestMakeConfirmTimeoutDenies(t *testing.T) {
	h := New(Config{})
	h.confirmTimeout = 30 * time.Millisecond
	ok, err := h.makeConfirm("u", "sess", func(string, any) {})(
		context.Background(), fakeConfirmTool{}, json.RawMessage(`{}`), "r")
	if ok || err != nil {
		t.Fatalf("an unanswered confirm must time out to deny, got ok=%v err=%v", ok, err)
	}
}

// A cancelled turn context must deny a parked confirm without leaking the goroutine.
func TestMakeConfirmCtxCancelDenies(t *testing.T) {
	h := New(Config{})
	cf := h.makeConfirm("u", "sess", func(string, any) {})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() {
		ok, _ := cf(ctx, fakeConfirmTool{}, json.RawMessage(`{}`), "r")
		result <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("a cancelled turn must deny the confirm (fail-safe)")
		}
	case <-time.After(time.Second):
		t.Fatal("confirm leaked: did not unwind on ctx cancel")
	}
}

func TestConfirmEndpoint(t *testing.T) {
	h := New(Config{})
	e := echo.New()

	// No pending confirm → 409 (a stale/duplicate decision cannot execute anything).
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"sessionId":"s","callId":"c","decision":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := h.Confirm(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown confirm must be 409, got %d", rec.Code)
	}

	// Missing fields → 400.
	req2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"decision":true}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	if err := h.Confirm(e.NewContext(req2, rec2)); err != nil {
		t.Fatal(err)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing sessionId/callId must be 400, got %d", rec2.Code)
	}

	// A pending confirm resolves only for its OWNING user (key bound to userId).
	ch := h.confirms.register("u1:s:c1", false)

	// A different principal must NOT resolve it → 409 (cross-user confirm hijack blocked).
	reqX := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"sessionId":"s","callId":"c1","decision":true}`))
	reqX.Header.Set("Content-Type", "application/json")
	recX := httptest.NewRecorder()
	cX := e.NewContext(reqX, recX)
	cX.Set("adminAuth", &auth.AdminAuth{KeyID: "u2"})
	if err := h.Confirm(cX); err != nil {
		t.Fatal(err)
	}
	if recX.Code != http.StatusConflict {
		t.Fatalf("a different user must not resolve the confirm, got %d", recX.Code)
	}

	// The owner → 200 and the decision is delivered.
	req3 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"sessionId":"s","callId":"c1","decision":true}`))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	c3 := e.NewContext(req3, rec3)
	c3.Set("adminAuth", &auth.AdminAuth{KeyID: "u1"})
	if err := h.Confirm(c3); err != nil {
		t.Fatal(err)
	}
	if rec3.Code != http.StatusOK {
		t.Fatalf("resolving a pending confirm must be 200, got %d", rec3.Code)
	}
	if ok := <-ch; !ok {
		t.Fatal("the delivered decision must be the posted one (true)")
	}
}

// postConfirm drives one POST /api/admin/assistant/confirm for userID and returns the
// HTTP status + decoded JSON body. challengeToken is omitted when empty.
func postConfirm(t *testing.T, h *Handler, userID, sessionID, callID string, decision bool, token string) (int, map[string]any) {
	t.Helper()
	e := echo.New()
	body := map[string]any{"sessionId": sessionID, "callId": callID, "decision": decision}
	if token != "" {
		body["challengeToken"] = token
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	if err := h.Confirm(c); err != nil {
		t.Fatalf("Confirm handler error: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestConfirm_ProdSecondConfirmRequired is the FR-9 happy path: on a prod instance a
// single Allow does NOT execute — it mints a one-time challenge token and keeps the
// write parked; a second Allow echoing that token executes it.
func TestConfirm_ProdSecondConfirmRequired(t *testing.T) {
	h := New(Config{IsProd: true})
	ch := h.confirms.register("u1:s:c1", true) // prod entry

	// First Allow: 200 + secondConfirmRequired + a token; the write must NOT resolve.
	code, body := postConfirm(t, h, "u1", "s", "c1", true, "")
	if code != http.StatusOK {
		t.Fatalf("first prod Allow must be 200, got %d", code)
	}
	if body["secondConfirmRequired"] != true {
		t.Fatalf("first prod Allow must require a second confirm, got %v", body["secondConfirmRequired"])
	}
	token, _ := body["challengeToken"].(string)
	if token == "" {
		t.Fatal("first prod Allow must return a non-empty challenge token")
	}
	select {
	case <-ch:
		t.Fatal("a single prod Allow must NOT unblock the write (it is only a cosmetic-defeating first step)")
	case <-time.After(30 * time.Millisecond):
		// good — still parked
	}

	// Second Allow with the token: 200 and the write executes (true delivered).
	code2, _ := postConfirm(t, h, "u1", "s", "c1", true, token)
	if code2 != http.StatusOK {
		t.Fatalf("second prod Allow with the token must be 200, got %d", code2)
	}
	select {
	case ok := <-ch:
		if !ok {
			t.Fatal("the second prod Allow must deliver true (execute)")
		}
	case <-time.After(time.Second):
		t.Fatal("second prod Allow did not unblock the write")
	}
}

// TestConfirm_ProdRejectsForgedToken proves the gate is backend-enforced: a second
// Allow with a wrong (or empty) token is a 409 and never executes the write.
func TestConfirm_ProdRejectsForgedToken(t *testing.T) {
	h := New(Config{IsProd: true})
	ch := h.confirms.register("u1:s:c1", true)

	// First Allow mints the real token.
	if code, _ := postConfirm(t, h, "u1", "s", "c1", true, ""); code != http.StatusOK {
		t.Fatalf("first prod Allow must be 200, got %d", code)
	}
	// Second Allow with a forged token → 409 invalid_challenge, no execution.
	code, _ := postConfirm(t, h, "u1", "s", "c1", true, "deadbeefdeadbeefdeadbeefdeadbeef")
	if code != http.StatusConflict {
		t.Fatalf("a forged second-confirm token must be 409, got %d", code)
	}
	select {
	case <-ch:
		t.Fatal("a forged token must NOT execute the prod write")
	case <-time.After(30 * time.Millisecond):
		// good — still parked, the real token can still complete it
	}
}

// TestConfirm_ProdDenyEndsImmediately confirms Deny short-circuits the two-phase
// flow: no challenge round is required to abort a prod write.
func TestConfirm_ProdDenyEndsImmediately(t *testing.T) {
	h := New(Config{IsProd: true})
	ch := h.confirms.register("u1:s:c1", true)

	code, body := postConfirm(t, h, "u1", "s", "c1", false, "")
	if code != http.StatusOK {
		t.Fatalf("prod Deny must be 200, got %d", code)
	}
	if body["secondConfirmRequired"] == true {
		t.Fatal("Deny must not ask for a second confirm")
	}
	select {
	case ok := <-ch:
		if ok {
			t.Fatal("Deny must deliver false (abort), not execute")
		}
	case <-time.After(time.Second):
		t.Fatal("prod Deny did not unblock the turn")
	}
}

// TestConfirm_NonProdSingleAllowExecutes pins that staging is unchanged: one Allow
// (no token) executes, no second confirm.
func TestConfirm_NonProdSingleAllowExecutes(t *testing.T) {
	h := New(Config{IsProd: false})
	ch := h.confirms.register("u1:s:c1", false)

	code, body := postConfirm(t, h, "u1", "s", "c1", true, "")
	if code != http.StatusOK {
		t.Fatalf("non-prod Allow must be 200, got %d", code)
	}
	if body["secondConfirmRequired"] == true {
		t.Fatal("non-prod must NOT require a second confirm")
	}
	if ok := <-ch; !ok {
		t.Fatal("non-prod single Allow must execute (true)")
	}
}

// TestMakeConfirm_ProdEndToEnd exercises the kernel ConfirmFunc through the full
// prod two-phase flow: makeConfirm parks, the first Allow keeps it parked, the second
// Allow returns (true, nil) so the tool runs.
func TestMakeConfirm_ProdEndToEnd(t *testing.T) {
	h := New(Config{IsProd: true})
	var mu sync.Mutex
	var captured map[string]any
	send := func(event string, payload any) {
		if event == "confirm" {
			mu.Lock()
			captured = payload.(map[string]any)
			mu.Unlock()
		}
	}
	cf := h.makeConfirm("u", "sess", send)
	result := make(chan bool, 1)
	go func() {
		ok, _ := cf(context.Background(), fakeConfirmTool{}, json.RawMessage(`{"engage":true}`), "engage kill switch")
		result <- ok
	}()

	var callID string
	for range 200 {
		mu.Lock()
		c := captured
		mu.Unlock()
		if c != nil {
			callID = c["callId"].(string)
			if c["prod"] != true {
				t.Fatalf("prod confirm event must carry prod=true, got %v", c["prod"])
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if callID == "" {
		t.Fatal("no confirm event emitted")
	}

	// First Allow → challenge, tool stays parked.
	code, body := postConfirm(t, h, "u", "sess", callID, true, "")
	if code != http.StatusOK || body["secondConfirmRequired"] != true {
		t.Fatalf("first prod Allow must require a second confirm; code=%d body=%v", code, body)
	}
	token := body["challengeToken"].(string)
	select {
	case <-result:
		t.Fatal("the tool ran after only one prod Allow")
	case <-time.After(30 * time.Millisecond):
	}

	// Second Allow → tool runs.
	if code2, _ := postConfirm(t, h, "u", "sess", callID, true, token); code2 != http.StatusOK {
		t.Fatalf("second prod Allow must be 200, got %d", code2)
	}
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("the second prod Allow must let the tool run (true)")
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not run after the second prod Allow")
	}
}
