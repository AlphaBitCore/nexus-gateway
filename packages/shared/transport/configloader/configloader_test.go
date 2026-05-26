package configloader_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type killswitchState struct {
	Engaged bool `json:"engaged"`
}

func TestNew_NilLoggerFallsBackToDefault(t *testing.T) {
	l := configloader.New(nil, nil, "test-id", "test-type")
	if l == nil {
		t.Fatal("New returned nil")
	}
}

func TestRegister_PanicsOnEmptyKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty key")
		}
	}()
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.Register(l, configloader.Handler[killswitchState]{
		Apply: func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) { return nil, nil },
	})
}

func TestRegister_PanicsOnNilApply(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Apply")
		}
	}()
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.Register(l, configloader.Handler[killswitchState]{Key: "killswitch"})
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	apply := func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) { return nil, nil }
	configloader.Register(l, configloader.Handler[killswitchState]{Key: "killswitch", Apply: apply})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate key")
		}
	}()
	configloader.Register(l, configloader.Handler[killswitchState]{Key: "killswitch", Apply: apply})
}

func TestRegisterRaw_PanicsOnEmptyKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty key")
		}
	}()
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.RegisterRaw(l, "", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })
}

func TestRegisterRaw_PanicsOnNilApply(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil apply")
		}
	}()
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.RegisterRaw(l, "k", nil)
}

func TestRegisterRaw_PanicsOnDuplicate(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	apply := func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil }
	configloader.RegisterRaw(l, "k", apply)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate key")
		}
	}()
	configloader.RegisterRaw(l, "k", apply)
}

func TestHasAndKeys(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	if l.Has("k") {
		t.Fatal("expected Has(k) = false")
	}
	configloader.RegisterRaw(l, "a", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })
	configloader.RegisterRaw(l, "b", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })
	if !l.Has("a") || !l.Has("b") {
		t.Fatal("expected Has true for registered keys")
	}
	keys := l.Keys()
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("Keys: got %v, want [a b]", keys)
	}
}

func TestApply_DispatchesAndAssemblesReported(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")

	applied := map[string]bool{}
	configloader.Register(l, configloader.Handler[killswitchState]{
		Key:   "killswitch",
		Parse: configloader.ParseJSON[killswitchState](),
		Apply: func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) {
			applied["killswitch"] = v.Engaged
			// Return live snapshot — verify Loader respects it over the
			// raw bytes (echo-from-state semantics for keys whose live
			// view diverges from desired).
			return []byte(`{"engaged":false,"note":"live"}`), nil
		},
	})
	configloader.RegisterRaw(l, "hooks", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		applied["hooks"] = true
		// nil reported → Loader echoes desired bytes.
		return nil, nil
	})

	desired := map[string]thingclient.ConfigState{
		"killswitch": {State: json.RawMessage(`{"engaged":true}`), Version: 7},
		"hooks":      {State: json.RawMessage(`{"hooks":[]}`), Version: 12},
	}
	reported, err := l.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}

	if applied["killswitch"] != true {
		t.Fatal("killswitch apply not invoked with parsed value")
	}
	if !applied["hooks"] {
		t.Fatal("hooks apply not invoked")
	}
	if got := string(reported["killswitch"].State); got != `{"engaged":false,"note":"live"}` {
		t.Fatalf("killswitch reported: got %q, want live snapshot", got)
	}
	if reported["killswitch"].Version != 7 {
		t.Fatalf("killswitch reported.Version: got %d, want 7", reported["killswitch"].Version)
	}
	if got := string(reported["hooks"].State); got != `{"hooks":[]}` {
		t.Fatalf("hooks reported: got %q, want echo of desired", got)
	}

	snap := tracker.Snapshot()
	if snap["killswitch"].AppliedVersion == nil || *snap["killswitch"].AppliedVersion != 7 {
		t.Fatalf("outcomes.killswitch.AppliedVersion not 7: %+v", snap["killswitch"])
	}
	if snap["hooks"].AppliedVersion == nil || *snap["hooks"].AppliedVersion != 12 {
		t.Fatalf("outcomes.hooks.AppliedVersion not 12: %+v", snap["hooks"])
	}
}

