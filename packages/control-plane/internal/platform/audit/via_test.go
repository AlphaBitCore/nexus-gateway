package audit

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/subagentmark"
)

// TestViaFor pins the audit via composition: the bare initiator
// channel for an ordinary call, and the channel suffixed with the sub-agent marker
// for a child agent's in-process tool call ("assistant ▸ subagent 2"). An empty
// channel with a marker (defensive) returns the bare label; an empty context
// returns "" so existing rows are byte-identical.
func TestViaFor(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"plain", context.Background(), ""},
		{"assistant", initiator.With(context.Background(), initiator.ViaAssistant), "assistant"},
		{
			"assistant+subagent",
			subagentmark.With(initiator.With(context.Background(), initiator.ViaAssistant), "subagent 2"),
			"assistant ▸ subagent 2",
		},
		{
			"marker-without-channel",
			subagentmark.With(context.Background(), "subagent 3"),
			"subagent 3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := viaFor(tc.ctx); got != tc.want {
				t.Errorf("viaFor = %q; want %q", got, tc.want)
			}
		})
	}
}
