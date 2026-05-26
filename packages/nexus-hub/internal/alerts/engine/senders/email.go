package senders

import (
	"context"
	"fmt"
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

type emailConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	From     string `json:"from"`
	To       string `json:"to"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (e *Email) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	cfg, err := decodeConfig[emailConfig](ch.Config)
	if err != nil {
		return 0, err
	}
	if cfg.Host == "" || cfg.Port == 0 || cfg.From == "" || cfg.To == "" {
		return 0, fmt.Errorf("email: host, port, from, to are required")
	}

	subject := fmt.Sprintf("[%s] %s: %s",
		strings.ToUpper(string(a.Severity)), a.RuleID, a.TargetLabel)
	bodyText := fmt.Sprintf("%s\n\nTarget: %s\nRule: %s\nSeverity: %s\n\nFired at: %s\n",
		a.Message, a.TargetLabel, a.RuleID, a.Severity, a.FiredAt.UTC().Format("2006-01-02T15:04:05Z"))

	msg := []byte(
		"From: " + cfg.From + "\r\n" +
			"To: " + cfg.To + "\r\n" +
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
