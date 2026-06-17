// loadtest — a scenario-driven stress tester for LLM gateways.
// See DESIGN.md for the full design. Build/run (own module, outside go.work):
//
//	cd tools/loadtest && GOWORK=off go run . -config profiles/ai-gateway.json -out runs/
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------- per-turn record (one JSONL line; crash-safe durable log) ----------

type record struct {
	Stage      int     `json:"stage"`
	Conc       int     `json:"concurrency"`
	Scenario   string  `json:"scenario"`
	Protocol   string  `json:"protocol"`
	ConvUUID   string  `json:"conv_uuid"`
	Turn       int     `json:"turn"`
	Turns      int     `json:"turns_total"`
	Stream     bool    `json:"stream"`
	StartMs    int64   `json:"start_unix_ms"`
	LatMs      float64 `json:"latency_ms"`
	TTFTMs     float64 `json:"ttft_ms"`
	Status     int     `json:"status"`
	PromptTok  int     `json:"prompt_tokens"`
	CompTok    int     `json:"completion_tokens"`
	ContentLen int     `json:"content_len"`
	Warmup     bool    `json:"warmup"`
	Err        string  `json:"err,omitempty"`
	Body       string  `json:"err_body,omitempty"`
}

func main() {
	cfgPath := flag.String("config", "", "path to JSON profile (see profiles/, DESIGN.md)")
	outDir := flag.String("out", "runs", "output directory")
	target := flag.String("target", "", "override defaults.target")
	vk := flag.String("vk", "", "convenience: sets defaults Authorization: Bearer <vk> on all scenarios")
	modelOv := flag.String("model", "", "override defaults.model (and scenarios inheriting it); e.g. a gateway that needs a 'provider/model' form")
	stagesS := flag.String("stages", "", "override stages, e.g. '1:10s,100:60s,1000:120s'")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification (self-signed gateways, e.g. an appliance on a bare IP)")
	flag.Parse()
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "error: -config is required (see tools/loadtest/profiles/)")
		os.Exit(2)
	}
	cfg, err := LoadConfig(*cfgPath)
	must(err, "load config")
	if *target != "" {
		for i := range cfg.Scenarios {
			if cfg.Scenarios[i].Target == cfg.Defaults.Target || cfg.Scenarios[i].Target == "" {
				cfg.Scenarios[i].Target = *target
			}
		}
		cfg.Defaults.Target = *target
	}
	if *vk != "" {
		for i := range cfg.Scenarios {
			cfg.Scenarios[i].Headers["Authorization"] = "Bearer " + *vk
		}
	}
	if *modelOv != "" {
		for i := range cfg.Scenarios {
			if cfg.Scenarios[i].Model == cfg.Defaults.Model || cfg.Scenarios[i].Model == "" {
				cfg.Scenarios[i].Model = *modelOv
			}
		}
		cfg.Defaults.Model = *modelOv
	}
	if *stagesS != "" {
		cfg.Stages = parseStagesFlag(*stagesS)
		_, err = cfg.finalize()
		must(err, "re-finalize after -stages")
	}
	if *insecure {
		cfg.Insecure = true
	}
	for i := range cfg.Scenarios {
		if _, ok := cfg.Scenarios[i].Headers["Content-Type"]; !ok {
			cfg.Scenarios[i].Headers["Content-Type"] = "application/json"
		}
	}

	maxConc := 0
	for _, s := range cfg.Stages {
		if s.Concurrency > maxConc {
			maxConc = s.Concurrency
		}
	}

	// --- 1k+ hardening: raise the generator's own FD limit ---
	fdHave := raiseFD(uint64(maxConc) + 256)
	fdWarn := ""
	if fdHave < uint64(maxConc)+128 {
		fdWarn = fmt.Sprintf("FD limit %d < needed ~%d — raise ulimit -n on this host", fdHave, maxConc+128)
		fmt.Fprintln(os.Stderr, "WARN: "+fdWarn)
	}

	must(os.MkdirAll(*outDir, 0o755), "mkdir out")
	stamp := time.Now().UTC().Format("20060102T150405Z")
	resultsPath := filepath.Join(*outDir, "results-"+stamp+".jsonl")
	rf, err := os.Create(resultsPath)
	must(err, "create results")
	defer rf.Close()
	bw := bufio.NewWriterSize(rf, 1<<20)

	// Sink: large buffer, batched flush. Decoupled from metrics — a VU never
	// blocks on it; overflow is counted (never silently lost in the stats,
	// which live in the in-memory aggregator).
	resCh := make(chan record, 65536)
	var sinkDropped int64
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		enc := json.NewEncoder(bw)
		tk := time.NewTicker(250 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case r, ok := <-resCh:
				if !ok {
					bw.Flush()
					return
				}
				_ = enc.Encode(&r)
			case <-tk.C:
				bw.Flush()
			}
		}
	}()
	emit := func(r record) {
		select {
		case resCh <- r:
		default:
			atomic.AddInt64(&sinkDropped, 1)
		}
	}

	client := buildClient(cfg, maxConc)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("loadtest → %d scenario(s), %d stage(s), peak conc %d, FD %d\n", len(cfg.Scenarios), len(cfg.Stages), maxConc, fdHave)
	fmt.Printf("results → %s\n\n", resultsPath)
	fmt.Printf("%-5s %-6s %-7s %8s %7s %6s | %9s %9s %9s | %9s %9s\n",
		"stage", "conc", "dur", "reqs", "rps", "ok%", "lat_p50", "lat_p95", "lat_p99", "ttft_p50", "ttft_p95")

	var stages []stageStat
	for i, st := range cfg.Stages {
		ss := runStage(ctx, client, cfg, i+1, st, emit)
		stages = append(stages, ss)
		fmt.Printf("%-5d %-6d %-7s %8d %7.1f %5.1f%% | %9.0f %9.0f %9.0f | %9.0f %9.0f\n",
			ss.Stage, ss.Concurrency, st.Duration, ss.Total.Requests, ss.Total.Throughput,
			100*float64(ss.Total.OK)/maxF(float64(ss.Total.Requests), 1),
			ss.Total.Lat.P50, ss.Total.Lat.P95, ss.Total.Lat.P99, ss.Total.TTFT.P50, ss.Total.TTFT.P95)
		if ctx.Err() != nil {
			fmt.Println("\n[interrupted — writing partial report]")
			break
		}
	}

	close(resCh)
	writerWG.Wait()
	writeReports(*outDir, stamp, cfg, stages, resultsPath, atomic.LoadInt64(&sinkDropped), fdHave, fdWarn)
}

