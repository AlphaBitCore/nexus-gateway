package runtime

import (
	"github.com/goccy/go-json"
	"net/url"
	"strings"
	"time"
)

// windowSchemaProp is the shared JSON-schema property for a time-range argument on
// the analytics/health tools, so a "today" / "last hour" question is answerable
// instead of always reporting the 7-day default.
const windowSchemaProp = `"window":{"type":"string","enum":["1h","24h","today","7d","30d"],"description":"time range (default 7d)"}`

// windowArg extracts the window keyword from a tool input (empty → default 7d).
func windowArg(in json.RawMessage) string {
	var a struct {
		Window string `json:"window"`
	}
	_ = json.Unmarshal(in, &a)
	return a.Window
}

// windowRange maps a window keyword to a [start, end] UTC range. An empty or
// unrecognized keyword is the last 7 days — the analytics endpoints' conventional
// default, preserving prior behavior.
func windowRange(window string) (time.Time, time.Time) {
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "1h":
		return now.Add(-time.Hour), now
	case "24h":
		return now.Add(-24 * time.Hour), now
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), now
	case "30d":
		return now.AddDate(0, 0, -30), now
	default: // "7d" and anything else
		return now.AddDate(0, 0, -7), now
	}
}

// windowValues renders a window keyword as the start/end query the analytics
// endpoints require (RFC3339).
func windowValues(window string) url.Values {
	start, end := windowRange(window)
	return url.Values{
		"start": {start.Format(time.RFC3339)},
		"end":   {end.Format(time.RFC3339)},
	}
}
