package mq

import "testing"

func TestJetstreamDurableName_distinctPerSubject(t *testing.T) {
	const group = "hub-db-writer"
	a := jetstreamDurableName(group, "nexus.event.compliance")
	b := jetstreamDurableName(group, "nexus.event.admin-audit")
	if a == b {
		t.Fatalf("expected different durable names, got %q for both", a)
	}
	emptyGroup := jetstreamDurableName("", "nexus.event.agent")
	if emptyGroup == jetstreamDurableName("hub-db-writer", "nexus.event.agent") {
		t.Fatalf("empty group must not produce same durable as hub-db-writer for same queue: %q", emptyGroup)
	}
}
