package tui

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// askAction is the closed set of things the NL bar can do. There is deliberately
// NO write action: the askbar executor has no arm that mutates, so a hallucinated
// mutation parses to actionUnknown and fires nothing. This is the safety boundary.
type askAction string

const (
	actionNavigate askAction = "navigate"
	actionAnswer   askAction = "answer"
	actionExplain  askAction = "explain"
	actionUnknown  askAction = "unknown"
)

// answerSource is the closed set of read data sources the answer path can fetch.
type answerSource string

const (
	sourceCost   answerSource = "cost"
	sourceErrors answerSource = "errors"
	sourceSLO    answerSource = "slo"
	sourceFleet  answerSource = "fleet"
)

// askFilter is the optional Radar filter carried by a navigate intent.
type askFilter struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Since    string `json:"since"`
}

// askIntent is the validated result of routing one NL question. It is produced
// only by parseIntent over the model's JSON reply; every field is checked against
// a closed enum, so an out-of-range value collapses to actionUnknown.
type askIntent struct {
	Action  askAction    `json:"action"`
	View    string       `json:"view"`
	Filter  *askFilter   `json:"filter"`
	Source  answerSource `json:"source"`
	EventID string       `json:"event_id"`
}

// parseIntent decodes the router model's JSON reply into a validated askIntent.
// Bad JSON, an unknown action, a navigate without a view, an answer without a
// known source, or an explain without an id all become actionUnknown so the
// executor does nothing surprising.
func parseIntent(raw []byte) askIntent {
	obj := extractJSONObject(raw)
	if obj == nil {
		return askIntent{Action: actionUnknown}
	}
	var in askIntent
	if err := json.Unmarshal(obj, &in); err != nil {
		return askIntent{Action: actionUnknown}
	}
	switch in.Action {
	case actionNavigate:
		if strings.TrimSpace(in.View) == "" {
			return askIntent{Action: actionUnknown}
		}
		return askIntent{Action: actionNavigate, View: strings.TrimSpace(in.View), Filter: in.Filter}
	case actionAnswer:
		if !validSource(in.Source) {
			return askIntent{Action: actionUnknown}
		}
		return askIntent{Action: actionAnswer, Source: in.Source}
	case actionExplain:
		if strings.TrimSpace(in.EventID) == "" {
			return askIntent{Action: actionUnknown}
		}
		return askIntent{Action: actionExplain, EventID: strings.TrimSpace(in.EventID)}
	default:
		return askIntent{Action: actionUnknown}
	}
}

func validSource(s answerSource) bool {
	switch s {
	case sourceCost, sourceErrors, sourceSLO, sourceFleet:
		return true
	}
	return false
}

// extractJSONObject returns the first balanced {...} span in raw, tolerating a
// model that wraps its JSON in a ```json fence or a sentence. It is string-aware
// so a brace inside a JSON string value does not unbalance the scan.
func extractJSONObject(raw []byte) []byte {
	s := string(raw)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return []byte(s[start : i+1])
			}
		}
	}
	return nil
}

// filterFromIntent maps a navigate intent's optional filter onto a
// core.TrafficFilter. Unrecognized fields are dropped (not errored) so a partial
// or fuzzy model reply still narrows what it can.
func filterFromIntent(f *askFilter, now time.Time) core.TrafficFilter {
	out := core.TrafficFilter{Limit: 20}
	if f == nil {
		return out
	}
	out.Provider = strings.TrimSpace(f.Provider)
	switch strings.ToLower(strings.TrimSpace(f.Status)) {
	case "4xx", "5xx", "error":
		out.StatusRange = strings.ToLower(strings.TrimSpace(f.Status))
	}
	if d, ok := parseSince(f.Since); ok {
		out.StartTime = now.Add(-d)
	}
	return out
}

func parseSince(s string) (time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1h", "hour", "lasthour", "last-hour":
		return time.Hour, true
	case "24h", "1d", "day", "today":
		return 24 * time.Hour, true
	case "7d", "week":
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// answerDataMax bounds the JSON fed to the summary call so a large read result
// cannot blow the prompt budget.
const answerDataMax = 6000

// buildRouterPrompt frames the system instruction that turns a question into a
// single JSON intent. It enumerates the live view names so the model can only
// navigate to a real view, and it never offers a write/mutation action.
func buildRouterPrompt(entries []viewEntry) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return "You are the router for a terminal operator console for an AI gateway. " +
		"Convert the user's question into ONE JSON object and output nothing else.\n" +
		`Schema: {"action":"navigate|answer|explain|unknown","view":"<name>",` +
		`"filter":{"provider":"","status":"4xx|5xx|error","since":"1h|24h|7d"},` +
		`"source":"cost|errors|slo|fleet","event_id":""}` + "\n" +
		"Use \"navigate\" to jump to a view; view MUST be one of: " + strings.Join(names, ", ") + ". " +
		"\"filter\" is only honored when navigating to Radar. " +
		"Use \"answer\" for questions needing a data summary, choosing source: " +
		"cost (spend/providers), errors (failures/blocks/alerts), slo (latency), fleet (nodes/sync). " +
		"Use \"explain\" only when the user names a specific event id (set event_id). " +
		"If unsure, return {\"action\":\"unknown\"}. You can only read; never produce a mutation."
}

// buildAnswerPrompt frames the second (summary) call: answer the operator's
// question from the fetched JSON data only, concisely.
func buildAnswerPrompt(question string, data []byte) string {
	d := string(data)
	if len(d) > answerDataMax {
		d = d[:answerDataMax]
	}
	return "You are an SRE assistant for an AI gateway. Answer the operator's question in 2-3 " +
		"sentences using ONLY the JSON data below; cite concrete numbers. If the data does not " +
		"contain the answer, say so briefly.\n\nQuestion: " + question + "\n\nData:\n" + d
}
