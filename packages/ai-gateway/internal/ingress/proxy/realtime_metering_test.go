package proxy

// realtime_metering_test.go — AC-3 (per-response rows + six-component cost +
// server-minted trace grouping + quota counters), AC-7 (unpriced honesty),
// AC-8 (post-settle over-limit sever), and AC-9 (fresh-Check-per-response
// mechanics via the fake quota seam).

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func approxUsd(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Compile-time pins: the PRODUCTION types satisfy the relay's seams. A
// signature drift would otherwise silently disable the VK recheck (the
// optional-interface assertion would just fail) or break quota settlement.
var (
	_ vkStatusRechecker = (*vkauth.Authenticator)(nil)
	_ realtimeQuota     = (*quota.Engine)(nil)
)

// TestRealtimeMetering_TwoResponseRows is AC-3 end-to-end: two
// response.done events with distinct usage yield two rows with distinct
// fresh ids, correct six-component cost (including the cached-audio
// NULL-fallback to the primary audio-in rate), UpstreamTotalMs==LatencyMs,
// a shared SERVER-minted trace id (≠ the client-supplied request id), a $0
// session row with LatencyMs=1, and quota counters advanced by the summed
// cost.
func TestRealtimeMetering_TwoResponseRows(t *testing.T) {
	// usage A: 800 uncached text-in, 1900 uncached audio-in, 200 cached
	// text, 100 cached audio, 500 text-out, 1500 audio-out.
	// prices: in $4, out $16, cachedText $0.40, audioIn $32, audioOut $64,
	// cachedAudio NULL → falls back to $32.
	frameA := rtDoneFrame(800, 1900, 200, 100, 500, 1500)
	wantCostA := (800*4.0+1900*32.0)/1e6 + (200*0.4+100*32.0)/1e6 + (500*16.0+1500*64.0)/1e6
	frameB := rtDoneFrame(10000, 0, 0, 0, 2000, 0)
	wantCostB := 10000*4.0/1e6 + 2000*16.0/1e6

	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, p *rtProvider) {
		for _, f := range []string{
			`{"type":"response.created"}`, frameA,
			`{"type":"response.created"}`, frameB,
		} {
			if err := c.Write(ctx, websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}
		// Provider-side close AFTER the frames: the provider pump meters
		// doneB before it observes this close, making row emission
		// deterministic.
		_ = c.Close(websocket.StatusNormalClosure, "script done")
	})

	engine, uc := videoQuotaHarness(t, 1_000_000)
	deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
		d.QuotaEngine = engine
		d.VKAuth = &rtVKAuth{meta: entitledVKMeta("vk-1"), hash: "h"} // usage keyed on vk-1
	})
	srv, done := rtServer(t, deps)

	hdr := http.Header{}
	hdr.Set("X-Nexus-Request-Id", "rid-client-123")
	c, _, err := rtDial(t, srv.URL, "gpt-realtime", hdr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, _ := readUntilClose(t, c, 10*time.Second)
	if len(frames) < 4 {
		t.Fatalf("client received %d frames, want the 4 relayed events", len(frames))
	}
	waitHandlerDone(t, done, 1)

	responses, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(responses) != 2 || len(sessions) != 1 {
		t.Fatalf("rows = %d responses + %d sessions, want 2 + 1", len(responses), len(sessions))
	}

	rA, rB := responses[0], responses[1]
	if !approxUsd(rA.EstimatedCostUsd, wantCostA) {
		t.Errorf("row A cost = %.9f, want %.9f", rA.EstimatedCostUsd, wantCostA)
	}
	if !approxUsd(rB.EstimatedCostUsd, wantCostB) {
		t.Errorf("row B cost = %.9f, want %.9f", rB.EstimatedCostUsd, wantCostB)
	}
	if rA.PromptTokens != 3000 || rA.CompletionTokens != 2000 || rA.TotalTokens != 5000 {
		t.Errorf("row A tokens = %d/%d/%d, want 3000/2000/5000 (aggregate incl. cached)",
			rA.PromptTokens, rA.CompletionTokens, rA.TotalTokens)
	}
	if rA.ID == "" || rA.ID == rB.ID {
		t.Errorf("response row ids must be distinct fresh UUIDs: %q vs %q", rA.ID, rB.ID)
	}
	if rA.ID == "rid-client-123" || rB.ID == "rid-client-123" {
		t.Error("response row minted its id from the client request id")
	}
	for _, r := range responses {
		if r.LatencyMs < 1 {
			t.Errorf("response LatencyMs = %d, want >= 1 (created→done wall clock)", r.LatencyMs)
		}
		if r.UpstreamTotalMs == nil || *r.UpstreamTotalMs != r.LatencyMs {
			t.Errorf("UpstreamTotalMs = %v, want == LatencyMs %d (exchange ran entirely upstream)",
				r.UpstreamTotalMs, r.LatencyMs)
		}
		if r.ProviderName != "openai" || r.ModelID != "m-rt" {
			t.Errorf("row provider/model = %q/%q", r.ProviderName, r.ModelID)
		}
	}

	sess := sessions[0]
	// The primary key is minted per row. It is the hub's ON CONFLICT (id)
	// idempotency key, so it must not be the client's X-Nexus-Request-Id —
	// a client reusing one across calls would collapse two events into one
	// row — nor the session trace, which every row in the session shares.
	if sess.ID == "" || sess.ID == "rid-client-123" || sess.ID == sess.TraceID {
		t.Errorf("session row id = %q, want a minted id distinct from the client header and the session trace", sess.ID)
	}
	if rA.ID == rB.ID || rA.ID == sess.ID {
		t.Errorf("rows share an id (%q / %q / %q); ON CONFLICT (id) DO NOTHING would drop all but one",
			sess.ID, rA.ID, rB.ID)
	}
	if sess.TraceID == "" || sess.TraceID == "rid-client-123" {
		t.Errorf("session trace id %q must be server-minted, never the client header", sess.TraceID)
	}
	if rA.TraceID != sess.TraceID || rB.TraceID != sess.TraceID {
		t.Errorf("trace ids differ: %q / %q / %q — all rows must share the session UUID",
			rA.TraceID, rB.TraceID, sess.TraceID)
	}
	if sess.EstimatedCostUsd != 0 || sess.PromptTokens != 0 || sess.TotalTokens != 0 {
		t.Errorf("session row must be $0/zero-token (cost lives on response rows): %+v", sess)
	}
	if sess.LatencyMs != 1 {
		t.Errorf("session row LatencyMs = %d, want the explicit floor 1", sess.LatencyMs)
	}

	// Quota counters advanced by the summed cost (17 + 7 whole cents after
	// the milli-cent carry: 17128 → 17c carry 128; 7200+128 → 7c carry 328).
	if cents := vkUsageCents(t, uc); cents != 24 {
		t.Errorf("vk usage = %d cents, want 24 booked from the two responses", cents)
	}
}

