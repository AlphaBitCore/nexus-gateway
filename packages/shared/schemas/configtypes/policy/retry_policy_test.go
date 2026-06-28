package policy

import (
	"github.com/goccy/go-json"
	"reflect"
	"testing"
	"time"
)

func TestDefaultRetryPolicy_Values(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxAttemptsPerTarget != 2 {
		t.Errorf("MaxAttemptsPerTarget: got %d, want 2 (one same-target retry)", p.MaxAttemptsPerTarget)
	}
	if got := len(p.RetryOn); got != 4 {
		t.Errorf("RetryOn: got %d classes, want 4", got)
	}
	if p.BackoffInitial != 250*time.Millisecond {
		t.Errorf("BackoffInitial: got %v, want 250ms", p.BackoffInitial)
	}
	if p.BackoffMax != 5*time.Second {
		t.Errorf("BackoffMax: got %v, want 5s", p.BackoffMax)
	}
	if p.BackoffJitter != 0.2 {
		t.Errorf("BackoffJitter: got %v, want 0.2", p.BackoffJitter)
	}
}

func equalPolicies(a, b RetryPolicy) bool {
	if a.MaxAttemptsPerTarget != b.MaxAttemptsPerTarget ||
		a.BackoffInitial != b.BackoffInitial ||
		a.BackoffMax != b.BackoffMax ||
		a.BackoffJitter != b.BackoffJitter {
		return false
	}
	return reflect.DeepEqual(a.RetryOn, b.RetryOn)
}

func TestMergedWith_NilOverride_UsesDefault(t *testing.T) {
	base := DefaultRetryPolicy()
	out := base.MergedWith(nil)
	if !equalPolicies(out, base) {
		t.Errorf("nil override must return base unchanged: got %+v, want %+v", out, base)
	}
}

func TestMergedWith_FullOverride(t *testing.T) {
	base := DefaultRetryPolicy()
	rule := &RetryPolicy{
		MaxAttemptsPerTarget: 3,
		RetryOn:              []ErrorClass{ErrorClass5xx},
		BackoffInitial:       100 * time.Millisecond,
		BackoffMax:           1 * time.Second,
		BackoffJitter:        0.1,
	}
	out := base.MergedWith(rule)
	if out.MaxAttemptsPerTarget != 3 {
		t.Errorf("MaxAttemptsPerTarget: %d", out.MaxAttemptsPerTarget)
	}
	if len(out.RetryOn) != 1 || out.RetryOn[0] != ErrorClass5xx {
		t.Errorf("RetryOn: %v", out.RetryOn)
	}
	if out.BackoffInitial != 100*time.Millisecond {
		t.Errorf("BackoffInitial: %v", out.BackoffInitial)
	}
}

func TestMergedWith_PartialOverride_FieldMerge(t *testing.T) {
	base := DefaultRetryPolicy()
	rule := &RetryPolicy{MaxAttemptsPerTarget: 2}
	out := base.MergedWith(rule)
	if out.MaxAttemptsPerTarget != 2 {
		t.Errorf("MaxAttemptsPerTarget should be overridden, got %d", out.MaxAttemptsPerTarget)
	}
	if len(out.RetryOn) != 4 {
		t.Errorf("RetryOn should fall back to default (4 classes), got %d", len(out.RetryOn))
	}
	if out.BackoffInitial != base.BackoffInitial {
		t.Errorf("BackoffInitial should fall back to default")
	}
}

func TestMergedWith_EmptyRetryOnIsRespected(t *testing.T) {
	base := DefaultRetryPolicy()
	rule := &RetryPolicy{RetryOn: []ErrorClass{}}
	out := base.MergedWith(rule)
	if out.RetryOn == nil {
		t.Errorf("empty RetryOn must not become nil")
	}
	if len(out.RetryOn) != 0 {
		t.Errorf("empty RetryOn must stay length 0 (means 'retry nothing'), got %d", len(out.RetryOn))
	}
}

func TestRetryPolicy_JSONRoundTrip(t *testing.T) {
	in := RetryPolicy{
		MaxAttemptsPerTarget: 3,
		RetryOn:              []ErrorClass{ErrorClassTimeout, ErrorClass5xx},
		BackoffInitial:       200 * time.Millisecond,
		BackoffMax:           3 * time.Second,
		BackoffJitter:        0.15,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RetryPolicy
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !equalPolicies(in, out) {
		t.Errorf("round trip mismatch:\n  in:  %+v\n  out: %+v", in, out)
	}
}

func TestRetryPolicy_MaxAttemptsClamping(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 1},
		{-3, 1},
		{1, 1},
		{3, 3},
		{5, 5},
		{6, 5},
		{1000, 5},
	}
	for _, tc := range cases {
		if got := ClampMaxAttempts(tc.in); got != tc.want {
			t.Errorf("ClampMaxAttempts(%d): got %d, want %d", tc.in, got, tc.want)
		}
	}
}
