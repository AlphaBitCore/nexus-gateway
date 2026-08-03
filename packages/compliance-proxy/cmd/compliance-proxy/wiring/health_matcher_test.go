package wiring

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// The content-scanning engine is a BUILD-TAG choice with an order-of-magnitude
// latency consequence on large bodies, so a running proxy has to be able to say
// which one it got. These two tests pin the trap that shape of wiring falls into:
// the source is registered, but the value is never populated, so it answers with
// a zero struct and reads as "engine: (blank)".
func TestDebugRuntime_ServesTheCompiledMatcherEngine(t *testing.T) {
	d := buildHealthDeps(t)
	d.MatcherEngine = matcher.DescribeEngine()
	mux, _ := InitHealthHandler(d)

	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer "+d.ServiceToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/runtime status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got, err := matcherSourceFrom(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("read policy.matcher: %v", err)
	}
	if !got.OK {
		t.Error("policy.matcher source reported not-ok")
	}
	want := matcher.DescribeEngine()
	if got.Value.Name != want.Name {
		t.Errorf("engine name = %q, want %q", got.Value.Name, want.Name)
	}
	if got.Value.Name == "" {
		t.Error("engine name is empty — the source is registered but nothing populates it")
	}
	if got.Value.Effect == "" {
		t.Error("engine effect is empty — an operator reading this learns nothing")
	}
	if got.Value.SinglePass != want.SinglePass {
		t.Errorf("engine properties = %+v, want %+v", got.Value, want)
	}
}

// matcherSourceFrom pulls just the policy.matcher entry out of a /debug/runtime
// snapshot. Every source carries a differently-shaped `value` (arrays, maps,
// structs), so the snapshot is decoded loosely and only this one entry is typed.
func matcherSourceFrom(body []byte) (struct {
	OK    bool           `json:"ok"`
	Value matcher.Engine `json:"value"`
}, error) {
	var typed struct {
		OK    bool           `json:"ok"`
		Value matcher.Engine `json:"value"`
	}
	var snap struct {
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return typed, fmt.Errorf("decode snapshot: %w", err)
	}
	raw, ok := snap.Sources["policy.matcher"]
	if !ok {
		return typed, fmt.Errorf("no policy.matcher source; have %v", keysOf(snap.Sources))
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		return typed, fmt.Errorf("decode policy.matcher: %w", err)
	}
	return typed, nil
}

// A caller that forgets to populate MatcherEngine must be visible as such, not
// silently reported as an unnamed engine. This is the negative half: the source
// answers, and what it answers is empty, which is exactly what the assertion
// above would catch in production wiring.
func TestDebugRuntime_UnpopulatedMatcherEngineIsEmpty(t *testing.T) {
	d := buildHealthDeps(t) // MatcherEngine deliberately left zero
	mux, _ := InitHealthHandler(d)

	req := httptest.NewRequest(http.MethodGet, "/debug/runtime", nil)
	req.Header.Set("Authorization", "Bearer "+d.ServiceToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	got, err := matcherSourceFrom(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("read policy.matcher: %v", err)
	}
	if name := got.Value.Name; name != "" {
		t.Errorf("zero deps reported engine %q — the test fixture no longer proves the population step", name)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