func TestApply_UnknownKeySkippedNotErrored(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	configloader.RegisterRaw(l, "known", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	})

	desired := map[string]thingclient.ConfigState{
		"known":   {State: json.RawMessage(`{}`), Version: 1},
		"unknown": {State: json.RawMessage(`{}`), Version: 2},
	}
	reported, err := l.Apply(context.Background(), desired)
	if err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if _, ok := reported["unknown"]; ok {
		t.Fatal("unknown key must not appear in reported map")
	}
	if _, ok := reported["known"]; !ok {
		t.Fatal("known key missing from reported map")
	}
	// Unknown keys must not pollute the outcome ledger — Hub would
	// otherwise see an apply_error for a key the Thing does not own.
	if _, ok := tracker.Snapshot()["unknown"]; ok {
		t.Fatal("unknown key recorded in outcome tracker")
	}
}

func TestApply_ParseErrorRecordsOutcomeAndContinues(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	configloader.Register(l, configloader.Handler[killswitchState]{
		Key:   "killswitch",
		Parse: configloader.ParseJSON[killswitchState](),
		Apply: func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) { return nil, nil },
	})
	// Second handler must still get its apply attempt despite the first
	// key's parse failure (continue-on-error invariant).
	otherApplied := false
	configloader.RegisterRaw(l, "other", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		otherApplied = true
		return nil, nil
	})

	desired := map[string]thingclient.ConfigState{
		"killswitch": {State: json.RawMessage(`{"engaged":NOT_JSON`), Version: 3},
		"other":      {State: json.RawMessage(`{}`), Version: 5},
	}
	_, err := l.Apply(context.Background(), desired)
	if err == nil {
		t.Fatal("expected parse error surfaced, got nil")
	}
	if !otherApplied {
		t.Fatal("continue-on-error broken: second handler did not run")
	}
	snap := tracker.Snapshot()
	if snap["killswitch"].ApplyError == nil {
		t.Fatal("outcomes.killswitch.ApplyError not set")
	}
	if snap["other"].AppliedVersion == nil || *snap["other"].AppliedVersion != 5 {
		t.Fatal("outcomes.other.AppliedVersion not recorded")
	}
}

func TestApply_ApplyErrorRecordsOutcomeAndContinues(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	wantErr := errors.New("subsystem reload failed")
	configloader.RegisterRaw(l, "hooks", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, wantErr
	})
	otherApplied := false
	configloader.RegisterRaw(l, "other", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		otherApplied = true
		return nil, nil
	})

	desired := map[string]thingclient.ConfigState{
		"hooks": {State: json.RawMessage(`{}`), Version: 4},
		"other": {State: json.RawMessage(`{}`), Version: 8},
	}
	_, err := l.Apply(context.Background(), desired)
	if err == nil {
		t.Fatal("expected apply error surfaced, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped err to contain wantErr; got %v", err)
	}
	if !otherApplied {
		t.Fatal("continue-on-error broken: second handler did not run")
	}
	snap := tracker.Snapshot()
	if snap["hooks"].ApplyError == nil {
		t.Fatal("outcomes.hooks.ApplyError not set")
	}
}

// When MULTIPLE keys fail, Apply surfaces only the first; the rest live
// on the OutcomeTracker. Verify both slots so the WS layer + Hub stay
// in sync about who broke.
func TestApply_MultipleFailures_FirstErrorWinsOutcomeKeepsAll(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	configloader.RegisterRaw(l, "a", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, errors.New("err-a")
	})
	configloader.RegisterRaw(l, "b", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, errors.New("err-b")
	})
	desired := map[string]thingclient.ConfigState{
		"a": {State: json.RawMessage(`{}`), Version: 1},
		"b": {State: json.RawMessage(`{}`), Version: 2},
	}
	_, err := l.Apply(context.Background(), desired)
	if err == nil {
		t.Fatal("expected an error")
	}
	snap := tracker.Snapshot()
	if snap["a"].ApplyError == nil || snap["b"].ApplyError == nil {
		t.Fatal("both keys must have ApplyError recorded")
	}
}

