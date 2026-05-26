package consumer

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
)

func TestDeserializeTrafficEvent(t *testing.T) {
	raw := `{
		"id": "evt-001",
		"source": "ai-gateway",
		"hookDecision": "block",
		"hookReasonCode": "rate_limited"
	}`

	f := &SIEMForwarder{}
	evt, err := f.deserializeTrafficEvent([]byte(raw))
	if err != nil {
		t.Fatalf("deserialize error: %v", err)
	}

	if evt["id"] != "evt-001" {
		t.Errorf("id = %v; want evt-001", evt["id"])
	}
	if evt["eventType"] != "traffic.rate_limited" {
		t.Errorf("eventType = %v; want traffic.rate_limited", evt["eventType"])
	}
}

func TestDeserializeTrafficEvent_AllowedClassified(t *testing.T) {
	raw := `{"id": "evt-002", "source": "ai-gateway", "hookDecision": "allow"}`

	f := &SIEMForwarder{}
	evt, err := f.deserializeTrafficEvent([]byte(raw))
	if err != nil {
		t.Fatalf("deserialize error: %v", err)
	}
	if evt["eventType"] != "traffic.allowed" {
		t.Errorf("eventType = %v; want traffic.allowed", evt["eventType"])
	}
}

func TestDeserializeAdminEvent(t *testing.T) {
	raw := `{
		"id": "audit-001",
		"action": "create",
		"entityType": "provider",
		"actorLabel": "admin@nexus.ai"
	}`

	f := &SIEMForwarder{}
	evt, err := f.deserializeAdminEvent([]byte(raw))
	if err != nil {
		t.Fatalf("deserialize error: %v", err)
	}

	if evt["source"] != "admin" {
		t.Errorf("source = %v; want admin", evt["source"])
	}
	if evt["eventType"] != "provider.create" {
		t.Errorf("eventType = %v; want provider.create", evt["eventType"])
	}
}

func TestDeserializeAdminEvent_PreservesExplicitSource(t *testing.T) {
	raw := `{"id": "a1", "action": "login", "entityType": "session", "source": "control-plane"}`

	f := &SIEMForwarder{}
	evt, err := f.deserializeAdminEvent([]byte(raw))
	if err != nil {
		t.Fatalf("deserialize error: %v", err)
	}
	if evt["source"] != "control-plane" {
		t.Errorf("source = %v; want control-plane (explicit)", evt["source"])
	}
}

func TestDeserializeEvent_InvalidJSON(t *testing.T) {
	f := &SIEMForwarder{}
	_, err := f.deserializeEvent("nexus.event.ai-traffic", []byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestFlush_FiltersEvents(t *testing.T) {
	var sentEvents []siem.Event

	sink := &fakeSink{sendFn: func(events []siem.Event) error {
		sentEvents = events
		return nil
	}}

	f := NewSIEMForwarder(
		nil, sink,
		SIEMForwarderConfig{
			EventTypes: []string{"traffic.rate_limited"},
		},
		testLogger(),
		newTestRegistry(),
	)

	blockedEvt := siem.Event{"eventType": "traffic.rate_limited"}
	allowedEvt := siem.Event{"eventType": ""}

	items := []pendingSIEMMessage{
		{event: blockedEvt, msg: newFakeMQMessage(nil)},
		{event: allowedEvt, msg: newFakeMQMessage(nil)},
	}

	_ = f.flush(context.Background(), items)

	if len(sentEvents) != 1 {
		t.Fatalf("expected 1 filtered event, got %d", len(sentEvents))
	}
	if sentEvents[0]["eventType"] != "traffic.rate_limited" {
		t.Errorf("eventType = %v; want traffic.rate_limited", sentEvents[0]["eventType"])
	}
}
