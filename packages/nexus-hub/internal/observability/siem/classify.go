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

// ClassifyAdminEvent returns an event-type string for an AdminAuditLog row.
// Format: "{entityType}.{action}". Returns "" if action or entityType is empty.
func ClassifyAdminEvent(evt Event) string {
	action, _ := evt["action"].(string)
	entityType, _ := evt["entityType"].(string)
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
