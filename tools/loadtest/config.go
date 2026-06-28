package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"
)

// ---------- config schema ----------

type Defaults struct {
	Protocol  string            `json:"protocol"`
	Target    string            `json:"target"`
	Headers   map[string]string `json:"headers"`
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Stream    bool              `json:"stream"`
	System    string            `json:"system"`
}

type Correlation struct {
	UUIDInPrompt bool   `json:"uuid_in_prompt"`
	Header       string `json:"header"`
}

type Thresholds struct {
	TTFTp95Ms float64 `json:"ttft_p95_ms"`
	P95Ms     float64 `json:"p95_ms"`
	ErrorRate float64 `json:"error_rate"`
}

type Content struct {
	Mode            string   `json:"mode"` // pool | scripted | sized
	Prompts         []string `json:"prompts"`
	Script          []string `json:"script"`
	ApproxInputToks int      `json:"approx_input_tokens"`
}

type Scenario struct {
	Name      string            `json:"name"`
	Weight    int               `json:"weight"`
	Protocol  string            `json:"protocol"`
	Target    string            `json:"target"`
	Headers   map[string]string `json:"headers"`
	Model     string            `json:"model"`
	System    string            `json:"system"`
	Turns     TurnSpec          `json:"turns"`
	Stream    *bool             `json:"stream"`
	MaxTokens int               `json:"max_tokens"`
	Content   Content           `json:"content"`

	proto Protocol // resolved
}

type Stage struct {
	Concurrency int    `json:"concurrency"`
	Duration    string `json:"duration"`
	dur         time.Duration
}

type Config struct {
	Defaults         Defaults    `json:"defaults"`
	Stages           []Stage     `json:"stages"`
	Warmup           string      `json:"warmup"`
	CacheMode        string      `json:"cache_mode"` // bust (default) | shared-prefix | natural
	Correlation      Correlation `json:"correlation"`
	Thresholds       Thresholds  `json:"thresholds"`
	Scenarios        []Scenario  `json:"scenarios"`
	DisableHTTP2     bool        `json:"disable_http2"`
	DisableKeepAlive bool        `json:"disable_keepalive"`
	Insecure         bool        `json:"insecure_skip_verify"` // skip TLS cert verification — for gateways behind a self-signed cert (e.g. an appliance on a bare IP)
	Timeout          string      `json:"timeout"`
	ThinkTime        string      `json:"think_time"`
	CaptureErr       bool        `json:"capture_error_body"`

	warmup    time.Duration
	timeout   time.Duration
	thinkTime time.Duration
}

// TurnSpec accepts either `5` or `{"min":3,"max":6}`.
type TurnSpec struct{ Min, Max int }

func (t *TurnSpec) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		t.Min, t.Max = n, n
		return nil
	}
	var o struct{ Min, Max int }
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("turns must be an int or {min,max}: %w", err)
	}
	t.Min, t.Max = o.Min, o.Max
	return nil
}

func (t TurnSpec) pick(r *rand.Rand) int {
	if t.Min <= 0 && t.Max <= 0 {
		return 1
	}
	if t.Max <= t.Min {
		return t.Min
	}
	return t.Min + r.Intn(t.Max-t.Min+1)
}

// LoadConfig reads + validates a profile and resolves defaults/protocols.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{CacheMode: "bust", Timeout: "120s", ThinkTime: "0s", Warmup: "0s"}
	cfg.Correlation = Correlation{UUIDInPrompt: true, Header: "x-request-id"}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg.finalize()
}