// TestRealtimeMetering_MissingUsageSkipsRow: a response.done without a usage
// object forwards but is not metered (deduped WARN; fail-open on metering,
// never on relay). The response still counts toward the session row.
func TestRealtimeMetering_MissingUsageSkipsRow(t *testing.T) {
	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, _ *rtProvider) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created"}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.done","response":{}}`))
		_ = c.Close(websocket.StatusNormalClosure, "done")
	})
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, _ := readUntilClose(t, c, 5*time.Second)
	var sawDone bool
	for _, f := range frames {
		if strings.Contains(string(f), "response.done") {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("the un-metered response.done frame must still be relayed")
	}
	waitHandlerDone(t, done, 1)

	responses, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(responses) != 0 {
		t.Errorf("response rows = %d, want 0 (no usage → no row)", len(responses))
	}
	if len(sessions) != 1 {
		t.Fatalf("session rows = %d, want 1", len(sessions))
	}
}

// TestRealtimeMetering_MissingAudioRateHonesty is AC-7: an UN-enforced VK on
// a model missing its primary audio rates still relays, the audio component
// prices $0 (visible in the row), and the gap is WARNed (deduped).
func TestRealtimeMetering_MissingAudioRateHonesty(t *testing.T) {
	m := rtPricedModel()
	m.ID = "m-rt-noaudio"
	m.AudioInputPricePM = nil
	m.AudioOutputPricePM = nil

	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, _ *rtProvider) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created"}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(rtDoneFrame(1000, 5000, 0, 0, 100, 5000)))
		_ = c.Close(websocket.StatusNormalClosure, "done")
	})

	var logBuf bytes.Buffer
	deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
		d.Models = rtStubModels{model: m}
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID: "p-openai", ProviderName: "openai",
			ModelID: "m-rt-noaudio", ModelName: "GPT Realtime NA",
			ProviderModelID: "gpt-realtime-x", AdapterType: "openai",
		}}}
		meta := entitledVKMeta("vk-na")
		meta.AllowedModels[0].ModelID = "m-rt-noaudio"
		d.VKAuth = &rtVKAuth{meta: meta, hash: "h"}
		d.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	})
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readUntilCloseDiscard(t, c, 5*time.Second)
	waitHandlerDone(t, done, 1)

	responses, _ := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(responses) != 1 {
		t.Fatalf("response rows = %d, want 1", len(responses))
	}
	// Text components only: 1000×$4/M + 100×$16/M; both audio components $0.
	want := 1000*4.0/1e6 + 100*16.0/1e6
	if !approxUsd(responses[0].EstimatedCostUsd, want) {
		t.Errorf("cost = %.9f, want %.9f (audio components $0)", responses[0].EstimatedCostUsd, want)
	}
	if !strings.Contains(logBuf.String(), "realtime pricing incomplete") {
		t.Error("missing-audio-rate WARN was not logged")
	}
}

func readUntilCloseDiscard(t *testing.T, c *websocket.Conn, within time.Duration) {
	t.Helper()
	_, _ = readUntilClose(t, c, within)
}

// TestRealtimeQuotaSever is AC-8 end-to-end with the real engine: the
// response that crosses a reject-mode cap is FORWARDED and SETTLED, then the
// session is severed with a terminal error event + close 1008. The at-limit
// arm proves the documented admission boundary: equality admits at upgrade
// and severs after the first response.
func TestRealtimeQuotaSever(t *testing.T) {
	for _, preload := range []int64{90, 100} {
		name := "crossing mid-session"
		if preload == 100 {
			name = "at-limit VK severs after first response"
		}
		t.Run(name, func(t *testing.T) {
			// One response of 10 000 uncached audio-in tokens at $32/M =
			// $0.32 = 32 cents against a 100-cent reject cap.
			provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, p *rtProvider) {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(rtDoneFrame(0, 10000, 0, 0, 0, 0)))
				rtHoldScript(ctx, c, p) // wait for the relay's sever
			})
			engine, uc := videoQuotaHarness(t, 100)
			uc.SetUsageForTest("virtual_key", "vk-1", quota.CurrentPeriodKey("monthly"), preload)
			deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
				d.QuotaEngine = engine
				d.VKAuth = &rtVKAuth{meta: entitledVKMeta("vk-1"), hash: "h"}
			})
			srv, done := rtServer(t, deps)

			c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
			if err != nil {
				t.Fatalf("dial: %v (at-limit must ADMIT at upgrade)", err)
			}
			frames, closeStatus := readUntilClose(t, c, 10*time.Second)
			if closeStatus != websocket.StatusPolicyViolation {
				t.Errorf("close status = %v, want 1008", closeStatus)
			}
			var sawDone, sawSeverEvent bool
			for _, f := range frames {
				if strings.Contains(string(f), "response.done") {
					sawDone = true
				}
				if strings.Contains(string(f), "REALTIME_QUOTA_EXCEEDED") {
					sawSeverEvent = true
				}
			}
			if !sawDone {
				t.Error("the crossing response.done must be forwarded before the sever")
			}
			if !sawSeverEvent {
				t.Error("no terminal quota error event before the close")
			}
			if st := provider.waitClosed(t, 5*time.Second); st != websocket.StatusPolicyViolation {
				t.Errorf("provider close status = %v, want 1008", st)
			}
			waitHandlerDone(t, done, 1)

			// The crossing response was settled: counters advanced past the cap.
			if cents := vkUsageCents(t, uc); cents != preload+32 {
				t.Errorf("usage = %d cents, want %d (crossing response settled)", cents, preload+32)
			}
			responses, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
			if len(responses) != 1 {
				t.Errorf("response rows = %d, want 1", len(responses))
			}
			if len(sessions) != 1 || sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_QUOTA_EXCEEDED" {
				t.Fatalf("session rows = %+v, want one with REALTIME_QUOTA_EXCEEDED", sessions)
			}
		})
	}
}

// --- fresh-check mechanics via the fake quota seam (AC-9) -----------------

// fakeRTQuota records the Check/Reconcile interleaving. Reconcile must
// receive the SAME decision pointer the immediately-preceding Check
// produced — the fresh-decision contract that makes period rollovers book
// into the new period's keys.
type fakeRTQuota struct {
	mu         sync.Mutex
	levels     []quota.CheckLevel
	checks     int
	last       *quota.Decision
	reconciles []struct {
		decision *quota.Decision
		wasLast  bool
		cost     float64
	}
}

func (f *fakeRTQuota) OrgParents() map[string]string { return nil }

func (f *fakeRTQuota) Check(_ context.Context, _ []quota.CheckLevel, estimate quota.CostEstimate, _ *vkauth.VKMeta) *quota.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if estimate.EstimatedCost() != 0 {
		panic("realtime per-response Check must use a zero estimate")
	}
	f.last = &quota.Decision{Allowed: true, Action: "allow",
		Levels: append([]quota.CheckLevel(nil), f.levels...)}
	return f.last
}

func (f *fakeRTQuota) Reconcile(_ context.Context, decision *quota.Decision, actual quota.ActualUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconciles = append(f.reconciles, struct {
		decision *quota.Decision
		wasLast  bool
		cost     float64
	}{decision, decision == f.last, actual.CostUSD})
}

// newMeterTestSession builds a minimal session for direct metering-unit
// calls (no live conns).
func newMeterTestSession(t *testing.T, q realtimeQuota) (*realtimeSession, *captureProducer) {
	t.Helper()
	deps, prod := realtimeDeps(t, "http://127.0.0.1:0")
	s := &realtimeSession{
		h:      NewHandler(deps),
		rec:    &audit.Record{RequestID: "rid-upgrade-1", Method: http.MethodGet, Path: "/v1/realtime"},
		in:     rtIngress(),
		vkMeta: entitledVKMeta("vk-1"),
		quota:  q,
		target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai",
			ModelID: "m-rt", ModelName: "GPT Realtime", ProviderModelID: "gpt-realtime-x",
		},
		callTarget: provcore.CallTarget{ProviderID: "p-openai", ProviderName: "openai"},
		priceModel: rtPricedModel(),
		prices:     realtimeModelPrices(rtPricedModel()),
		sessionID:  "sess-uuid-1",
		state:      &realtimeproxy.SessionState{},
	}
	s.ctx = context.Background()
	return s, prod
}

// TestRealtimeSettleQuota_FreshCheckPerResponse is AC-9's testable core: a
// FRESH Check precedes every Reconcile, and each Reconcile books against
// exactly that fresh decision (whose period keys the engine computes at
// call time — so a rollover between responses lands in the new period).
func TestRealtimeSettleQuota_FreshCheckPerResponse(t *testing.T) {
	fq := &fakeRTQuota{}
	s, _ := newMeterTestSession(t, fq)

	frame := []byte(rtDoneFrame(1000, 0, 0, 0, 100, 0))
	if s.meterResponseDone(frame, time.Now()) {
		t.Fatal("unexpected sever with no limits")
	}
	if s.meterResponseDone(frame, time.Now()) {
		t.Fatal("unexpected sever with no limits")
	}

	fq.mu.Lock()
	defer fq.mu.Unlock()
	if fq.checks != 2 || len(fq.reconciles) != 2 {
		t.Fatalf("checks=%d reconciles=%d, want 2/2 (one fresh Check per response)", fq.checks, len(fq.reconciles))
	}
	wantCost := 1000*4.0/1e6 + 100*16.0/1e6
	for i, r := range fq.reconciles {
		if !r.wasLast {
			t.Errorf("reconcile %d did not receive the immediately-preceding fresh decision", i)
		}
		if !approxUsd(r.cost, wantCost) {
			t.Errorf("reconcile %d cost = %.9f, want %.9f", i, r.cost, wantCost)
		}
	}
	if fq.reconciles[0].decision == fq.reconciles[1].decision {
		t.Error("the two responses shared one decision — Check is not fresh per response")
	}
}

// TestRealtimeSettleQuota_SeverArms pins the LOCAL post-settle evaluation:
// only a reject-mode level whose pre-settle counter plus the row's cost
// strictly exceeds its limit severs.
func TestRealtimeSettleQuota_SeverArms(t *testing.T) {
	frame := []byte(rtDoneFrame(0, 10000, 0, 0, 0, 0)) // 32 cents
	lvl := func(mode string, current, limit int64) quota.CheckLevel {
		return quota.CheckLevel{TargetType: "virtual_key", TargetID: "vk-1",
			HasLimit: true, CurrentCents: current, LimitCents: limit, EnforcementMode: mode}
	}
	cases := []struct {
		name   string
		levels []quota.CheckLevel
		want   bool
	}{
		{"reject level crossed", []quota.CheckLevel{lvl("reject", 90, 100)}, true},
		{"reject at exactly limit after row", []quota.CheckLevel{lvl("reject", 68, 100)}, false},
		{"reject with headroom", []quota.CheckLevel{lvl("reject", 10, 100)}, false},
		// downgrade has no live-session remedy (a running voice session cannot
		// be swapped to a cheaper model), so a crossed downgrade cap severs
		// exactly like reject.
		{"downgrade level crossed severs", []quota.CheckLevel{lvl("downgrade", 90, 100)}, true},
		{"downgrade with headroom", []quota.CheckLevel{lvl("downgrade", 10, 100)}, false},
		{"notify mode never severs", []quota.CheckLevel{lvl("notify-and-proceed", 90, 100)}, false},
		{"track-only never severs", []quota.CheckLevel{lvl("track-only", 90, 100)}, false},
		{"unstamped level ignored", []quota.CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}, false},
		{"org-level reject crossed", []quota.CheckLevel{lvl("notify-and-proceed", 0, 1000),
			{TargetType: "organization", TargetID: "org-1", HasLimit: true,
				CurrentCents: 95, LimitCents: 100, EnforcementMode: "reject"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := &fakeRTQuota{levels: tc.levels}
			s, _ := newMeterTestSession(t, fq)
			if got := s.meterResponseDone(frame, time.Now()); got != tc.want {
				t.Errorf("sever = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("nil quota engine fails open", func(t *testing.T) {
		s, _ := newMeterTestSession(t, nil)
		s.quota = nil
		if s.meterResponseDone(frame, time.Now()) {
			t.Error("sever without a quota engine")
		}
	})
	t.Run("nil fresh decision fails open", func(t *testing.T) {
		s, _ := newMeterTestSession(t, nilDecisionQuota{})
		if s.meterResponseDone(frame, time.Now()) {
			t.Error("sever on a nil decision")
		}
	})
}

type nilDecisionQuota struct{}

func (nilDecisionQuota) OrgParents() map[string]string { return nil }
func (nilDecisionQuota) Check(_ context.Context, _ []quota.CheckLevel, _ quota.CostEstimate, _ *vkauth.VKMeta) *quota.Decision {
	return nil
}
func (nilDecisionQuota) Reconcile(_ context.Context, _ *quota.Decision, _ quota.ActualUsage) {
	panic("Reconcile must not run when Check returned nil")
}

// TestRealtimeMeterResponseDone_NoUsageNoQuotaCalls: a usage-less done is
// counted for the session but produces no row and touches no quota.
func TestRealtimeMeterResponseDone_NoUsageNoQuotaCalls(t *testing.T) {
	fq := &fakeRTQuota{}
	s, prod := newMeterTestSession(t, fq)
	if s.meterResponseDone([]byte(`{"type":"response.done","response":{}}`), time.Now()) {
		t.Fatal("sever on an un-metered response")
	}
	responses, _, _ := s.state.Snapshot()
	if responses != 1 {
		t.Errorf("session response count = %d, want 1 (counted even when un-metered)", responses)
	}
	if fq.checks != 0 {
		t.Errorf("quota checks = %d, want 0", fq.checks)
	}
	s.h.deps.AuditWriter.Close()
	if n := len(rawAudit(prod)); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

// rtMetricsRecorder captures RecordRequest calls for the per-response
// Prometheus assertion.
type rtMetricsRecorder struct {
	mu    sync.Mutex
	calls []struct {
		provider, model, endpoint string
		status                    int
		usage                     metrics.Usage
	}
}

func (r *rtMetricsRecorder) RecordRequest(provider, model, endpoint string, status int, _ time.Duration, usage metrics.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		provider, model, endpoint string
		status                    int
		usage                     metrics.Usage
	}{provider, model, endpoint, status, usage})
}
func (r *rtMetricsRecorder) RecordHookRequest(_, _, _ string)                       {}
func (r *rtMetricsRecorder) RecordTrafficExtract(_, _, _ string)                    {}
func (r *rtMetricsRecorder) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (r *rtMetricsRecorder) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}
func (r *rtMetricsRecorder) RecordError(_, _ string)                                {}

// TestRealtimeMetering_RecordsPrometheus: every metered response drives the
// existing RecordRequest instrument with the realtime endpoint label.
func TestRealtimeMetering_RecordsPrometheus(t *testing.T) {
	mr := &rtMetricsRecorder{}
	fq := &fakeRTQuota{}
	s, _ := newMeterTestSession(t, fq)
	s.h.deps.Metrics = mr

	if s.meterResponseDone([]byte(rtDoneFrame(1000, 2000, 0, 0, 100, 300)), time.Now()) {
		t.Fatal("unexpected sever")
	}
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if len(mr.calls) != 1 {
		t.Fatalf("RecordRequest calls = %d, want 1", len(mr.calls))
	}
	c := mr.calls[0]
	if c.provider != "openai" || c.model != "m-rt" || c.endpoint != "realtime" || c.status != 200 {
		t.Errorf("labels = %s/%s/%s/%d, want openai/m-rt/realtime/200", c.provider, c.model, c.endpoint, c.status)
	}
	if c.usage.PromptTokens != 3000 || c.usage.CompletionTokens != 400 || c.usage.TotalTokens != 3400 {
		t.Errorf("usage = %+v, want 3000/400/3400", c.usage)
	}
}

// TestRealtimeTapError_RecordsProviderCode: an error event is recorded on
// the session accounting (code only, never message text) and surfaces on
// the snapshot when no close reason masks it.
func TestRealtimeTapError_RecordsProviderCode(t *testing.T) {
	s, _ := newMeterTestSession(t, nil)
	s.tapServerEvent([]byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"secret user text must not persist"}}`))
	_, _, code := s.state.Snapshot()
	if code != "rate_limit_exceeded" {
		t.Fatalf("session error code = %q, want the provider's machine code", code)
	}
	// response.created via the tap starts the wall clock: a following done
	// yields a >= 1 ms latency (the tap wiring, not just the state unit).
	s.tapServerEvent([]byte(`{"type":"response.created"}`))
	if sever := s.meterResponseDone([]byte(rtDoneFrame(1, 0, 0, 0, 1, 0)), time.Now()); sever {
		t.Fatal("unexpected sever")
	}
}
