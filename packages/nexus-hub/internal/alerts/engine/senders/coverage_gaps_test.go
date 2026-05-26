package senders

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// These tests are package `senders` (white-box) so they can drive the
// decodeConfig generic helper directly, exercise the smtp PLAIN-auth
// branch of Email.Send via the unexported `sender` field seam, and verify
// the postJSON c.Do error path with a custom RoundTripper.
//
// Black-box equivalents for the per-sender decodeConfig error and the
// webhook-specific marshal/c.Do/new-request error arms live alongside in
// the existing *_test.go files; these gap tests pin the remaining four
// branches that bring the package from 89.7% to ≥95% under
// [[unit_test_coverage_95]].

// errRoundTripper always returns the given error from RoundTrip — used to
// drive c.Do(...) error branches in postJSON / webhook.Send without a real
// network failure.
type errRoundTripper struct{ err error }

func (e *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// TestDecodeConfig_MarshalErrorBranch covers the `json.Marshal` error arm
// of the shared generic helper. Production callers always pass a regular
// JSON-shaped map, but the helper's defensive marshal-then-unmarshal
// pattern has an err-return that stays uncovered without an explicit
// non-marshalable value (chan / func). Pinning it guards against any
// future caller that silently feeds a richer map type.
func TestDecodeConfig_MarshalErrorBranch(t *testing.T) {
	type cfg struct {
		URL string `json:"url"`
	}
	m := map[string]any{"bad": make(chan int)} // json: unsupported type
	_, err := decodeConfig[cfg](m)
	if err == nil {
		t.Fatal("decodeConfig must return error when map contains an unmarshalable value")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("expected json marshal error, got %v", err)
	}
}

// TestDecodeConfig_UnmarshalErrorBranch covers the `json.Unmarshal` error
// arm. Driven via a wrong-typed value in the source map (number where a
// string is declared). This is the same condition that fires the
// per-sender Send error arm when the admin UI ships a misformed config.
func TestDecodeConfig_UnmarshalErrorBranch(t *testing.T) {
	type cfg struct {
		URL string `json:"url"`
	}
	_, err := decodeConfig[cfg](map[string]any{"url": 12345})
	if err == nil {
		t.Fatal("decodeConfig must return error when JSON shape mismatches T")
	}
	if !strings.Contains(err.Error(), "url") || !strings.Contains(err.Error(), "string") {
		t.Errorf("expected unmarshal-into-string error citing field, got %v", err)
	}
}

// TestPostJSON_TransportError covers the `c.Do(req)` error arm of postJSON.
// Without this the dispatcher would silently swallow transport failures
// because the success path also returns (0, nil) until the response is
// received. Pinning the error wrap protects the dispatcher's ability to
// distinguish "PD server returned 500" from "PD unreachable".
func TestPostJSON_TransportError(t *testing.T) {
	sentinel := errors.New("simulated transport boom")
	client := &http.Client{Transport: &errRoundTripper{err: sentinel}}
	sc, err := postJSON(context.Background(), client, "http://example.invalid", []byte("{}"), nil)
	if err == nil {
		t.Fatal("postJSON must surface transport error from c.Do")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0 (no response received)", sc)
	}
	if !errors.Is(err, sentinel) && !strings.Contains(err.Error(), "simulated transport boom") {
		t.Errorf("expected transport error to propagate, got %v", err)
	}
}

// TestEmailSender_PlainAuthBranch covers the `cfg.Username != ""` arm of
// Email.Send — the SMTP PLAIN-auth path. The non-auth happy path is
// already covered by TestEmailSender_SendsComposedMIME; pinning this arm
// catches a regression where admin-supplied credentials get silently
// dropped (the sender would then dial the relay unauthenticated and the
// MTA would 5xx).
func TestEmailSender_PlainAuthBranch(t *testing.T) {
	var gotAuth smtp.Auth
	fake := func(_ string, auth smtp.Auth, _ string, _ []string, _ []byte) error {
		gotAuth = auth
		return nil
	}
	e := &Email{sender: fake}
	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"host":     "smtp.example.com",
		"port":     587,
		"from":     "alerts@example.com",
		"to":       "a@example.com",
		"username": "alerts@example.com",
		"password": "s3cret",
	}}
	if _, err := e.Send(context.Background(), ch, alerting.Alert{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth == nil {
		t.Fatal("PLAIN auth not installed when username is set")
	}
}

// TestEmailSender_DecodeConfigError covers the decodeConfig error arm of
// Email.Send — a wrong-typed `port` (string where int is declared) makes
// the unmarshal fail before any SMTP dial. This is the same admin
// misconfiguration the UI is meant to validate, but the sender must
// fail-closed rather than dial with a zero port.
func TestEmailSender_DecodeConfigError(t *testing.T) {
	e := NewEmail()
	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"port": "not-a-number",
	}}
	sc, err := e.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected decodeConfig error for non-int port")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
}

