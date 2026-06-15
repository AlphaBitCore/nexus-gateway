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

// TestWebhookSender_Non2xxCollapsesOracle proves F-0370's oracle collapse: a
// non-2xx upstream status is NOT reflected back (the dispatch row would otherwise
// leak whether an internal endpoint exists / which status it returned). The
// status code is dropped to 0 and the error is a single generic string,
// byte-identical regardless of the upstream status. Uses an unguarded client so
// the 127.0.0.1 httptest target is reachable — the guard is tested separately.
func TestWebhookSender_Non2xxCollapsesOracle(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		ch := alerting.Channel{Type: "webhook", Config: map[string]any{"url": srv.URL}}
		sc, err := senders.NewWebhook(http.DefaultClient).Send(context.Background(), ch, alerting.Alert{ID: "x"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", status)
		}
		if sc != 0 {
			t.Errorf("status %d: sc=%d, want 0 (collapsed)", status, sc)
		}
		if err.Error() != "alert delivery failed" {
			t.Errorf("status %d: err=%q, want generic 'alert delivery failed'", status, err.Error())
		}
	}
}

// TestWebhookSender_DialErrorCollapsesOracle proves a transport-level failure
// (here the SSRF guard refusing a private target via the default guarded client)
// surfaces the SAME generic error as a non-2xx — so a caller cannot distinguish
// "blocked by SSRF guard" from "upstream returned 4xx". NewWebhook(nil) installs
// the AdminEgressExternalOnly guard, which refuses the 127.0.0.1 httptest target.
func TestWebhookSender_DialErrorCollapsesOracle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := alerting.Channel{Type: "webhook", Config: map[string]any{"url": srv.URL}}
	sc, err := senders.NewWebhook(nil).Send(context.Background(), ch, alerting.Alert{ID: "x"})
	if err == nil {
		t.Fatal("expected SSRF-guard dial error for loopback target")
	}
	if sc != 0 {
		t.Errorf("sc=%d, want 0 (collapsed)", sc)
	}
	if err.Error() != "alert delivery failed" {
		t.Errorf("err=%q, want generic 'alert delivery failed' (no raw transport detail)", err.Error())
	}
}

// TestWebhookSender_BlocksMetadataEndpoint proves the default guarded client
// refuses the cloud-metadata endpoint outright.
func TestWebhookSender_BlocksMetadataEndpoint(t *testing.T) {
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{
		"url": "http://169.254.169.254/latest/meta-data/",
	}}
	_, err := senders.NewWebhook(nil).Send(context.Background(), ch, alerting.Alert{ID: "x"})
	if err == nil {
		t.Fatal("webhook to 169.254.169.254 must be blocked")
	}
}

func TestWebhookSender_MissingURLIsError(t *testing.T) {
	ch := alerting.Channel{Type: "webhook", Config: map[string]any{}}
	_, err := senders.NewWebhook(nil).Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}
