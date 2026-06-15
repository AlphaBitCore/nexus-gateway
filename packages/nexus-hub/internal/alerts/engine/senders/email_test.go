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
		"smtpHost": "smtp.example.com",
		"smtpPort": 587,
		"smtpFrom": "alerts@example.com",
		"smtpTo":   "a@example.com, b@example.com",
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

// TestEmailSender_StripsCRLFFromTargetLabel verifies F-0246: a TargetLabel
// carrying embedded CR/LF cannot inject additional RFC822 headers or smuggle
// body content. The composed message must contain exactly one Subject header
// and the injected header must not appear.
func TestEmailSender_StripsCRLFFromTargetLabel(t *testing.T) {
	var gotMsg []byte
	fake := func(_ string, _ smtp.Auth, _ string, _ []string, msg []byte) error {
		gotMsg = msg
		return nil
	}
	e := &Email{sender: fake}
	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"smtpHost": "h", "smtpPort": 25, "smtpFrom": "f@example.com", "smtpTo": "t@example.com",
	}}
	a := alerting.Alert{
		RuleID:      "quota.threshold",
		TargetLabel: "Org X\r\nBcc: attacker@evil.com\r\nX-Injected: 1",
		Severity:    alerting.SeverityHigh,
		Message:     "body\r\nContent-Type: text/html",
	}
	if _, err := e.Send(context.Background(), ch, a); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgStr := string(gotMsg)
	// The security property: the injected header names must NOT appear at the
	// START of any line (which would make them real headers). The same text
	// appearing INSIDE the de-CRLF'd Subject value is harmless.
	for _, line := range strings.Split(msgStr, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "X-Injected:") {
			t.Fatalf("CRLF injection created a new header line: %q", line)
		}
	}
	// Header section ends at the first blank line; exactly one Subject header,
	// living entirely on one line.
	headerSection := msgStr
	if i := strings.Index(msgStr, "\r\n\r\n"); i >= 0 {
		headerSection = msgStr[:i]
	}
	if strings.Count(headerSection, "Subject:") != 1 {
		t.Fatalf("expected exactly one Subject header, got: %q", headerSection)
	}
	if !strings.Contains(headerSection, "Subject: [HIGH] quota.threshold: Org XBcc: attacker@evil.comX-Injected: 1") {
		t.Fatalf("subject must contain the de-CRLF'd label on one line: %q", headerSection)
	}
}

// TestEmailSender_RejectsMalformedFromAddress verifies the From/To headers are
// validated via net/mail so a control-character address is rejected rather than
// concatenated raw into the header (F-0246).
func TestEmailSender_RejectsMalformedFromAddress(t *testing.T) {
	e := &Email{sender: func(string, smtp.Auth, string, []string, []byte) error { return nil }}
	ch := alerting.Channel{Type: "email", Config: map[string]any{
		"smtpHost": "h", "smtpPort": 25,
		"smtpFrom": "bad\r\nBcc: x@y.com",
		"smtpTo":   "t@example.com",
	}}
	if _, err := e.Send(context.Background(), ch, alerting.Alert{Severity: alerting.SeverityHigh}); err == nil {
		t.Fatal("expected error for malformed from address with CRLF")
	}
}

func TestEmailSender_MissingConfigIsError(t *testing.T) {
	e := NewEmail()
	ch := alerting.Channel{Type: "email", Config: map[string]any{"smtpHost": "x"}}
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
		"smtpHost": "h", "smtpPort": 25, "smtpFrom": "f@x", "smtpTo": "t@x",
	}}
	sc, err := e.Send(context.Background(), ch, alerting.Alert{})
	if err == nil {
		t.Fatal("expected error")
	}
	if sc != 0 {
		t.Fatalf("sc=%d want 0", sc)
	}
}
