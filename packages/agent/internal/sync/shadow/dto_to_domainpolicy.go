package shadow

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

// ToDomainPolicy converts the agent's wire-format InterceptionDomainDTO
// list into the shared/domain.InterceptionDomain shape so the agent
// can construct a domain.Engine and pass it into shared/tlsbump
// alongside the cp ingress. The eight Streaming_*/Capture_*/RawBodySpill
// per-host override columns flow through as nullable pointers; NULL on
// any field falls back to the global StreamingPolicy / payloadcapture
// defaults — same behaviour cp's resolver gives a domain row with no
// overrides set.
//
// PathAction enum values flow through unchanged because the DB schema
// uses uppercase strings (PROCESS / PASSTHROUGH / BLOCK) that match
// domain.PathActionXxx exactly.
func (s *ConfigSnapshot) ToDomainPolicy() []domain.InterceptionDomain {
	if s == nil {
		return nil
	}
	out := make([]domain.InterceptionDomain, 0, len(s.InterceptionDomains))
	for _, d := range s.InterceptionDomains {
		row := domain.InterceptionDomain{
			ID:                      d.ID,
			Name:                    d.Name,
			HostPattern:             d.HostPattern,
			HostMatchType:           domain.HostMatchType(d.HostMatchType),
			AdapterID:               d.AdapterID,
			NetworkZone:             domain.NetworkZone(d.NetworkZone),
			DefaultPathAction:       domain.PathAction(d.DefaultPathAction),
			OnAdapterError:          domain.AdapterErrorBehavior(d.OnAdapterError),
			Enabled:                 d.Enabled,
			Priority:                d.Priority,
			UpdatedAt:               time.Now().UTC(),
			StreamingMode:           d.StreamingMode,
			StreamingChunkBytes:     d.StreamingChunkBytes,
			StreamingHookTimeoutMs:  d.StreamingHookTimeoutMs,
			StreamingMaxBufferBytes: d.StreamingMaxBufferBytes,
			StreamingFailBehavior:   d.StreamingFailBehavior,
			CaptureRequestBody:      d.CaptureRequestBody,
			CaptureResponseBody:     d.CaptureResponseBody,
			RawBodySpillEnabled:     d.RawBodySpillEnabled,
		}
		row.Paths = make([]domain.InterceptionPath, 0, len(d.Paths))
		for _, p := range d.Paths {
			if !p.Enabled {
				continue
			}
			row.Paths = append(row.Paths, domain.InterceptionPath{
				ID:          p.ID,
				PathPattern: p.PathPattern,
				MatchType:   domain.PathMatchType(p.MatchType),
				Action:      domain.PathAction(p.Action),
			})
		}
		out = append(out, row)
	}
	return out
}