func TestApply_NilOutcomeTrackerIsSafe(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.RegisterRaw(l, "k", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	})
	_, err := l.Apply(context.Background(), map[string]thingclient.ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 1},
	})
	if err != nil {
		t.Fatalf("Apply with nil tracker: %v", err)
	}
}

func TestHandler_Returns_OnConfigChangedCompatibleFunc(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.RegisterRaw(l, "k", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	})
	fn := l.Handler()
	reported, err := fn(map[string]thingclient.ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 1},
	})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if _, ok := reported["k"]; !ok {
		t.Fatal("Handler: reported missing key")
	}
}

func TestParseJSON_EmptyInputIsZeroValueNoError(t *testing.T) {
	parse := configloader.ParseJSON[killswitchState]()
	v, err := parse(nil)
	if err != nil {
		t.Fatalf("ParseJSON(nil): %v", err)
	}
	if v.Engaged {
		t.Fatalf("ParseJSON(nil): expected zero value, got %+v", v)
	}
}

func TestParseJSON_ParseErrorPropagates(t *testing.T) {
	parse := configloader.ParseJSON[killswitchState]()
	if _, err := parse([]byte(`{NOT_JSON`)); err == nil {
		t.Fatal("expected parse error")
	}
}

// Exercise the full happy-path through Handler() with the wiring shape
// real services use: thingclient → Loader.Handler → Register chain.
func TestHandler_EndToEnd(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "compliance-proxy-1", "compliance-proxy")

	type observability struct {
		OtelEnabled  bool    `json:"otelEnabled"`
		SamplingRate float64 `json:"samplingRate"`
	}

	configloader.Register(l, configloader.Handler[observability]{
		Key:   "observability",
		Parse: configloader.ParseJSON[observability](),
		Apply: func(ctx context.Context, v observability, ver int64) ([]byte, error) {
			// Apply would call telemetry provider Reconfigure here.
			b, _ := json.Marshal(map[string]any{"applied": v.OtelEnabled})
			return b, nil
		},
	})

	cb := l.Handler()
	reported, err := cb(map[string]thingclient.ConfigState{
		"observability": {
			State:   json.RawMessage(`{"otelEnabled":true,"samplingRate":0.5}`),
			Version: 19,
		},
	})
	if err != nil {
		t.Fatalf("cb: %v", err)
	}
	got := string(reported["observability"].State)
	if got != `{"applied":true}` {
		t.Fatalf("reported state: got %q, want %q", got, `{"applied":true}`)
	}
}

// Ensure that even when an Apply panics inside the handler, we don't
// silently lose the outcome — Go's panic semantics let the error bubble
// up unwound, but the tracker should not be in a partial state. The
// current implementation does not recover panics; we keep this test
// to assert that contract explicitly.
func TestApply_PanicInHandlerPropagates(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	configloader.RegisterRaw(l, "boom", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		panic("apply blew up")
	})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic to propagate")
		}
	}()
	_, _ = l.Apply(context.Background(), map[string]thingclient.ConfigState{
		"boom": {State: json.RawMessage(`{}`), Version: 1},
	})
}

func TestRegisterRawPull_MarksNeedsPull(t *testing.T) {
	l := configloader.New(discardLogger(), nil, "t", "test")
	configloader.RegisterRaw(l, "a", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })
	configloader.RegisterRawPull(l, "b", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })
	pulls := l.PullKeys()
	if len(pulls) != 1 || pulls[0] != "b" {
		t.Fatalf("PullKeys: got %v, want [b]", pulls)
	}
}

