package senders_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
)

func TestWebhookSender_PostsJSON(t *testing.T) {
	var gotHeaders http.Header
	var gotBody json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := alerting.Channel{Type: "webhook", Config: map[string]any{
		"url":     srv.URL + "/hook",
		"headers": map[string]any{"X-Token": "abc"},
	}}
	alert := alerting.Alert{
		ID: "x", RuleID: "r", TargetKey: "t",
		Severity: alerting.SeverityHigh, Message: "m",
		FiredAt: time.Now().UTC(),
	}
	sc, err := senders.NewWebhook(http.DefaultClient).Send(context.Background(), ch, alert)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sc != 200 {
		t.Fatalf("sc=%d want 200", sc)
	}
	if gotHeaders.Get("X-Token") != "abc" {
		t.Fatalf("missing X-Token header: got %q", gotHeaders.Get("X-Token"))
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type: got %q", gotHeaders.Get("Content-Type"))
	}
	// body should be Alert-shaped JSON — decode and verify ruleId present
	var parsed map[string]any
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if parsed["ruleId"] != "r" && parsed["RuleID"] != "r" {
		t.Fatalf("body missing ruleId: %v", parsed)
	}
}

func TestWebhookSender_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ch := alerting.Channel{Type: "webhook", Config: map[string]any{"url": srv.URL}}
	sc, err := senders.NewWebhook(nil).Send(context.Background(), ch, alerting.Alert{ID: "x"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if sc != 500 {
		t.Fatalf("sc=%d want 500", sc)
	}
}

func TestWebhookSender_MissingURLIsError(t *testing.T) {
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{}}
	_, err := senders.NewWebhook(nil).Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}
