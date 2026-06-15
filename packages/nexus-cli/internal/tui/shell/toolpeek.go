package shell

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// renderToolBlock renders one tool call for the transcript: a bright, input-forward
// action line (the tool + its key arguments), then — once the result lands — either
// a dim one-line result peek (collapsed) or the full input/output (verbose, capped).
// An error tints the detail amber. The action line is always shown so the operator
// sees WHAT ran even before the result arrives.
func renderToolBlock(ln convLine, verbose bool, width int) string {
	head := lipgloss.NewStyle().Foreground(styles.Green).Render("▸ " + toolActionLabel(ln.toolName, ln.toolInput))
	headWrapped := kit.WrapText(head, width)
	if !ln.toolDone {
		return headWrapped // still running — the peek/detail fills in on the result
	}
	dim := styles.TileLabel
	if ln.toolErr {
		dim = lipgloss.NewStyle().Foreground(styles.Amber)
	}
	if !verbose {
		return headWrapped + "\n" + kit.WrapText(dim.Render("  └ "+toolResultPeek(ln.toolName, ln.toolOutput, ln.toolErr)), width)
	}
	// Verbose: the full input + output, dim, each capped so a 360-op result never
	// floods the pane (the cockpit dashboard views are the place for full data).
	var b strings.Builder
	b.WriteString(headWrapped)
	if in := compact(strings.TrimSpace(string(ln.toolInput))); in != "" && in != "{}" {
		b.WriteString("\n" + kit.WrapText(dim.Render("  in   "+truncate(in, 240)), width))
	}
	b.WriteString("\n" + kit.WrapText(dim.Render("  out  "+truncate(compact(strings.TrimSpace(string(ln.toolOutput))), 480)), width))
	return b.String()
}

// toolActionLabel is the bright, input-forward summary of a call: the tool name plus
// its most-meaningful argument(s). The resource cascade leads with kind /
// operationId; the rest fall back to a compacted, truncated input.
func toolActionLabel(name string, input []byte) string {
	g := func(k string) string { return gjson.GetBytes(input, k).String() }
	switch name {
	case "analyze_cost":
		s := "analyze_cost"
		if w := g("window"); w != "" {
			s += " · " + w
		}
		if gb := g("groupBy"); gb != "" {
			s += ", by " + gb
		}
		return s
	case "analyze_slo", "analyze_compliance", "observe_health", "observe_traffic_list":
		if w := g("window"); w != "" {
			return name + " · " + w
		}
		return name
	case "resource_search":
		return "resource_search · " + truncate(g("query"), 40)
	case "resource_describe":
		return "resource_describe · " + g("kind")
	case "resource_read", "resource_invoke":
		s := name + " · " + g("kind")
		if op := g("operationId"); op != "" {
			s += " / " + op
		}
		return s
	case "mitigate_routing_rule_enabled":
		return "routing rule " + g("rule") + " → " + onOff(g("enabled"))
	case "mitigate_provider_enabled":
		return "provider " + g("provider") + " → " + onOff(g("enabled"))
	case "mitigate_kill_switch":
		return "kill switch → " + onOff(g("engage"))
	case "mitigate_vk_revoke":
		return "revoke virtual key " + g("vk")
	case "mitigate_passthrough_global":
		return "global passthrough → " + onOff(g("enabled"))
	case "navigate":
		return "navigate · " + g("view")
	case "show_event":
		return "show_event · " + g("id")
	case "route_explain":
		return "route_explain · " + g("model")
	case "simulate_request":
		return "simulate_request · " + g("model")
	}
	// Generic: lead with the name, then the compacted input (truncated).
	if s := compact(strings.TrimSpace(string(input))); s != "" && s != "{}" {
		return name + " · " + truncate(s, 50)
	}
	return name
}

// toolResultPeek is the dim, secondary one-liner: an error message, a row count for a
// collection, or the first line of the output, all truncated.
func toolResultPeek(name string, output []byte, isError bool) string {
	s := strings.TrimSpace(string(output))
	if s == "" {
		return "(no output)"
	}
	if isError {
		return "⚠ " + truncate(firstLine(s), 80)
	}
	// Collections read as "N rows / items" rather than dumping the array.
	if d := gjson.GetBytes(output, "data"); d.IsArray() {
		return fmt.Sprintf("%d rows", len(d.Array()))
	}
	if gjson.ValidBytes(output) {
		r := gjson.ParseBytes(output)
		if r.IsArray() {
			return fmt.Sprintf("%d items", len(r.Array()))
		}
		if r.IsObject() {
			// A single object result (one event, one node detail): a pretty-printed
			// body's first line is a bare "{", which tells the operator nothing. Collapse
			// it to a one-line preview so the collapsed peek shows real fields; the full
			// body is one ctrl+t away. An empty object reads as "(no output)".
			c := compact(s)
			if c == "{}" {
				return "(no output)"
			}
			return truncate(c, 80)
		}
	}
	return truncate(firstLine(s), 80)
}

func onOff(boolStr string) string {
	if boolStr == "true" {
		return "on"
	}
	return "off"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// compact collapses runs of whitespace/newlines so a pretty-printed JSON body reads
// as one flowing line before truncation.
func compact(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
