package main

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/policies"
)

// fakeShadowApplier records the raw it was handed and returns a configurable error.
type fakeShadowApplier struct {
	gotRaw json.RawMessage
	err    error
}

func (f *fakeShadowApplier) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	f.gotRaw = raw
	return f.err
}

func TestApplyOf(t *testing.T) {
	fa := &fakeShadowApplier{}
	ra := applyOf(fa)
	// rawApply always returns nil bytes (appliers are side-effecting) and
	// forwards the raw + propagates the applier's error.
	out, err := ra(context.Background(), []byte(`{"x":1}`), 7)
	if out != nil || err != nil || string(fa.gotRaw) != `{"x":1}` {
		t.Fatalf("applyOf happy: out=%v err=%v gotRaw=%s", out, err, fa.gotRaw)
	}
	fa.err = errors.New("apply boom")
	if _, err := ra(context.Background(), []byte(`{}`), 8); err == nil {
		t.Fatal("applyOf must propagate the applier error")
	}
}

func TestAdaptApplyFunc(t *testing.T) {
	var gotRaw json.RawMessage
	sentinel := errors.New("fn boom")
	ra := adaptApplyFunc(func(_ context.Context, raw json.RawMessage) error {
		gotRaw = raw
		return sentinel
	})
	out, err := ra(context.Background(), []byte(`{"k":2}`), 1)
	if out != nil || !errors.Is(err, sentinel) || string(gotRaw) != `{"k":2}` {
		t.Fatalf("adaptApplyFunc: out=%v err=%v gotRaw=%s", out, err, gotRaw)
	}
}

func TestTeeCatB(t *testing.T) {
	inner := &fakeShadowApplier{}
	applier := teeCatB("hooks", inner, nil)
	tee, ok := applier.(policies.TeeApplier)
	if !ok {
		t.Fatalf("teeCatB must return a policies.TeeApplier, got %T", applier)
	}
	if tee.CfgKey != "hooks" || tee.Inner != inner {
		t.Fatalf("teeCatB wiring wrong: %+v", tee)
	}
}

// TestApplyOfKillSwitch verifies:
// - The applier returns a non-nil live snapshot.
// - The pauser's adminEngaged bit tracks the shadow payload.
func TestApplyOfKillSwitch(t *testing.T) {
	newDeps := func() (*killswitch.Switch, *protectionpause.Pauser) {
		ks := killswitch.New(slog.Default())
		p := protectionpause.New(ks)
		return ks, p
	}

	t.Run("engage via shadow sets adminEngaged and returns live snapshot", func(t *testing.T) {
		ks, p := newDeps()
		ra := applyOfKillSwitch(ks, p)
		out, err := ra(context.Background(), []byte(`{"engaged":true}`), 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Returned bytes must be a non-nil live snapshot.
		if out == nil {
			t.Fatal("applyOfKillSwitch must return non-nil snapshot bytes")
		}
		var snap killswitch.Snapshot
		if err := json.Unmarshal(out, &snap); err != nil {
			t.Fatalf("snapshot bytes not valid JSON: %v", err)
		}
		if !snap.Engaged {
			t.Error("snapshot.Engaged must be true after engage shadow")
		}
		// Pauser's adminEngaged bit must be set.
		if !p.IsAdminEngaged() {
			t.Error("pauser.IsAdminEngaged must be true after engage shadow")
		}
	})

	t.Run("disengage via shadow clears adminEngaged but preserves user pause", func(t *testing.T) {
		ks, p := newDeps()
		ra := applyOfKillSwitch(ks, p)

		// First engage via shadow.
		if _, err := ra(context.Background(), []byte(`{"engaged":true}`), 1); err != nil {
			t.Fatalf("setup engage: %v", err)
		}
		// User also pauses.
		p.Pause(0)

		// Now shadow disengages.
		out, err := ra(context.Background(), []byte(`{"engaged":false}`), 2)
		if err != nil {
			t.Fatalf("disengage shadow: %v", err)
		}
		// Admin brake cleared.
		if p.IsAdminEngaged() {
			t.Error("pauser.IsAdminEngaged must be false after disengage shadow")
		}
		// User pause keeps kill switch engaged.
		if !ks.IsEngaged() {
			t.Error("killswitch must remain engaged because user pause is still active")
		}
		// Returned bytes must be non-nil.
		if out == nil {
			t.Fatal("applyOfKillSwitch must return non-nil snapshot bytes on disengage")
		}
		// The snapshot reports the live state (engaged=true because user is still paused).
		var snap killswitch.Snapshot
		if err := json.Unmarshal(out, &snap); err != nil {
			t.Fatalf("snapshot bytes not valid JSON: %v", err)
		}
		if !snap.Engaged {
			t.Error("snapshot.Engaged must be true: user pause is still active")
		}
	})

	t.Run("null payload is no-op and returns disengaged snapshot", func(t *testing.T) {
		ks, p := newDeps()
		ra := applyOfKillSwitch(ks, p)
		out, err := ra(context.Background(), nil, 1)
		if err != nil {
			t.Fatalf("null payload: %v", err)
		}
		if out == nil {
			t.Fatal("applyOfKillSwitch must return non-nil snapshot bytes for null payload")
		}
		var snap killswitch.Snapshot
		if err := json.Unmarshal(out, &snap); err != nil {
			t.Fatalf("snapshot bytes not valid JSON: %v", err)
		}
		if snap.Engaged {
			t.Error("snapshot.Engaged must be false for null payload (no-op)")
		}
		if p.IsAdminEngaged() {
			t.Error("pauser.IsAdminEngaged must be false for null payload")
		}
	})
}