func (cfg *Config) finalize() (*Config, error) {
	var err error
	if cfg.Defaults.Protocol == "" {
		cfg.Defaults.Protocol = "openai-chat"
	}
	if cfg.Defaults.MaxTokens == 0 {
		cfg.Defaults.MaxTokens = 64
	}
	if cfg.warmup, err = time.ParseDuration(cfg.Warmup); err != nil {
		return nil, fmt.Errorf("warmup: %w", err)
	}
	if cfg.timeout, err = time.ParseDuration(cfg.Timeout); err != nil {
		return nil, fmt.Errorf("timeout: %w", err)
	}
	if cfg.thinkTime, err = time.ParseDuration(cfg.ThinkTime); err != nil {
		return nil, fmt.Errorf("think_time: %w", err)
	}
	for i := range cfg.Stages {
		if cfg.Stages[i].dur, err = time.ParseDuration(cfg.Stages[i].Duration); err != nil {
			return nil, fmt.Errorf("stage %d duration: %w", i, err)
		}
	}
	// No scenarios → one implicit scenario from defaults (simple use).
	if len(cfg.Scenarios) == 0 {
		cfg.Scenarios = []Scenario{{Name: "default", Weight: 1, Content: Content{Mode: "pool"}}}
	}
	for i := range cfg.Scenarios {
		s := &cfg.Scenarios[i]
		if s.Weight <= 0 {
			s.Weight = 1
		}
		if s.Protocol == "" {
			s.Protocol = cfg.Defaults.Protocol
		}
		if s.Target == "" {
			s.Target = cfg.Defaults.Target
		}
		if s.Model == "" {
			s.Model = cfg.Defaults.Model
		}
		if s.System == "" {
			s.System = cfg.Defaults.System
		}
		if s.MaxTokens == 0 {
			s.MaxTokens = cfg.Defaults.MaxTokens
		}
		if s.Stream == nil {
			st := cfg.Defaults.Stream
			s.Stream = &st
		}
		if s.Content.Mode == "" {
			s.Content.Mode = "pool"
		}
		if s.Content.Mode == "pool" && len(s.Content.Prompts) == 0 {
			s.Content.Prompts = []string{"In one short sentence, explain a randomly chosen idea."}
		}
		// merge headers: scenario overrides defaults
		merged := map[string]string{}
		for k, v := range cfg.Defaults.Headers {
			merged[k] = v
		}
		for k, v := range s.Headers {
			merged[k] = v
		}
		s.Headers = merged
		if s.proto, err = GetProtocol(s.Protocol); err != nil {
			return nil, fmt.Errorf("scenario %q: %w", s.Name, err)
		}
		if s.Target == "" {
			return nil, fmt.Errorf("scenario %q: no target (set defaults.target or scenario.target)", s.Name)
		}
	}
	if len(cfg.Stages) == 0 {
		return nil, fmt.Errorf("no stages")
	}
	return cfg, nil
}

// userContent returns the user message text for a given turn of a scenario.
// (The conversation-level cache-bust UUID is injected by the engine.)
func (s *Scenario) userContent(turnIdx int, r *rand.Rand) string {
	switch s.Content.Mode {
	case "scripted":
		if len(s.Content.Script) == 0 {
			return "continue"
		}
		return s.Content.Script[turnIdx%len(s.Content.Script)]
	case "sized":
		return sizedPrompt(s.Content.ApproxInputToks, r)
	default: // pool
		if len(s.Content.Prompts) == 0 {
			return "hello"
		}
		return s.Content.Prompts[r.Intn(len(s.Content.Prompts))]
	}
}

// sizedPrompt generates ~approxToks tokens of filler (~4 chars/token) followed
// by a question, to exercise a target input size.
func sizedPrompt(approxToks int, r *rand.Rand) string {
	if approxToks <= 0 {
		approxToks = 256
	}
	words := []string{"system", "latency", "throughput", "token", "cache", "stream",
		"gateway", "request", "model", "context", "budget", "concurrency", "scale", "quota"}
	targetChars := approxToks * 4
	var sb strings.Builder
	sb.WriteString("Context: ")
	for sb.Len() < targetChars {
		sb.WriteString(words[r.Intn(len(words))])
		sb.WriteByte(' ')
	}
	sb.WriteString(". Summarize the above in one sentence.")
	return sb.String()
}

// allocate maps each of `total` VU slots to a scenario index, proportional to
// weights (largest-remainder), so weight = share of the concurrency pool.
func allocate(total int, scenarios []Scenario) []int {
	if total <= 0 {
		return nil
	}
	sumW := 0
	for _, s := range scenarios {
		sumW += s.Weight
	}
	counts := make([]int, len(scenarios))
	assigned := 0
	type rem struct {
		idx int
		r   float64
	}
	var rems []rem
	for i, s := range scenarios {
		exact := float64(total) * float64(s.Weight) / float64(sumW)
		counts[i] = int(exact)
		assigned += counts[i]
		rems = append(rems, rem{i, exact - float64(counts[i])})
	}
	// distribute the remainder to the largest fractional parts
	for assigned < total {
		best, bestR := -1, -1.0
		for _, rr := range rems {
			if counts[rr.idx] >= 0 && rr.r > bestR {
				bestR, best = rr.r, rr.idx
			}
		}
		if best < 0 {
			break
		}
		counts[best]++
		assigned++
		for i := range rems {
			if rems[i].idx == best {
				rems[i].r = -1 // consume once
			}
		}
	}
	// ensure every scenario gets at least 1 if total >= len (so a scenario is never silent)
	out := make([]int, 0, total)
	for i, c := range counts {
		for j := 0; j < c; j++ {
			out = append(out, i)
		}
	}
	for len(out) < total {
		out = append(out, 0)
	}
	return out[:total]
}
