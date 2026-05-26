package defs

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// AlertRaiser is the narrow subset of alerting.Raiser used by scheduled jobs.
//
// Jobs depend on this interface (rather than *alerting.Raiser directly) so
// tests can substitute an in-memory fake without standing up the full
// alerting persistence stack. *alerting.Raiser satisfies this interface by
// virtue of exposing the same two methods.
type AlertRaiser interface {
	Raise(ctx context.Context, in alerting.RaiseInput) error
	Resolve(ctx context.Context, ruleID, targetKey, reason string) error
}
