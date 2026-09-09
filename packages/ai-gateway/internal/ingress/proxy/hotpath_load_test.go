package proxy

// In-process load harness for the non-streaming hot path.
//
// perf-optimization-playbook §1 makes this task #1: an optimization without a
// measurement is a guess, and the per-request numbers this reports — bytes,
// objects, GC rate, pause, latency — are what every proxy perf commit cites.
//
// TWO ARMS, ALWAYS. The apparatus is not free: httptest.NewRequest allocates a
// bufio.Reader per call, which was 15.8% of a profile taken before the control
// arm existed and would have sent the first optimization after a 64 KiB read
// buffer that is not on this path at all. The reported figure is the
// difference, so the harness cannot flatter or slander the gateway.
//
// httptest.NewRecorder rather than a socket: the subject is the gateway's own
// CPU and heap, and a real listener buries that under syscalls.
//
// The audit producer is nil rather than the captureProducer the behaviour
// tests use — that one retains every message, and a million of them measure
// the harness rather than the handler.
//
// Opt-in: NEXUS_PERF_LOAD=1. It runs for 20s and is not a correctness test.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

const hotPathUpstreamBody = `{"id":"chatcmpl-zz","object":"chat.completion","created":1,` +
	`"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant",` +
	`"content":"pong"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`

const hotPathReqBody = `{"model":"gpt-4o","max_tokens":16,` +
	`"messages":[{"role":"user","content":"ping"}]}`

// perfRecorder is an httptest.ResponseRecorder that also satisfies the
// deadline and flush interfaces http.NewResponseController looks for.
//
// Without them the gateway's write-deadline arming takes the unsupported
// branch, and net/http builds a fresh error with fmt.Errorf every time it does
// — 4.1 allocations per request in an early profile, none of which a real
// deployment pays and none of which the control arm subtracts, since a no-op
// handler never arms a deadline. Measuring the gateway on a writer that
// refuses what production accepts measures a branch production does not take.
type perfRecorder struct{ *httptest.ResponseRecorder }

func (perfRecorder) SetWriteDeadline(time.Time) error { return nil }
func (perfRecorder) SetReadDeadline(time.Time) error  { return nil }

// discardRecorder answers like perfRecorder but throws the body away.
//
// A streaming response is tens of kilobytes, and httptest.ResponseRecorder
// accumulates all of it in a bytes.Buffer that doubles as it grows — 84% of the
// byte-allocation profile for the streaming arm, by the recorder rather than by
// the gateway. The control arm cannot subtract it either: the no-op handler
// writes nothing, so this is apparatus that only the measured arm pays.
//
// A deployment writes to a socket. Status and headers are still recorded
// because the harness asserts on them; only the body is discarded.
type discardRecorder struct {
	hdr    http.Header
	Code   int
	nBytes int
}

func (d *discardRecorder) Header() http.Header {
	if d.hdr == nil {
		d.hdr = make(http.Header, 8)
	}
	return d.hdr
}
func (d *discardRecorder) Write(p []byte) (int, error) {
	if d.Code == 0 {
		d.Code = http.StatusOK
	}
	d.nBytes += len(p)
	return len(p), nil
}
func (d *discardRecorder) WriteHeader(c int)              { d.Code = c }
func (d *discardRecorder) Flush()                         {}
func (*discardRecorder) SetWriteDeadline(time.Time) error { return nil }
func (*discardRecorder) SetReadDeadline(time.Time) error  { return nil }

func newRecorder() perfRecorder {
	return perfRecorder{httptest.NewRecorder()}
}

type loadResult struct {
	n           int64
	rps         float64
	p50, p99    float64
	p999, max   float64
	bytesPerReq uint64
	allocsPer   uint64
	gcCycles    uint32
	gcPerSec    float64
	pauseTotMs  float64
	pauseMaxMs  float64
	heapMB      float64
}

// zzDrive runs one arm: the same request-construction and recorder loop, with
// only the handler swapped.
// 对照臂用空 handler,量的就是探针自己的开销 —— 两臂之差才是被测物。
func driveLoad(t *testing.T, h http.HandlerFunc, workers int, dur time.Duration) loadResult {
	return driveLoadWith(t, h, workers, dur, func() *http.Request {
		return freshChatRequest(t, hotPathReqBody)
	})
}

