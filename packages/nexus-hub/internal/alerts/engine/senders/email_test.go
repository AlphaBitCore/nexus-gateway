package senders

import (
	"context"
	"net/smtp"
	"strings"
	"testing"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// NOTE: this test file is package `senders` (white-box) so it can poke the
// unexported `sender` field. The black-box tests for other senders live in
// `senders_test`; email is an exception because stubbing net/smtp.SendMail
// in-process is the cleanest way to assert on composed MIME.

func TestEmailSender_SendsComposedMIME(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	fake := func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr = addr
		gotFrom = from
		gotTo = to
		gotMsg = msg
		return nil
	}
	e := &Email{sender: fake}

	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"host": "smtp.example.com",
		"port": 587,
		"from": "alerts@example.com",
		"to":   "a@example.com, b@example.com",
	}}
	a := alerting.Alert{
		RuleID: "quota.threshold", TargetLabel: "Org X",
		Severity: alerting.SeverityHigh, Message: "95% used",
	}

	sc, err := e.Send(context.Background(), ch, a)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sc != 250 {
		t.Fatalf("sc=%d", sc)
	}
	if gotAddr != "smtp.example.com:587" {
		t.Fatalf("addr=%q", gotAddr)
	}
	if gotFrom != "alerts@example.com" {
		t.Fatalf("from=%q", gotFrom)
	}
	if len(gotTo) != 2 || gotTo[0] != "a@example.com" || gotTo[1] != "b@example.com" {
		t.Fatalf("to=%v", gotTo)
	}
	msgStr := string(gotMsg)
	if !strings.Contains(msgStr, "Subject: [HIGH] quota.threshold: Org X") {
		t.Fatalf("subject missing: %s", msgStr)
	}
	if !strings.Contains(msgStr, "95% used") {
		t.Fatalf("message body missing: %s", msgStr)
	}
}

func TestEmailSender_MissingConfigIsError(t *testing.T) {
	e := NewEmail()
	ch := alerting.Channel{Type: "email", Config: map[string]any{"host": "x"}}
	_, err := e.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error for missing to/from/port")
	}
}

func TestEmailSender_SenderFailurePropagates(t *testing.T) {
	fake := func(string, smtp.Auth, string, []string, []byte) error {
		return context.DeadlineExceeded
	}
	e := &Email{sender: fake}
	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"host": "h", "port": 25, "from": "f@x", "to": "t@x",
	}}
	sc, err := e.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error")
	}
	if sc != 0 {
		t.Fatalf("sc=%d want 0", sc)
	}
}
