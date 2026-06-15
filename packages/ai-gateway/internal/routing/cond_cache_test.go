package routing

import (
	"encoding/json"
	"sync"
	"testing"
)

// TestParseMatchConditions_MemoizesByContent proves the F-6 cache: identical
// conditions bytes parse once and return the SAME pointer thereafter, distinct
// bytes return distinct results, and malformed JSON is cached as nil (callers
// treat nil as "no match"). This is the per-request hot path — ruleMatches runs
// it for every rule on every request.
func TestParseMatchConditions_MemoizesByContent(t *testing.T) {
	r := &Resolver{}

	a := json.RawMessage(`{"models":["gpt-4o"]}`)
	first := r.parseMatchConditions(a)
	if first == nil {
		t.Fatal("valid conditions parsed to nil")
	}
	// Same content (a fresh slice with identical bytes) must hit the cache and
	// return the exact same pointer — proving it was not re-unmarshaled.
	second := r.parseMatchConditions(json.RawMessage(`{"models":["gpt-4o"]}`))
	if second != first {
		t.Errorf("identical conditions should return the cached pointer; got a fresh parse")
	}
	if len(first.Models) != 1 || first.Models[0] != "gpt-4o" {
		t.Errorf("parsed conditions wrong: %+v", first.Models)
	}

	// Distinct content → distinct parse.
	other := r.parseMatchConditions(json.RawMessage(`{"providers":["openai"]}`))
	if other == first {
		t.Error("distinct conditions must not collide onto the same cached value")
	}

	// Malformed → nil, and the nil is cached (second call still nil, no re-parse churn).
	bad := json.RawMessage(`{not valid`)
	if r.parseMatchConditions(bad) != nil {
		t.Error("malformed conditions must parse to nil")
	}
	if r.parseMatchConditions(json.RawMessage(`{not valid`)) != nil {
		t.Error("malformed conditions must stay nil from cache")
	}
}

// TestParseMatchConditions_RaceSafe runs concurrent parses of the same and
// distinct conditions through the lock-free sync.Map cache; -race must stay
// clean and every valid result must be non-nil (correct data flow under
// concurrency).
func TestParseMatchConditions_RaceSafe(t *testing.T) {
	r := &Resolver{}
	inputs := []json.RawMessage{
		json.RawMessage(`{"models":["a"]}`),
		json.RawMessage(`{"models":["b"]}`),
		json.RawMessage(`{"providers":["p"]}`),
		json.RawMessage(`{bad`),
	}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			in := inputs[n%len(inputs)]
			got := r.parseMatchConditions(in)
			if len(in) > 0 && in[0] == '{' && in[1] != 'b' && got == nil {
				t.Errorf("valid input %s parsed to nil under concurrency", in)
			}
		}(i)
	}
	wg.Wait()
}
