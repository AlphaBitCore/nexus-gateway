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
		a.MaxUpstreamCalls != b.MaxUpstreamCalls ||
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

// TestEffectiveCallBudget_UnsetPreservesTodaysReach is the non-regression
// assertion for the call budget.
//
// No across-target cap exists before this field: the walk visits every target
// and each may be attempted up to MaxAttemptsPerTarget times. An unset budget
// must reproduce exactly that reach, so introducing the field changes no
// request's outcome. A fixed default would not be neutral — a rule balancing
// three models behind a four-entry fallback chain legitimately reaches its
// sixth distinct target during a multi-provider incident, and the first
// proposal (a constant 5) would have cut that request off.
func TestEffectiveCallBudget_UnsetPreservesTodaysReach(t *testing.T) {
	p := DefaultRetryPolicy() // MaxAttemptsPerTarget = 2, no budget set

	// 3 load-balanced models + a 4-entry fallback chain.
	const targets = 7
	if got, want := EffectiveCallBudget(p, targets), targets*2; got != want {
		t.Errorf("unset budget must equal targets × attempts (%d), got %d", want, got)
	}

	// The derived value clamps the attempt count the same way the walk does.
	// Reading MaxAttemptsPerTarget raw would let an unset attempt count produce
	// a budget of zero — a policy that refuses to dispatch anything — and an
	// over-large one produce a budget the walk can never spend.
	for _, tc := range []struct{ attempts, targets, want int }{
		{0, 3, 3},  // unset attempts clamp to 1, not 0
		{-2, 3, 3}, // negative likewise
		{7, 3, 15}, // above the ceiling clamps to 5
		{1, 4, 4},  // the floor is honoured as written
	} {
		q := RetryPolicy{MaxAttemptsPerTarget: tc.attempts}
		if got := EffectiveCallBudget(q, tc.targets); got != tc.want {
			t.Errorf("attempts=%d targets=%d: budget %d, want %d (the derived value "+
				"must clamp attempts exactly as the walk does)", tc.attempts, tc.targets, got, tc.want)
		}
	}

	// The property that actually matters: every target can still be reached.
	for _, n := range []int{1, 2, 7, 25, 200} {
		if got := EffectiveCallBudget(p, n); got < n {
			t.Errorf("unset budget %d cannot reach all %d targets — a chain that "+
				"succeeds today would be cut short", got, n)
		}
	}
}

func TestEffectiveCallBudget_ExplicitLowersAndClamps(t *testing.T) {
	base := DefaultRetryPolicy()

	cases := []struct {
		name       string
		configured int
		targets    int
		want       int
	}{
		{"explicit below derived is honored", 3, 7, 3},
		{"explicit above derived is honored", 18, 2, 18},
		{"above ceiling clamps", 1000, 7, 20},
		{"negative clamps to one call", -5, 7, 1},
		{"one is a legal floor", 1, 7, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.MaxUpstreamCalls = tc.configured
			if got := EffectiveCallBudget(p, tc.targets); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}

	// An empty plan still permits the one call that discovers it is empty,
	// rather than a zero budget that refuses before dispatching anything.
	if got := EffectiveCallBudget(base, 0); got < 1 {
		t.Errorf("empty plan produced a budget of %d", got)
	}
}

func TestMergedWith_CallBudget(t *testing.T) {
	base := DefaultRetryPolicy()
	base.MaxUpstreamCalls = 9

	// Zero means absent — the rule said nothing about the budget, so the
	// platform value survives. Same convention as MaxAttemptsPerTarget.
	if got := base.MergedWith(&RetryPolicy{MaxAttemptsPerTarget: 3}); got.MaxUpstreamCalls != 9 {
		t.Errorf("absent budget must not clear the base: got %d", got.MaxUpstreamCalls)
	}
	if got := base.MergedWith(&RetryPolicy{MaxUpstreamCalls: 4}); got.MaxUpstreamCalls != 4 {
		t.Errorf("a rule override must win: got %d", got.MaxUpstreamCalls)
	}
}

// TestRetryPolicy_IsZeroCoversEveryField is the drift guard for IsZero.
//
// IsZero has to enumerate fields, and an enumeration in code is a list someone
// will forget to extend. This sets each field in turn, by reflection, and
// requires IsZero to notice — so adding a field without accounting for it fails
// here, naming the field, instead of surfacing as a deployment whose configured
// policy is silently discarded as "not wired".
func TestRetryPolicy_IsZeroCoversEveryField(t *testing.T) {
	if !(RetryPolicy{}).IsZero() {
		t.Fatal("an untouched policy must read as zero")
	}

	rv := reflect.ValueOf(&RetryPolicy{}).Elem()
	for i := range rv.NumField() {
		var p RetryPolicy
		f := reflect.ValueOf(&p).Elem().Field(i)
		name := rv.Type().Field(i).Name

		switch f.Kind() {
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(0.5)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 0, 0)) // non-nil, length 0
		default:
			t.Fatalf("field %s has kind %s, which this guard does not know how to "+
				"set — extend the switch so the field stays covered", name, f.Kind())
		}

		if p.IsZero() {
			t.Errorf("setting %s left IsZero reporting true: a deployment configuring "+
				"only that field would be treated as unwired and have its policy discarded", name)
		}
	}
}
