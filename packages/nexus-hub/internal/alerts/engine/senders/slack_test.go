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

func TestSlackSender_WebhookPath(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := alerting.Channel{Type: "slack", Config: map[string]any{"webhookUrl": srv.URL}}
	alert := alerting.Alert{
		Severity: alerting.SeverityCritical, TargetLabel: "Org X", Message: "quota 95%",
	}
	sc, err := senders.NewSlack(nil).Send(context.Background(), ch, alert)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sc != 200 {
		t.Fatalf("sc=%d", sc)
	}
	text, _ := body["text"].(string)
	if text == "" {
		t.Fatalf("missing text in body: %v", body)
	}
	// sanity: text contains severity + target + message
	for _, want := range []string{"CRITICAL", "Org X", "quota 95%"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q missing %q", text, want)
		}
	}
}

func TestSlackSender_ChatPostMessagePath(t *testing.T) {
	var gotAuth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Rewrite the outbound request URL (code hits the hard-coded Slack API URL)
	// so the real code path runs against our test server.
	client := &http.Client{Transport: &urlRewriter{to: srv.URL}}
	ch := alerting.Channel{Type: "slack", Config: map[string]any{
		"botToken": "xoxb-test", "channel": "#alerts",
	}}
	alert := alerting.Alert{
		Severity: alerting.SeverityHigh, TargetLabel: "X", Message: "m",
	}
	sc, err := senders.NewSlack(client).Send(context.Background(), ch, alert)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sc != 200 {
		t.Fatalf("sc=%d", sc)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("Authorization: got %q", gotAuth)
	}
	if body["channel"] != "#alerts" {
		t.Fatalf("channel: got %v", body["channel"])
	}
}

func TestSlackSender_MissingConfigIsError(t *testing.T) {
	ch := alerting.Channel{Type: "slack", Config: map[string]any{}}
	_, err := senders.NewSlack(nil).Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

// urlRewriter redirects any outbound request to the given server URL, keeping
// method and body, so we can exercise code that hits hard-coded external URLs
// (e.g. Slack chat.postMessage, PagerDuty Events API).
type urlRewriter struct{ to string }

func (u *urlRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	target, err := http.NewRequest(r.Method, u.to, r.Body)
	if err != nil {
		return nil, err
	}
	target.Header = r.Header
	return http.DefaultTransport.RoundTrip(target)
}