func driveLoadWith(
	t *testing.T,
	h http.HandlerFunc,
	workers int,
	dur time.Duration,
	newReq func() *http.Request,
) loadResult {
	return driveLoadWriter(t, h, workers, dur, newReq, func() (http.ResponseWriter, func() int) {
		w := newRecorder()
		return w, func() int { return w.Code }
	})
}

func driveLoadWriter(
	t *testing.T,
	h http.HandlerFunc,
	workers int,
	dur time.Duration,
	newReq func() *http.Request,
	newW func() (http.ResponseWriter, func() int),
) loadResult {
	t.Helper()
	for range 200 {
		w, _ := newW()
		h(w, newReq())
	}
	runtime.GC()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	var ok, bad atomic.Int64
	var mu sync.Mutex
	lat := make([]float64, 0, 1<<20)
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	t0 := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]float64, 0, 8192)
			for time.Now().Before(deadline) {
				w, code := newW()
				s := time.Now()
				h(w, newReq())
				local = append(local, float64(time.Since(s).Microseconds())/1000)
				if code() == http.StatusOK {
					ok.Add(1)
				} else {
					bad.Add(1)
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(t0)
	runtime.ReadMemStats(&after)

	if bad.Load() > 0 {
		t.Fatalf("%d requests were not 200 — measuring an error path, not the hot path", bad.Load())
	}
	n := ok.Load()
	if n == 0 {
		t.Fatal("no requests completed")
	}
	sort.Float64s(lat)

	var maxPause, sumPause uint64
	for i := before.NumGC; i < after.NumGC; i++ {
		p := after.PauseNs[i%256]
		sumPause += p
		if p > maxPause {
			maxPause = p
		}
	}
	q := func(p float64) float64 { return lat[int(float64(len(lat)-1)*p)] }
	return loadResult{
		n: n, rps: float64(n) / elapsed.Seconds(),
		p50: q(0.50), p99: q(0.99), p999: q(0.999), max: lat[len(lat)-1],
		bytesPerReq: (after.TotalAlloc - before.TotalAlloc) / uint64(n),
		allocsPer:   (after.Mallocs - before.Mallocs) / uint64(n),
		gcCycles:    after.NumGC - before.NumGC,
		gcPerSec:    float64(after.NumGC-before.NumGC) / elapsed.Seconds(),
		pauseTotMs:  float64(sumPause) / 1e6,
		pauseMaxMs:  float64(maxPause) / 1e6,
		heapMB:      float64(after.HeapInuse) / (1 << 20),
	}
}

// benchmarkHookCache builds the hook set the published comparison's
// nexus-hooks-on arm actually runs: pii-scanner, keyword-blocker and
// request-content-safety on the request stage, pii-outbound-scanner and
// response-content-safety on the response stage.
//
// Read from the seed fixture the deployment itself seeds from, rather than
// transcribed here. A transcribed copy drifts, and a drifted copy still
// produces a plausible number — which is the failure this whole harness is
// built to avoid. The count is asserted so a renamed row fails loudly instead
// of quietly measuring four hooks.
func benchmarkHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	const seed = "../../../../../tools/db-migrate/seed/fixtures/HookConfig.json"
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("read hook seed %s: %v", seed, err)
	}
	var all []goHooks.HookConfig
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("parse hook seed: %v", err)
	}
	want := map[string]bool{
		"pii-scanner": true, "keyword-blocker": true, "request-content-safety": true,
		"pii-outbound-scanner": true, "response-content-safety": true,
	}
	on := make([]goHooks.HookConfig, 0, len(want))
	for _, h := range all {
		if want[h.Name] {
			// Seeded disabled; the arm is what enables them.
			h.Enabled = true
			on = append(on, h)
		}
	}
	if len(on) != len(want) {
		t.Fatalf("hook seed yielded %d of the %d hooks the arm declares", len(on), len(want))
	}
	hc := compliance.NewHookConfigCache(
		func(context.Context) ([]goHooks.HookConfig, error) { return on, nil },
		builtins.Registry, 0, slog.Default(),
	)
	if err := hc.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}
	return hc
}