func buildClient(cfg *Config, maxConc int) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        maxConc + 64,
		MaxIdleConnsPerHost: maxConc + 64,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		WriteBufferSize:     64 << 10,
		ReadBufferSize:      64 << 10,
		ForceAttemptHTTP2:   !cfg.DisableHTTP2,
		DisableKeepAlives:   cfg.DisableKeepAlive,
	}
	if cfg.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Timeout: cfg.timeout, Transport: tr}
}

// runStage holds st.Concurrency VUs (split across scenarios by weight) running
// conversations back-to-back until the stage deadline. Warmup-window turns are
// recorded to JSONL but excluded from steady-state stats.
//
// Graceful stop: the stage deadline (launchCtx) gates only the START of new
// conversations and turns — it does NOT cancel in-flight HTTP requests. Each
// request runs under the base ctx (signal-only) bounded by the client Timeout,
// so a request already on the wire at the boundary completes naturally instead
// of being hard-cancelled. Hard-cancelling in-flight requests makes the gateway
// see a client disconnect mid-upstream and record a 502 PROVIDER_UNAVAILABLE —
// a failure the load generator manufactured, not the server. With graceful stop
// the only cancellation is a real interrupt (Ctrl-C/SIGTERM); genuine slow
// requests still surface as honest `timeout` failures via the client Timeout.
func runStage(ctx context.Context, client *http.Client, cfg *Config, stageNo int, st Stage, emit func(record)) stageStat {
	start := time.Now()
	deadline := start.Add(st.dur)
	warmUntil := start.Add(cfg.warmup)
	launchCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	assign := allocate(st.Concurrency, cfg.Scenarios)
	samplers := make([]*sampler, len(cfg.Scenarios))
	for i := range samplers {
		samplers[i] = newSampler()
	}

	var wg sync.WaitGroup
	for w := 0; w < st.Concurrency; w++ {
		scIdx := assign[w]
		wg.Add(1)
		go func(scIdx, seed int) {
			defer wg.Done()
			r := mrand.New(mrand.NewSource(int64(seed)*2654435761 + time.Now().UnixNano()))
			sc := &cfg.Scenarios[scIdx]
			smp := samplers[scIdx]
			for launchCtx.Err() == nil {
				runConversation(launchCtx, ctx, client, cfg, sc, stageNo, st.Concurrency, r, smp, warmUntil, emit)
			}
		}(scIdx, w)
	}
	wg.Wait()

	// Roll up per-scenario + stage total (steady-state samples only).
	ss := stageStat{Stage: stageNo, Concurrency: st.Concurrency, DurationS: st.dur.Seconds()}
	steady := st.dur.Seconds() - cfg.warmup.Seconds()
	if steady <= 0 {
		steady = st.dur.Seconds()
	}
	merged := newSampler()
	for i := range cfg.Scenarios {
		st2 := samplers[i].rollup(cfg.Scenarios[i].Name, steady, &cfg.Thresholds)
		ss.Scenarios = append(ss.Scenarios, st2)
		samplers[i].mergeInto(merged)
	}
	ss.Total = merged.rollup("ALL", steady, &cfg.Thresholds)
	return ss
}

