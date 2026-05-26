package aggregators

import (
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
)

// CredentialAuthFailuresCascade fires when the ratio of 401/403 with no
// Nexus-side classification (so the upstream provider rejected the
// underlying credential) exceeds threshold per credential_id. Catches
// credential rotation needs and revoked-by-provider keys.
type CredentialAuthFailuresCascade struct{}

func NewCredentialAuthFailuresCascade() *CredentialAuthFailuresCascade {
	return &CredentialAuthFailuresCascade{}
}

func (a *CredentialAuthFailuresCascade) RuleID() string { return "credential.auth_failures_cascade" }

func (a *CredentialAuthFailuresCascade) Sources() []alerteval.EventSource {
	return []alerteval.EventSource{alerteval.SourceAITraffic}
}

func (a *CredentialAuthFailuresCascade) MinWarmupSec(params map[string]any) int {
	return intParam(params, "windowSec", 600)
}

// extractCredentialID returns the UPSTREAM provider's API credential
// ID — the real OpenAI/Anthropic token Nexus used to make the upstream
// call. This alert evaluates per-credential cascading 401/403s
// ("Credential 401/403 cascade … rotate or revoke?"), so it groups by
// the credential that's actually being rejected by the upstream, NOT
// by the client's Virtual Key (which is a separate dimension on
// identity.vk).
//
// Historic mistake: this used to JSON-decode identity.credential.id,
// which has never existed in the actual data — ai-gateway only writes
// {vk, user, project, status, apiCredential}. The alert silently
// grouped every event under empty string and was effectively dead for
// however long it's been deployed. Two fixes here:
//   1. Read from the dedicated top-level credential_id column (carried
//      on TrafficEventMessage as CredentialID) — cheaper than decoding
//      the identity JSONB and can be indexed if hot.
//   2. Keep the function name + alert name "Credential…" since they
//      semantically refer to the upstream apiCredential, not the VK.
func extractCredentialID(t *consumer.TrafficEventMessage) string {
	if t == nil || t.CredentialID == nil {
		return ""
	}
	return *t.CredentialID
}

func (a *CredentialAuthFailuresCascade) OnEvent(rt *alerteval.Runtime, evt *alerteval.Event) {
	if evt.Kind != alerteval.EventTraffic || evt.Traffic == nil {
		return
	}
	t := evt.Traffic
	credID := extractCredentialID(t)
	if credID == "" {
		return
	}
	w := rt.Window("cred:"+credID, 1200)
	sc := derefInt(t.StatusCode)
	isAuthFail := (sc == 401 || sc == 403) && derefString(t.ErrorCode) == ""
	if isAuthFail {
		w.Add(evt.Timestamp, 1, 1)
	} else {
		w.Add(evt.Timestamp, 0, 1)
	}
}

func (a *CredentialAuthFailuresCascade) Tick(rt *alerteval.Runtime, params map[string]any, now time.Time) []alerteval.Decision {
	windowSec := intParam(params, "windowSec", 600)
	thresholdPct := intParam(params, "thresholdPct", 20)
	minSamples := intParam(params, "minSamples", 10)
	var out []alerteval.Decision
	for _, target := range rt.Targets() {
		msg := fmt.Sprintf("Credential 401/403 cascade: %d%% upstream auth failures over %ds — rotate or revoke?", thresholdPct, windowSec)
		if d := EvalRatioInWindow(rt, target, windowSec, thresholdPct, minSamples, now, msg); d != nil {
			out = append(out, *d)
		}
	}
	return out
}
