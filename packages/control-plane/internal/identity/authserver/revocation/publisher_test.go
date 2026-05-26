package revocation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
)

type fakeProducer struct {
	topic     string
	data      []byte
	enqueueEr error
}

func (f *fakeProducer) Publish(ctx context.Context, topic string, data []byte) error { return nil }
func (f *fakeProducer) Enqueue(ctx context.Context, queue string, data []byte) error {
	f.topic, f.data = queue, data
	return f.enqueueEr
}
func (f *fakeProducer) Close() error { return nil }

func TestPublisher_EnqueuesOnExpectedTopic(t *testing.T) {
	p := &fakeProducer{}
	pub := revocation.NewPublisher(p)
	ev := revocation.Event{
		EventID:   "evt_1",
		Scope:     revocation.ScopeJTI,
		TargetJTI: "jti_1",
		RevokedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Reason:    revocation.ReasonUserLogout,
	}
	if err := pub.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if p.topic != revocation.Topic {
		t.Fatalf("topic = %q, want %q", p.topic, revocation.Topic)
	}
	var got revocation.Event
	if err := json.Unmarshal(p.data, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.EventID != ev.EventID {
		t.Fatalf("event_id = %q, want %q", got.EventID, ev.EventID)
	}
}

// TestPublisher_EnqueueErrorIsWrapped covers the err-from-producer branch
// (66.7% → 100% on Publish). A failed enqueue must propagate wrapped with
// "revocation: enqueue:" so the Service caller can log the failure surface
// distinctly from a DB-side insert failure.
func TestPublisher_EnqueueErrorIsWrapped(t *testing.T) {
	want := errors.New("nats unavailable")
	p := &fakeProducer{enqueueEr: want}
	pub := revocation.NewPublisher(p)
	err := pub.Publish(context.Background(), revocation.Event{
		EventID:   "evt_x",
		Scope:     revocation.ScopeJTI,
		TargetJTI: "j",
		RevokedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Reason:    revocation.ReasonReplayDetected,
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped enqueue err: %v", err)
	}
	if !strings.Contains(err.Error(), "revocation: enqueue") {
		t.Fatalf("wrap prefix missing: %v", err)
	}
}