// streamFrames builds an SSE transcript long enough for the checkpoint
// cadence to fire repeatedly. live.go widens the step as the transcript grows
// (max(CheckpointChars, accumulated/8)), so a three-frame stream reaches only
// the mandatory final checkpoint and would measure streaming's fixed overhead
// rather than what accumulates across checkpoints.
func streamFrames() []string {
	const deltas = 200 // ~40 chars each, ~8 KB of transcript
	out := make([]string, 0, deltas+2)
	for i := range deltas {
		_ = i
		out = append(out, `data: {"choices":[{"index":0,"delta":{"content":"the quick brown fox jumps "}}]}`)
	}
	out = append(out,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":9,"completion_tokens":200,"total_tokens":209}}`,
		`data: [DONE]`)
	return out
}

const hotPathStreamReqBody = `{"model":"gpt-4o","stream":true,` +
	`"messages":[{"role":"user","content":"ping"}]}`

func TestHotPathLoad(t *testing.T) {
	if os.Getenv("NEXUS_PERF_LOAD") == "" {
		t.Skip("set NEXUS_PERF_LOAD=1 to run the in-process load harness")
	}
	const workers = 32
	const dur = 10 * time.Second

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(hotPathUpstreamBody))
	}))
	t.Cleanup(upstream.Close)

	// Mirrors the competitor benchmark's configuration: the response cache is
	// OFF and no routing rules are installed there, so the two arms differ in
	// hooks and nothing else. Wiring a cache here would measure a gateway that
	// benchmark never ran.
	depsOff := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), func(d *Deps) {
		d.AuditWriter = audit.NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	})
	// hooks-on: the five hooks the published arm enables, seeded from the same
	// fixture the deployment uses.
	depsOn := makeOpenAIDeps(t, upstream.URL, benchmarkHookCache(t), func(d *Deps) {
		d.AuditWriter = audit.NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	})
	ingress := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	}
	gwOff := NewHandler(depsOff).ServeProxy(ingress)
	gwOn := NewHandler(depsOn).ServeProxy(ingress)
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	// Streaming upstream, shared by both streaming arms.
	frames := streamFrames()
	sseUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(sseUpstream.Close)

	streamIngress := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Stream:     true,
	}
	sDepsOff := makeOpenAIDeps(t, sseUpstream.URL, emptyHookCache(t), func(d *Deps) {
		d.AuditWriter = audit.NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	})
	sDepsOn := makeOpenAIDeps(t, sseUpstream.URL, benchmarkHookCache(t), func(d *Deps) {
		d.AuditWriter = audit.NewWriter(nil, "nexus.event.ai-traffic", nil, slog.Default())
	})
	sgwOff := NewHandler(sDepsOff).ServeProxy(streamIngress)
	sgwOn := NewHandler(sDepsOn).ServeProxy(streamIngress)

	// The streaming control arm. The non-streaming rows have had one from the
	// start; the streaming rows did not, so every B/req they reported carried
	// the whole apparatus — the in-process upstream, the client that talks to
	// it, and the frame copy — with nothing subtracting any of it. Three
	// earlier commits chased exactly this class of inflation on the
	// non-streaming side and stopped at the boundary.
	//
	// It does what a gateway does MINUS the gateway: read the request, call the
	// same SSE upstream, copy the frames out. What remains after subtracting it
	// is the gateway's own work.
	streamNoop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		resp, err := http.Get(sseUpstream.URL)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	})
	newStreamReq := func() *http.Request { return freshChatRequest(t, hotPathStreamReqBody) }

	var soff, son loadResult

	// NEXUS_PERF_ARM pins the run to one arm so a -memprofile taken with it
	// contains that arm and nothing else. Both gateway arms walk ServeProxy, so
	// pprof's -focus cannot separate them after the fact.
	arm := os.Getenv("NEXUS_PERF_ARM")
	var ctrl, off, on loadResult
	if arm == "" || arm == "probe" {
		ctrl = driveLoad(t, noop, workers, dur)
	}
	if arm == "" || arm == "off" {
		off = driveLoad(t, gwOff, workers, dur)
	}
	if arm == "" || arm == "on" {
		on = driveLoad(t, gwOn, workers, dur)
	}
	// Streaming carries far more work per request, so fewer workers keep the
	// box out of saturation while still exercising the checkpoint cadence.
	newDiscard := func() (http.ResponseWriter, func() int) {
		d := &discardRecorder{}
		return d, func() int { return d.Code }
	}
	var sctrl loadResult
	if arm == "" || arm == "stream-probe" {
		sctrl = driveLoadWriter(t, streamNoop, workers/4, dur, newStreamReq, newDiscard)
	}
	if arm == "" || arm == "stream-off" {
		soff = driveLoadWriter(t, sgwOff, workers/4, dur, newStreamReq, newDiscard)
	}
	if arm == "" || arm == "stream-on" {
		son = driveLoadWriter(t, sgwOn, workers/4, dur, newStreamReq, newDiscard)
	}

	row := func(name string, r loadResult) {
		fmt.Printf("  %-18s %8.0f %8.3f %8.3f %8.3f %9.3f %9d %7d %7.1f %8.3f %7.1f\n",
			name, r.rps, r.p50, r.p99, r.p999, r.max,
			r.bytesPerReq, r.allocsPer, r.gcPerSec, r.pauseMaxMs, r.heapMB)
	}
	fmt.Printf("\n=== hot path, in-process (%d workers x %v per arm) ===\n", workers, dur)
	fmt.Printf("  %-18s %8s %8s %8s %8s %9s %9s %7s %7s %8s %7s\n",
		"arm", "rps", "p50ms", "p99ms", "p99.9", "maxms", "B/req", "obj/req", "gc/s", "pauseMax", "heapMB")
	if ctrl.n > 0 {
		row("probe only", ctrl)
	}
	if off.n > 0 {
		row("hooks off", off)
	}
	if on.n > 0 {
		row("hooks on", on)
	}
	if sctrl.n > 0 {
		row("stream probe only", sctrl)
	}
	if soff.n > 0 {
		row("stream hooks off", soff)
	}
	if son.n > 0 {
		row("stream hooks on", son)
	}
	delta := func(name string, r loadResult) {
		fmt.Printf("  %-18s %8s %8.3f %8.3f %8.3f %9.3f %9d %7d %7.1f %8s %7s\n",
			name, "", r.p50-ctrl.p50, r.p99-ctrl.p99, r.p999-ctrl.p999, r.max-ctrl.max,
			int64(r.bytesPerReq)-int64(ctrl.bytesPerReq),
			int64(r.allocsPer)-int64(ctrl.allocsPer),
			r.gcPerSec-ctrl.gcPerSec, "", "")
	}
	if arm == "" {
		delta("off - probe", off)
		delta("on - probe", on)
	}
	// The streaming deltas subtract the streaming control, not the
	// non-streaming one: the two arms run different work at different
	// concurrency, so crossing them would produce a number that describes
	// neither.
	sdelta := func(name string, r loadResult) {
		fmt.Printf("  %-18s %8s %8.3f %8.3f %8.3f %9.3f %9d %7d %7.1f %8s %7s\n",
			name, "", r.p50-sctrl.p50, r.p99-sctrl.p99, r.p999-sctrl.p999, r.max-sctrl.max,
			int64(r.bytesPerReq)-int64(sctrl.bytesPerReq),
			int64(r.allocsPer)-int64(sctrl.allocsPer),
			r.gcPerSec-sctrl.gcPerSec, "", "")
	}
	if arm == "" {
		sdelta("stream off - probe", soff)
		sdelta("stream on - probe", son)
	}
	if arm == "" {
		fmt.Printf("  %-18s %8s %8s %8s %8s %9s %9d %7d\n",
			"hooks cost", "", "", "", "", "",
			int64(on.bytesPerReq)-int64(off.bytesPerReq),
			int64(on.allocsPer)-int64(off.allocsPer))
	}
	fmt.Println()
}
