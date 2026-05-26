// configdispatch.go wires every shadow config key the desktop Agent
// consumes onto a single shared/transport/configloader.Loader.
//
// Mirrors the pattern proven in compliance-proxy / control-plane /
// ai-gateway (configdispatch.go in each), but adds the Cat B HTTP-pull
// path the Agent uniquely needs — Hub sends `{needsPull:true}` stubs
// for Cat B keys and the Agent must HTTP-pull the live bytes from
// Hub before applying.
//
// Each shadow key is registered as a `rawApply` closure. Main.go
// assembles the closures (so they retain access to the goroutine-
// local subsystems they touch — atomic counters, status collector,
// cfgMgr, thingclient handle), then passes them to buildConfigLoader
// for registration.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// rawApply is the per-key handler shape main.go provides; matches the
// raw signature configloader.RegisterRaw / RegisterRawPull accepts.
type rawApply func(ctx context.Context, raw []byte, ver int64) ([]byte, error)

// configDispatchDeps carries every per-key applier the Agent's
// shadow path consumes. main.go pre-wraps each shadow applier (Cat A
// directly, Cat B via the TeeApplier that records into policiesCache)
// before constructing the deps.
type configDispatchDeps struct {
	Logger      *slog.Logger
	ThingID     string
	Outcomes    *thingclient.OutcomeTracker
	HubHTTPURL  string
	DeviceToken string

	// Cat A — Hub pushes full bytes inline. No HTTP pull.
	KillSwitch    rawApply // killswitch
	AgentSettings rawApply // agent_settings

	// Cat B — Hub pushes `{needsPull:true}` stub; Loader pulls bytes
	// via the HTTP Puller before invoking apply.
	Exemptions          rawApply // exemptions
	InterceptionDomains rawApply // interception_domains
	HookConfig          rawApply // hooks
	PayloadCapture      rawApply // payload_capture
	StreamingCompliance rawApply // streaming_compliance
	InstalledRulePacks  rawApply // installed_rule_packs (view-only)
	UserContext         rawApply // user_context (view-only)
}

// buildConfigLoader returns a Loader pre-populated with every Agent
// shadow key + the HTTP puller closure that translates Cat B "needs
// pull" markers into a Hub HTTP GET against
// /api/internal/things/config/<key>?type=agent.
func buildConfigLoader(d configDispatchDeps) *cfgloader.Loader {
	httpCli := nexushttp.New(nexushttp.Config{
		Timeout:             30 * time.Second,
		Caller:              "agent-configsync",
		PropagateReqID:      true,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		H2ReadIdleTimeout:   30 * time.Second,
		ForceHTTP2:          nexushttp.On(),
	})
	puller := func(ctx context.Context, key string) ([]byte, error) {
		return agentPullConfig(ctx, httpCli, d.HubHTTPURL, d.DeviceToken, d.ThingID, key)
	}

	l := cfgloader.New(d.Logger, d.Outcomes, d.ThingID, "agent",
		cfgloader.WithPuller(puller))

	// Cat A registrations — desired bytes ARE the data.
	cfgloader.RegisterRaw(l, "killswitch", d.KillSwitch)
	cfgloader.RegisterRaw(l, "agent_settings", d.AgentSettings)

	// Cat B registrations — Loader HTTP-pulls before each apply.
	// `exemptions` was historically Cat A (RegisterRaw) but CP's
	// write path uses InvalidateConfig (signal-only); Hub's WS push
	// then carried empty state and the agent applied an empty payload
	// on every signal — silently overwriting admin grants. Cat B
	// (HTTP-pull from Hub's AgentExemptionsLoader) is the canonical
	// flow that matches the CP write contract.
	cfgloader.RegisterRawPull(l, configkey.Exemptions, d.Exemptions)
	cfgloader.RegisterRawPull(l, "interception_domains", d.InterceptionDomains)
	cfgloader.RegisterRawPull(l, configkey.Hooks, d.HookConfig)
	cfgloader.RegisterRawPull(l, "payload_capture", d.PayloadCapture)
	cfgloader.RegisterRawPull(l, "streaming_compliance", d.StreamingCompliance)
	cfgloader.RegisterRawPull(l, "installed_rule_packs", d.InstalledRulePacks)
	cfgloader.RegisterRawPull(l, "user_context", d.UserContext)

	// Acknowledged-external keys — processed by OTHER subsystems
	// (diag drain) rather than by shadow. Hub still pushes them to
	// every agent; without this no-op handler the Loader would
	// WARN-and-skip and spam the log every tick.
	silentNoop := func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		return nil, nil
	}
	cfgloader.RegisterRaw(l, "diag_mode", silentNoop)

	return l
}

// agentPullConfig issues an HTTP GET to Hub's internal config
// endpoint and returns the `state` JSON payload. Errors include the
// HTTP status and a short body excerpt to ease troubleshooting from
// the Hub side; the body is bounded to 1 KiB.
//
// Lifted from the shadow.Manager.pullConfig path before that
// package's Manager was retired. The agent uses Bearer auth (the
// per-device token written by the auth bootstrap step) plus an
// X-Thing-Id header for Hub-side multi-tenancy validation.
func agentPullConfig(ctx context.Context, c *http.Client, hubHTTPURL, deviceToken, thingID, key string) ([]byte, error) {
	url := fmt.Sprintf("%s/api/internal/things/config/%s?type=agent", hubHTTPURL, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	req.Header.Set("X-Thing-Id", thingID)

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return result.State, nil
}
