package siem

// ClassifyTrafficEvent returns an event-type string for a traffic_event row.
func ClassifyTrafficEvent(evt Event) string {
	decision, _ := evt["hookDecision"].(string)
	reasonCode, _ := evt["hookReasonCode"].(string)

	switch decision {
	case "block":
		switch reasonCode {
		case "rate_limited":
			return "traffic.rate_limited"
		case "budget_exceeded":
			return "traffic.budget_exceeded"
		default:
			return "traffic.request_blocked"
		}
	case "allow":
		return "traffic.allowed"
	case "":
		return "traffic.passthrough"
	default:
		return "traffic.unknown"
	}
}

// Cataloged event-type identities for admin audit rows whose raw
// (entityType, action) pair does not follow the canonical
// "<resource>.<verb>" shape, so the generic rule below would otherwise yield a
// non-cataloged eventType ("" or e.g. "thing.thing_override_set") that any SIEM
// whitelist silently drops and that is absent from the admin filter picker.
// The mapping gives them stable, picker-listed identities:
//
//   - login events carry a dotted action ("admin.login.failed/.succeeded") and
//     an absent or inconsistent entityType → map to the canonical auth.* names
//     the picker hand-lists and the CEF/syslog severity table recognises.
//   - node override / break-glass writes use the legacy internal "thing" /
//     "thing_override_set" vocabulary → map to the canonical node.write-override
//     catalog identity (the config-sync / kill-switch override path
//     at the SIEM layer).
const (
	eventTypeLoginFailure = "auth.login_failure"
	eventTypeLoginSuccess = "auth.login_success"
	eventTypeNodeOverride = "node.write-override"
)

// ClassifyAdminEvent returns a cataloged event-type string for an AdminAuditLog
// row. Canonical rows derive their type as "{entityType}.{action}"; a handful of
// legacy-shaped events (login, node override) are mapped to their cataloged
// identities first so they are not dropped under a SIEM whitelist.
// Returns "" only when neither a known mapping nor a canonical pair applies.
func ClassifyAdminEvent(evt Event) string {
	action, _ := evt["action"].(string)
	entityType, _ := evt["entityType"].(string)

	switch action {
	case "admin.login.failed":
		return eventTypeLoginFailure
	case "admin.login.succeeded":
		return eventTypeLoginSuccess
	}
	if entityType == "thing" && action == "thing_override_set" {
		return eventTypeNodeOverride
	}

	if action == "" || entityType == "" {
		return ""
	}
	return entityType + "." + action
}

// FilterByEventTypes returns only those events whose "eventType" field is
// present in allowedTypes. If allowedTypes is nil or empty, all events are
// returned unchanged (backward-compatible behaviour).
func FilterByEventTypes(events []Event, allowedTypes []string) []Event {
	if len(allowedTypes) == 0 {
		return events
	}
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}
	result := make([]Event, 0, len(events))
	for _, e := range events {
		et, _ := e["eventType"].(string)
		if _, ok := allowed[et]; ok {
			result = append(result, e)
		}
	}
	return result
}
