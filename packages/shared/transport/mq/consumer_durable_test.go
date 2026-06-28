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

func TestMaxAckPending_defaultAndEnv(t *testing.T) {
	t.Setenv("NEXUS_MQ_MAX_ACK_PENDING", "")
	if got := maxAckPending(); got != defaultMaxAckPending {
		t.Fatalf("empty env: got %d want default %d", got, defaultMaxAckPending)
	}
	t.Setenv("NEXUS_MQ_MAX_ACK_PENDING", "12345")
	if got := maxAckPending(); got != 12345 {
		t.Fatalf("valid env: got %d want 12345", got)
	}
	// Non-positive and unparseable values must fall back to the default, never 0
	// (a 0 MaxAckPending means "unlimited" in JetStream — a footgun we refuse).
	for _, bad := range []string{"0", "-5", "abc", "  "} {
		t.Setenv("NEXUS_MQ_MAX_ACK_PENDING", bad)
		if got := maxAckPending(); got != defaultMaxAckPending {
			t.Fatalf("bad env %q: got %d want default %d", bad, got, defaultMaxAckPending)
		}
	}
}
