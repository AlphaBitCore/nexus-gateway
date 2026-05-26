package senders_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
)

func TestPagerDutySender_Triggers(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	// Rewrite the outbound URL — the PagerDuty Events API URL is a const in
	// the sender, so we route through a custom Transport to hit the test server.
	client := &http.Client{Transport: &urlRewriter{to: srv.URL}}
	ch := alerting.Channel{Type: "pagerduty", Config: map[string]any{"routingKey": "abc123"}}
	a := alerting.Alert{
		RuleID: "thing.offline", TargetKey: "thing-42", TargetLabel: "Agent 42",
		Severity: alerting.SeverityHigh, Message: "heartbeat lost",
		Details: map[string]any{"lastSeen": "5m"},
	}

	sc, err := senders.NewPagerDuty(client).Send(context.Background(), ch, a)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sc != 202 {
		t.Fatalf("sc=%d want 202", sc)
	}
	if body["routing_key"] != "abc123" {
		t.Fatalf("routing_key=%v", body["routing_key"])
	}
	if body["event_action"] != "trigger" {
		t.Fatalf("event_action=%v", body["event_action"])
	}
	if body["dedup_key"] != "thing.offline|thing-42" {
		t.Fatalf("dedup_key=%v", body["dedup_key"])
	}
	payload, _ := body["payload"].(map[string]any)
	summary, _ := payload["summary"].(string)
	if !strings.Contains(summary, "HIGH") || !strings.Contains(summary, "thing.offline") || !strings.Contains(summary, "heartbeat lost") {
		t.Fatalf("summary=%q", summary)
	}
}

func TestPagerDutySender_MissingRoutingKeyIsError(t *testing.T) {
	ch := alerting.Channel{Type: "pagerduty", Config: map[string]any{}}
	_, err := senders.NewPagerDuty(nil).Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for missing routingKey")
	}
}

func TestPagerDutySender_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &urlRewriter{to: srv.URL}}
	ch := alerting.Channel{Type: "pagerduty", Config: map[string]any{"routingKey": "abc"}}
	sc, err := senders.NewPagerDuty(client).Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for 402")
	}
	if sc != 402 {
		t.Fatalf("sc=%d want 402", sc)
	}
}
