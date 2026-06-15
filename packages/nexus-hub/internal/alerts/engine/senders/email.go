package senders

import (
	"context"
	"fmt"
	"net/mail"
	"net/smtp"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// Email delivers alerts via SMTP. The dial-and-send function is indirected
// through the `sender` field so tests can stub it without running a real
// SMTP server.
type Email struct {
	// sender is the dial+send function, indirected for testability.
	sender func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error
}

func NewEmail() *Email { return &Email{sender: smtp.SendMail} }

// emailConfig keys mirror exactly what the admin UI persists (smtp* prefix);
// the masking and at-rest-encryption layers key off the same names, so the
// sender, the store, and the form stay on one canonical key set.
type emailConfig struct {
	Host     string `json:"smtpHost"`
	Port     int    `json:"smtpPort"`
	From     string `json:"smtpFrom"`
	To       string `json:"smtpTo"`
	Username string `json:"smtpUsername,omitempty"`
	Password string `json:"smtpPassword,omitempty"`
}

func (e *Email) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	cfg, err := decodeConfig[emailConfig](ch.Config)
	if err != nil {
		return 0, err
	}
	if cfg.Host == "" || cfg.Port == 0 || cfg.From == "" || cfg.To == "" {
		return 0, fmt.Errorf("email: host, port, from, to are required")
	}

	// TargetLabel / RuleID / Message are attacker-influenced (a device name,
	// rule id, or alert message can carry embedded CR/LF). Strip line breaks
	// before they reach the RFC822 header (Subject) AND the body so a crafted
	// value cannot inject extra headers or smuggle MIME parts.
	severity := stripCRLF(strings.ToUpper(string(a.Severity)))
	subject := fmt.Sprintf("[%s] %s: %s",
		severity, stripCRLF(a.RuleID), stripCRLF(a.TargetLabel))
	bodyText := fmt.Sprintf("%s\n\nTarget: %s\nRule: %s\nSeverity: %s\n\nFired at: %s\n",
		stripCRLF(a.Message), stripCRLF(a.TargetLabel), stripCRLF(a.RuleID),
		severity, a.FiredAt.UTC().Format("2006-01-02T15:04:05Z"))

	// Encode From/To via net/mail so addresses are RFC5322-formatted (and any
	// stray control characters in a configured address are rejected/escaped
	// rather than concatenated raw into the header).
	fromHeader, err := formatAddressList(cfg.From)
	if err != nil {
		return 0, fmt.Errorf("email: invalid from address: %w", err)
	}
	toHeader, err := formatAddressList(cfg.To)
	if err != nil {
		return 0, fmt.Errorf("email: invalid to address: %w", err)
	}

	msg := []byte(
		"From: " + fromHeader + "\r\n" +
			"To: " + toHeader + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"\r\n" +
			bodyText,
	)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	to := splitAndTrim(cfg.To)
	if err := e.sender(addr, auth, cfg.From, to, msg); err != nil {
		return 0, fmt.Errorf("email: send: %w", err)
	}
	return 250, nil // SMTP 250 = OK
}

// stripCRLF removes carriage-return and line-feed characters so an
// attacker-controlled value cannot break out of a single RFC822 header line or
// inject additional headers / body content.
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// formatAddressList parses a comma-separated address string and re-emits it via
// net/mail so each address is canonically encoded. Parsing rejects addresses
// carrying control characters (CR/LF), closing the header-injection vector on
// the From/To headers. Returns an error if any address is malformed or the
// list is empty.
func formatAddressList(csv string) (string, error) {
	parts := splitAndTrim(csv)
	if len(parts) == 0 {
		return "", fmt.Errorf("no addresses")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		addr, err := mail.ParseAddress(p)
		if err != nil {
			return "", err
		}
		out = append(out, addr.String())
	}
	return strings.Join(out, ", "), nil
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
