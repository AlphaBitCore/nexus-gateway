package mq

import "testing"

func TestStreamNameRouting(t *testing.T) {
	cases := []struct {
		queue string
		want  string
	}{
		{"nexus.event.traffic", "NEXUS_EVENTS"},
		{"nexus.event.audit.admin", "NEXUS_EVENTS"},
		{"nexus.auth.revocation", "NEXUS_AUTH"},
		{"nexus.auth.future.subject", "NEXUS_AUTH"},
		{"nexus.other.subject", "NEXUS_DEFAULT"},
		{"", "NEXUS_DEFAULT"},
	}
	for _, tc := range cases {
		if got := streamName(tc.queue); got != tc.want {
			t.Errorf("streamName(%q) = %q, want %q", tc.queue, got, tc.want)
		}
	}
}