// runConversation runs one virtual user's conversation: inject the per-conv
// UUID (cache-bust + trace + server-join key), then 1..turns turns carrying the
// assistant reply forward.
// launchCtx gates starting a new turn (stage deadline); reqCtx (base, signal-
// only) carries the HTTP request so an in-flight turn finishes after the
// boundary instead of being hard-cancelled into a manufactured 502.
func runConversation(launchCtx, reqCtx context.Context, client *http.Client, cfg *Config, sc *Scenario, stageNo, conc int, r *mrand.Rand, smp *sampler, warmUntil time.Time, emit func(record)) {
	uuid := randHex(16)
	turns := sc.Turns.pick(r)
	stream := *sc.Stream
	var msgs []Msg
	for t := 1; t <= turns; t++ {
		if launchCtx.Err() != nil {
			return
		}
		content := sc.userContent(t-1, r)
		if t == 1 && cfg.CacheMode == "bust" && cfg.Correlation.UUIDInPrompt {
			content = "[" + uuid + "] " + content // UUID at the FRONT → cache miss + threads the conversation
		}
		msgs = append(msgs, Msg{Role: "user", Content: content})

		headers := sc.Headers
		if cfg.Correlation.Header != "" {
			headers = cloneWith(sc.Headers, cfg.Correlation.Header, uuid)
		}
		conv := Conversation{Model: sc.Model, System: sc.System, Msgs: msgs, MaxTokens: sc.MaxTokens, Stream: stream}

		startT := time.Now()
		turn, ttft, status, errBody, err := doTurn(reqCtx, client, sc.proto, sc.Target, headers, conv)
		lat := msSince(startT)
		if err != nil && reqCtx.Err() != nil {
			return // true global interrupt (Ctrl-C/SIGTERM); not real load
		}
		warm := startT.Before(warmUntil)
		ok := err == nil && status == 200

		rec := record{Stage: stageNo, Conc: conc, Scenario: sc.Name, Protocol: sc.Protocol,
			ConvUUID: uuid, Turn: t, Turns: turns, Stream: stream, StartMs: startT.UnixMilli(),
			LatMs: lat, TTFTMs: ttft, Status: status, PromptTok: turn.PromptTokens,
			CompTok: turn.CompletionTokens, ContentLen: len(turn.Content), Warmup: warm}
		if err != nil {
			rec.Err = err.Error()
		} else if !ok && cfg.CaptureErr {
			rec.Body = errBody
		}
		emit(rec)
		if !warm {
			smp.add(lat, ttft, status, turn.PromptTokens, turn.CompletionTokens, ok, err)
		}
		if !ok {
			return // failed turn ends the conversation
		}
		msgs = append(msgs, Msg{Role: "assistant", Content: turn.Content})
		if cfg.thinkTime > 0 && t < turns {
			select {
			case <-time.After(cfg.thinkTime):
			case <-launchCtx.Done():
				return
			case <-reqCtx.Done():
				return
			}
		}
	}
}

// errEmptyCompletion marks a turn that returned HTTP 200 but carried no usable
// payload — no content AND zero completion tokens. The gateway can answer 200
// while the upstream silently produced nothing (broken stream, empty choice,
// degraded fallback); treating that as success would hide exactly the
// silent-failure class this tool exists to catch, so it is a failure.
var errEmptyCompletion = errors.New("empty completion body")

// doTurn sends one request, measuring TTFT via httptrace; parsing is delegated
// to the protocol adapter (stream vs non-stream is the only branch, and it is
// protocol-agnostic). A 200 is only a success if it also parses to a non-empty
// completion — both the HTTP status AND the returned data must be valid.
func doTurn(ctx context.Context, client *http.Client, p Protocol, target string, headers map[string]string, conv Conversation) (Turn, float64, int, string, error) {
	body, err := p.BuildBody(conv)
	if err != nil {
		return Turn{}, 0, 0, "", err
	}
	start := time.Now()
	var ttfb time.Duration
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Since(start) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "POST", target, strings.NewReader(string(body)))
	if err != nil {
		return Turn{}, 0, 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Turn{}, 0, 0, "", err
	}
	defer resp.Body.Close()
	ttft := float64(ttfb.Microseconds()) / 1000.0
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		_, _ = io.Copy(io.Discard, resp.Body)
		return Turn{}, ttft, resp.StatusCode, string(b), nil
	}
	var turn Turn
	if conv.Stream {
		turn, err = p.ParseStream(resp.Body)
	} else {
		var raw []byte
		raw, err = io.ReadAll(resp.Body)
		if err == nil {
			turn, err = p.ParseNonStream(raw)
		}
	}
	// Body-content validation: a 200 that parsed cleanly but yielded nothing
	// is a silent failure, not a success.
	if err == nil && strings.TrimSpace(turn.Content) == "" && turn.CompletionTokens == 0 {
		return turn, ttft, resp.StatusCode, "", errEmptyCompletion
	}
	return turn, ttft, resp.StatusCode, "", err
}

// ---------- helpers ----------

func raiseFD(need uint64) uint64 {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) != nil {
		return 0
	}
	if lim.Cur < need {
		want := need
		if want > lim.Max {
			want = lim.Max
		}
		lim.Cur = want
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
		_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
	return lim.Cur
}

func cloneWith(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

func parseStagesFlag(s string) []Stage {
	var out []Stage
	for _, part := range strings.Split(s, ",") {
		c, d, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		conc, err := strconv.Atoi(strings.TrimSpace(c))
		must(err, "parse -stages")
		out = append(out, Stage{Concurrency: conc, Duration: strings.TrimSpace(d)})
	}
	return out
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", ctx, err)
		os.Exit(1)
	}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