// TestPagerDutySender_DecodeConfigError covers the decodeConfig error arm
// of PagerDuty.Send — a wrong-typed routingKey causes the same fail-closed
// behaviour as Email and prevents an empty-key POST to PagerDuty.
func TestPagerDutySender_DecodeConfigError(t *testing.T) {
	p := NewPagerDuty(nil)
	ch := alerting.Channel{Type: "pagerduty", Config: map[string]any{
		"routingKey": 12345, // declared as string in pdConfig
	}}
	sc, err := p.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected decodeConfig error for non-string routingKey")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
}

// TestSlackSender_DecodeConfigError covers the decodeConfig error arm of
// Slack.Send — a wrong-typed botToken (numeric) causes fail-closed
// behaviour before any Slack API call.
func TestSlackSender_DecodeConfigError(t *testing.T) {
	s := NewSlack(nil)
	ch := alerting.Channel{Type: "slack", Config: map[string]any{
		"botToken": 12345, // declared as string in slackConfig
	}}
	sc, err := s.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected decodeConfig error for non-string botToken")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
}

// TestWebhookSender_DecodeConfigError covers the decodeConfig error arm
// of Webhook.Send. Without this, a misshapen `headers` field (string
// instead of object) would silently proceed to a no-headers POST.
func TestWebhookSender_DecodeConfigError(t *testing.T) {
	w := NewWebhook(nil)
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{
		"url":     "http://example.com",
		"headers": "not-an-object", // declared map[string]string in webhookConfig
	}}
	sc, err := w.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected decodeConfig error for misshapen headers")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
}

// TestWebhookSender_AlertMarshalError covers the `json.Marshal(a)` error
// arm of Webhook.Send. Alert.Details is `map[string]any`, so a non-JSON
// value (chan) inserted by an upstream rule renderer makes the marshal
// fail before any POST. Pinning it guards against future changes that
// might drop the error check and ship a malformed body.
func TestWebhookSender_AlertMarshalError(t *testing.T) {
	w := NewWebhook(nil)
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{"url": "http://example.com"}}
	a := alerting.Alert{
		ID:      "x",
		Details: map[string]any{"bad": make(chan int)},
	}
	sc, err := w.Send(context.Background(), ch, a)
	if err == nil {
		t.Fatal("expected json.Marshal error for non-marshalable Alert.Details")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("expected marshal error, got %v", err)
	}
}

// TestWebhookSender_NewRequestError covers the `http.NewRequestWithContext`
// error arm of Webhook.Send. A control character in the URL makes
// url.Parse fail. Pinning this guards the path that would otherwise
// surface as a confusing "nil pointer" if the error check were dropped.
func TestWebhookSender_NewRequestError(t *testing.T) {
	w := NewWebhook(nil)
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{
		"url": "http://exa\x7fmple.com",
	}}
	sc, err := w.Send(context.Background(), ch, alerting.Alert{ID: "x"})
	if err == nil {
		t.Fatal("expected NewRequestWithContext to fail on URL with control char")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0", sc)
	}
}

// TestWebhookSender_TransportError covers the `c.Do(req)` error arm of
// Webhook.Send. Mirrors TestPostJSON_TransportError but routed through
// the webhook sender's own client so a regression in either layer is
// caught independently.
func TestWebhookSender_TransportError(t *testing.T) {
	sentinel := fmt.Errorf("simulated webhook transport boom")
	client := &http.Client{Transport: &errRoundTripper{err: sentinel}}
	w := NewWebhook(client)
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{"url": "http://example.invalid"}}
	sc, err := w.Send(context.Background(), ch, alerting.Alert{ID: "x"})
	if err == nil {
		t.Fatal("expected transport error from c.Do")
	}
	if sc != 0 {
		t.Errorf("status: got %d want 0 (no response)", sc)
	}
	if !strings.Contains(err.Error(), "simulated webhook transport boom") {
		t.Errorf("expected transport error to propagate, got %v", err)
	}
}
