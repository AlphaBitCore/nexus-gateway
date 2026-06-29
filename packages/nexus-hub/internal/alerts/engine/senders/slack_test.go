package senders_test

import (
	"context"
	"github.com/goccy/go-json"
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
	// Unguarded client so the 127.0.0.1 httptest webhook is reachable; the SSRF
	// guard that NewSlack(nil) installs is covered by TestSlackSender_BlocksMetadata.
	sc, err := senders.NewSlack(http.DefaultClient).Send(context.Background(), ch, alert)
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

// TestSlackSender_WebhookErrorCollapsesOracle verifies that when the
// webhook POST fails with a non-2xx status, the returned error (persisted on the
// AlertDispatch row and shown in the admin UI) must NOT contain the secret webhook
// URL AND must NOT reveal the upstream status code — both are fingerprinting
// signals. The status code collapses to 0 and the error is the single generic
// string, byte-identical across distinct upstream statuses.
func TestSlackSender_WebhookErrorCollapsesOracle(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		ch := alerting.Channel{Type: "slack", Config: map[string]any{"webhookUrl": srv.URL}}
		sc, err := senders.NewSlack(http.DefaultClient).Send(context.Background(), ch,
			alerting.Alert{Severity: alerting.SeverityHigh, Message: "x"})
		leaked := srv.URL
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error on non-2xx", status)
		}
		if sc != 0 {
			t.Errorf("status %d: sc=%d, want 0 (collapsed)", status, sc)
		}
		if strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), "http://") {
			t.Errorf("status %d: error leaks the webhook URL: %q", status, err.Error())
		}
		if err.Error() != "alert delivery failed" {
			t.Errorf("status %d: err=%q, want generic 'alert delivery failed'", status, err.Error())
		}
	}
}

// TestSlackSender_BlocksMetadata proves the default guarded client (NewSlack(nil))
// refuses a metadata / private webhook target.
func TestSlackSender_BlocksMetadata(t *testing.T) {
	ch := alerting.Channel{Type: "slack", Config: map[string]any{
		"webhookUrl": "http://169.254.169.254/hook",
	}}
	_, err := senders.NewSlack(nil).Send(context.Background(), ch,
		alerting.Alert{Severity: alerting.SeverityHigh, Message: "x"})
	if err == nil {
		t.Fatal("slack webhook to 169.254.169.254 must be blocked")
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