func TestApply_NeedsPull_ReplacesDesiredBytesWithPulled(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	pulled := []byte(`{"real":true}`)
	pullCalls := 0
	puller := func(ctx context.Context, key string) ([]byte, error) {
		pullCalls++
		if key != "hooks" {
			t.Errorf("puller called for unexpected key %q", key)
		}
		return pulled, nil
	}
	l := configloader.New(discardLogger(), tracker, "t", "test", configloader.WithPuller(puller))

	receivedBytes := []byte(nil)
	configloader.RegisterRawPull(l, "hooks", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		receivedBytes = raw
		return nil, nil
	})

	desired := map[string]thingclient.ConfigState{
		// Stub bytes (Hub-pushed "needsPull" marker) — must be ignored.
		"hooks": {State: json.RawMessage(`{"needsPull":true}`), Version: 5},
	}
	if _, err := l.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pullCalls != 1 {
		t.Fatalf("puller call count: got %d, want 1", pullCalls)
	}
	if string(receivedBytes) != string(pulled) {
		t.Fatalf("apply received %q, want pulled bytes %q", receivedBytes, pulled)
	}
}

func TestApply_NeedsPull_NoPullerConfigured_FailsKey(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test") // no WithPuller
	applied := false
	configloader.RegisterRawPull(l, "policy_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		applied = true
		return nil, nil
	})
	_, err := l.Apply(context.Background(), map[string]thingclient.ConfigState{
		"policy_rules": {State: json.RawMessage(`{}`), Version: 1},
	})
	if err == nil {
		t.Fatal("expected error surfaced for missing puller")
	}
	if applied {
		t.Fatal("apply must not run when pull failed")
	}
	if tracker.Snapshot()["policy_rules"].ApplyError == nil {
		t.Fatal("outcome tracker missing pull error")
	}
}

func TestApply_NeedsPull_PullerError_PropagatedNonBlocking(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	puller := func(ctx context.Context, key string) ([]byte, error) {
		return nil, errors.New("pull failed")
	}
	l := configloader.New(discardLogger(), tracker, "t", "test", configloader.WithPuller(puller))
	configloader.RegisterRawPull(l, "policy_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		t.Fatal("apply must not run when pull fails")
		return nil, nil
	})
	// Second key without NeedsPull must still get its apply attempt.
	otherApplied := false
	configloader.RegisterRaw(l, "log_level", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		otherApplied = true
		return nil, nil
	})
	_, err := l.Apply(context.Background(), map[string]thingclient.ConfigState{
		"policy_rules": {State: json.RawMessage(`{}`), Version: 7},
		"log_level":    {State: json.RawMessage(`{}`), Version: 8},
	})
	if err == nil {
		t.Fatal("expected an error from pull failure")
	}
	if !otherApplied {
		t.Fatal("continue-on-error broken: non-NeedsPull key did not run")
	}
	if tracker.Snapshot()["policy_rules"].ApplyError == nil {
		t.Fatal("policy_rules ApplyError not recorded")
	}
}

func TestApply_HandlerWithNeedsPull_FieldOnGenericHandler(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	pulled := []byte(`{"engaged":true}`)
	puller := func(ctx context.Context, key string) ([]byte, error) { return pulled, nil }
	l := configloader.New(discardLogger(), tracker, "t", "test", configloader.WithPuller(puller))

	receivedV := killswitchState{}
	configloader.Register(l, configloader.Handler[killswitchState]{
		Key:       "killswitch",
		NeedsPull: true,
		Parse:     configloader.ParseJSON[killswitchState](),
		Apply: func(ctx context.Context, v killswitchState, ver int64) ([]byte, error) {
			receivedV = v
			return nil, nil
		},
	})
	desired := map[string]thingclient.ConfigState{
		"killswitch": {State: json.RawMessage(`{"needsPull":true}`), Version: 9},
	}
	if _, err := l.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !receivedV.Engaged {
		t.Fatal("Parse did not read the pulled bytes (Engaged stayed false)")
	}
}

