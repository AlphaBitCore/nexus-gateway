package tlsbump

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// RejectLevel controls how much information is included in reject responses.
type RejectLevel int

const (
	// RejectLevelStealth returns a generic 403 with no explanation.
	RejectLevelStealth RejectLevel = 0
	// RejectLevelStandard includes a blocked message, contact info, and transaction ID.
	RejectLevelStandard RejectLevel = 1
	// RejectLevelDetailed includes everything from Standard plus the specific reason.
	RejectLevelDetailed RejectLevel = 2
)

// RejectConfig holds reject response configuration.
type RejectConfig struct {
	DefaultLevel RejectLevel
	ContactInfo  string
}

// rejectJSONLevel1 is the JSON response body for level 1 rejections.
type rejectJSONLevel1 struct {
	Error         string `json:"error"`
	Message       string `json:"message"`
	Contact       string `json:"contact,omitempty"`
	TransactionID string `json:"transactionId,omitempty"`
}

// rejectJSONLevel2 extends level 1 with specific reason details.
type rejectJSONLevel2 struct {
	Error         string `json:"error"`
	Message       string `json:"message"`
	Contact       string `json:"contact,omitempty"`
	TransactionID string `json:"transactionId,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ReasonCode    string `json:"reasonCode,omitempty"`
}

// WriteRejectResponse writes a reject response to the client based on the configured level.
// For JSON clients (Accept: application/json), returns JSON.
// For browser/other clients, returns HTML.
func WriteRejectResponse(
	w http.ResponseWriter,
	r *http.Request,
	cfg RejectConfig,
	transactionID string,
	reason string,
	reasonCode string,
	statusCode int,
) {
	switch cfg.DefaultLevel {
	case RejectLevelStealth:
		http.Error(w, "Forbidden", statusCode)
		return

	case RejectLevelStandard:
		if wantsJSON(r) {
			writeJSONResponse(w, statusCode, rejectJSONLevel1{
				Error:         "blocked_by_policy",
				Message:       "Request blocked by enterprise AI security policy",
				Contact:       cfg.ContactInfo,
				TransactionID: transactionID,
			})
		} else {
			writeHTMLResponse(w, statusCode, cfg.ContactInfo, transactionID, "", "")
		}
		return

	case RejectLevelDetailed:
		if wantsJSON(r) {
			writeJSONResponse(w, statusCode, rejectJSONLevel2{
				Error:         "blocked_by_policy",
				Message:       "Request blocked by enterprise AI security policy",
				Contact:       cfg.ContactInfo,
				TransactionID: transactionID,
				Reason:        reason,
				ReasonCode:    reasonCode,
			})
		} else {
			writeHTMLResponse(w, statusCode, cfg.ContactInfo, transactionID, reason, reasonCode)
		}
		return

	default:
		// Unknown level falls back to stealth.
		http.Error(w, "Forbidden", statusCode)
	}
}

// wantsJSON checks the Accept header to determine if the client prefers JSON.
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		// Default to JSON when no Accept header is present (API clients).
		return true
	}
	// Check if JSON appears before HTML in the Accept header.
	jsonIdx := strings.Index(strings.ToLower(accept), "application/json")
	htmlIdx := strings.Index(strings.ToLower(accept), "text/html")
	if jsonIdx >= 0 && (htmlIdx < 0 || jsonIdx < htmlIdx) {
		return true
	}
	if htmlIdx >= 0 {
		return false
	}
	// Wildcard or unrecognized — default to JSON.
	return true
}

// writeJSONResponse marshals the payload and writes it as a JSON HTTP response.
func writeJSONResponse(w http.ResponseWriter, statusCode int, payload interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "Forbidden", statusCode)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

// writeHTMLResponse writes a styled HTML reject page.
func writeHTMLResponse(
	w http.ResponseWriter,
	statusCode int,
	contactInfo string,
	transactionID string,
	reason string,
	reasonCode string,
) {
	var reasonBlock string
	if reason != "" {
		reasonBlock = fmt.Sprintf(`<p style="margin:8px 0 0;font-size:0.9em;color:#666;">Reason: %s</p>`, htmlEscape(reason))
	}
	if reasonCode != "" {
		reasonBlock += fmt.Sprintf(`<p style="margin:4px 0 0;font-size:0.85em;color:#888;">Code: %s</p>`, htmlEscape(reasonCode))
	}

	var contactBlock string
	if contactInfo != "" {
		contactBlock = fmt.Sprintf(`<p style="margin:16px 0 0;font-size:0.9em;color:#555;">%s</p>`, htmlEscape(contactInfo))
	}

	var txBlock string
	if transactionID != "" {
		txBlock = fmt.Sprintf(`<p style="margin:8px 0 0;font-size:0.8em;color:#999;">Transaction ID: %s</p>`, htmlEscape(transactionID))
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Request Blocked</title>
<style>
body{margin:0;padding:40px 20px;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#f5f5f5;color:#333;display:flex;justify-content:center;align-items:flex-start;min-height:100vh}
.container{max-width:520px;width:100%%;background:#fff;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.08);padding:32px;text-align:center}
h1{margin:0 0 12px;font-size:1.4em;color:#d32f2f}
.msg{margin:0;font-size:1em;color:#444}
</style>
</head>
<body>
<div class="container">
<h1>Request Blocked</h1>
<p class="msg">This request has been blocked by enterprise AI security policy.</p>
%s%s%s
</div>
</body>
</html>`, reasonBlock, contactBlock, txBlock)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(html))
}

// htmlEscaper is a package-level Replacer for HTML entity escaping.
// Constructed once; safe for concurrent use.
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

// htmlEscape escapes special HTML characters to prevent injection.
func htmlEscape(s string) string {
	return htmlEscaper.Replace(s)
}
