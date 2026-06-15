package assistant

import (
	"context"
	"errors"
)

// bearerTokenSource attaches the calling web user's bearer token verbatim to the
// assistant's in-process admin self-calls, so every tool the agent runs executes
// AS THAT USER — IAM, audit actor, and validation are inherited from the existing
// admin middleware (binding invariant I1: no privilege escalation). It is NEVER a
// service account / bootstrap / dev principal: the agent's reach is exactly the
// caller's IAM policy.
//
// These self-calls are dispatched IN-PROCESS (see internal/platform/selfdispatch): no
// loopback HTTP hop, and the transport stamps the originating web user's RealIP on
// the synthetic request, so the audit row and any IAM condition keyed on
// nexus:SourceIp see the real client IP rather than the loopback. The identity
// (actor) is the forwarded bearer, validated by the same AdminAuth middleware.
type bearerTokenSource struct {
	// authorization is the full "Authorization" header value, e.g. "Bearer eyJ...".
	authorization string
}

func newBearerTokenSource(authorization string) bearerTokenSource {
	return bearerTokenSource{authorization: authorization}
}

// Credential satisfies core.TokenSource. It returns the Authorization header with
// the caller's bearer unchanged.
func (b bearerTokenSource) Credential(context.Context) (string, string, error) {
	if b.authorization == "" {
		return "", "", errors.New("assistant: no caller bearer to forward (not authenticated)")
	}
	return "Authorization", b.authorization, nil
}
