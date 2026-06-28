package kit

import (
	"fmt"
	"github.com/goccy/go-json"
)

// Dash renders an em dash for an empty string, else the string unchanged — the
// shared "no value" placeholder used by every table/detail cell.
func Dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// Ms formats a millisecond duration compactly: sub-second as "250ms", a second or
// more as "1.5s".
func Ms(v int) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", float64(v)/1000)
	}
	return fmt.Sprintf("%dms", v)
}

// Ktok formats a token/unit count compactly: "512", "200k", "1.5M".
func Ktok(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// OptSession returns the first session in s, or the zero Session when s is empty —
// the shared "pick the resolved session (model/VK), tolerating none" accessor used
// by the session-bearing views.
func OptSession(s []Session) Session {
	if len(s) > 0 {
		return s[0]
	}
	return Session{}
}

// PrettyJSON pretty-prints raw JSON with two-space indentation. Empty input yields
// an empty string; input that is not valid JSON is returned verbatim (best-effort
// display, never an error).
func PrettyJSON(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(body)
	}
	return string(out)
}