func TestRefreshPullKeys_AppliesEveryNeedsPullKey(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	pulled := map[string][]byte{
		"policy_rules":         []byte(`{"policy":1}`),
		"hooks":                []byte(`{"hook":1}`),
		"interception_domains": []byte(`{"id":1}`),
	}
	puller := func(ctx context.Context, key string) ([]byte, error) {
		b, ok := pulled[key]
		if !ok {
			return nil, fmt.Errorf("unexpected pull for %s", key)
		}
		return b, nil
	}
	l := configloader.New(discardLogger(), tracker, "agent-1", "agent", configloader.WithPuller(puller))

	appliedKeys := map[string]bool{}
	for _, key := range []string{"policy_rules", "hooks", "interception_domains"} {
		k := key
		configloader.RegisterRawPull(l, k, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
			appliedKeys[k] = true
			return nil, nil
		})
	}
	// Cat A key — must NOT be auto-pulled by RefreshPullKeys.
	configloader.RegisterRaw(l, "killswitch", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		t.Fatal("RefreshPullKeys must not invoke Cat A keys")
		return nil, nil
	})

	applied, failed := l.RefreshPullKeys(context.Background())
	if applied != 3 || failed != 0 {
		t.Fatalf("RefreshPullKeys: applied=%d failed=%d, want 3/0", applied, failed)
	}
	for _, key := range []string{"policy_rules", "hooks", "interception_domains"} {
		if !appliedKeys[key] {
			t.Errorf("RefreshPullKeys did not apply %q", key)
		}
	}
}

func TestRefreshPullKeys_NoPuller_NoOp(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test") // no WithPuller
	configloader.RegisterRawPull(l, "policy_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		t.Fatal("RefreshPullKeys must skip when no Puller wired")
		return nil, nil
	})
	applied, failed := l.RefreshPullKeys(context.Background())
	if applied != 0 || failed != 0 {
		t.Fatalf("RefreshPullKeys: got applied=%d failed=%d, want 0/0", applied, failed)
	}
}

func TestRefreshPullKeys_PullerErrorCounted(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	puller := func(ctx context.Context, key string) ([]byte, error) {
		if key == "policy_rules" {
			return nil, errors.New("network blip")
		}
		return []byte(`{}`), nil
	}
	l := configloader.New(discardLogger(), tracker, "t", "test", configloader.WithPuller(puller))
	configloader.RegisterRawPull(l, "policy_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		t.Fatal("apply must not run when pull fails")
		return nil, nil
	})
	configloader.RegisterRawPull(l, "hooks", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) { return nil, nil })

	applied, failed := l.RefreshPullKeys(context.Background())
	if applied != 1 || failed != 1 {
		t.Fatalf("RefreshPullKeys: applied=%d failed=%d, want 1/1", applied, failed)
	}
	if tracker.Snapshot()["policy_rules"].ApplyError == nil {
		t.Fatal("policy_rules pull failure not recorded")
	}
}

func TestRefreshPullKeys_ApplyErrorCounted(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	puller := func(ctx context.Context, key string) ([]byte, error) { return []byte(`{}`), nil }
	l := configloader.New(discardLogger(), tracker, "t", "test", configloader.WithPuller(puller))
	wantErr := errors.New("apply blew up")
	configloader.RegisterRawPull(l, "policy_rules", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, wantErr
	})
	applied, failed := l.RefreshPullKeys(context.Background())
	if applied != 0 || failed != 1 {
		t.Fatalf("RefreshPullKeys: applied=%d failed=%d, want 0/1", applied, failed)
	}
	snap := tracker.Snapshot()
	if snap["policy_rules"].ApplyError == nil {
		t.Fatal("apply failure not recorded")
	}
}

// Sanity: showing the package name + key in the error message is the
// contract callers rely on for log filtering.
func TestApply_ErrorMessageShape(t *testing.T) {
	tracker := thingclient.NewOutcomeTracker()
	l := configloader.New(discardLogger(), tracker, "t", "test")
	configloader.RegisterRaw(l, "k", func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, errors.New("boom")
	})
	_, err := l.Apply(context.Background(), map[string]thingclient.ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 1},
	})
	if err == nil {
		t.Fatal("expected err")
	}
	want := fmt.Sprintf("configloader: apply %s: boom", "k")
	if err.Error() != want {
		t.Fatalf("err.Error: got %q, want %q", err.Error(), want)
	}
}
