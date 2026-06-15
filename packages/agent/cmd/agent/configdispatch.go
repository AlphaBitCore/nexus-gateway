// configdispatch.go wires every shadow config key the desktop Agent
// consumes onto a single shared/transport/configloader.Loader.
//
// Mirrors the pattern proven in compliance-proxy / control-plane /
// ai-gateway (configdispatch.go in each), but adds the Cat B HTTP-pull
// path the Agent uniquely needs. For Cat B keys the Hub pushes only
// minimal state bytes over WebSocket; the client side drives a real
// HTTP pull (GET /api/internal/things/config/<key>) to fetch the full
// payload before applying. The pull is triggered by the configloader's
// RegisterRawPull handler flag — `needsPull` is a client-side concept
// in configloader, NOT a wire field the Hub stamps into the payload.
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

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
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
	DiagMode      rawApply // diag_mode (per-thing override carrying {until})

	// Cat B — Hub pushes minimal state over WS; the client's
	// RegisterRawPull handler flag triggers an HTTP pull from Hub
	// (GET /api/internal/things/config/<key>) before invoking apply.
	Exemptions          rawApply // exemptions
	InterceptionDomains rawApply // interception_domains
	HookConfig          rawApply // hooks
	PayloadCapture      rawApply // payload_capture
	StreamingCompliance rawApply // streaming_compliance
	InstalledRulePacks  rawApply // installed_rule_packs (view-only)
	UserContext         rawApply // user_context (view-only)

	// ConfigCache is a late-bound getter for the local offline config
	// cache. Each registered applier is wrapped so a successful apply
	// mirrors its raw bytes into the cache; the daemon replays the cache
	// at boot (restoreCachedConfig) when Hub is unreachable. Nil — or a
	// getter that returns nil — disables persistence (the cache opens
	// only after the audit queue's SQLCipher DB is ready in cmdRun).
	ConfigCache func() *shadow.Cache
}

// buildConfigLoader returns a Loader pre-populated with every Agent
// shadow key + the HTTP puller closure that translates Cat B "needs
// pull" markers into a Hub HTTP GET against
// /api/internal/things/config/<key>?type=agent.
func buildConfigLoader(d configDispatchDeps) (*cfgloader.Loader, map[string]rawApply) {
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

	// getCache is the late-bound offline-cache accessor. A nil deps field
	// disables persistence entirely (used by tests that don't open a DB).
	getCache := d.ConfigCache
	if getCache == nil {
		getCache = func() *shadow.Cache { return nil }
	}

	// restore maps each key to its UNWRAPPED applier. The daemon replays
	// this map from config_cache at boot (restoreCachedConfig); it must
	// stay unwrapped so a restore does not re-persist what it just read.
	restore := make(map[string]rawApply)

	// reg registers a key under its applier wrapped with cachePersist (so
	// every successful apply mirrors into config_cache) and records the
	// unwrapped applier into the restore map. pull selects Cat B (Loader
	// HTTP-pulls before apply) vs Cat A (desired bytes ARE the data).
	reg := func(key string, apply rawApply, pull bool) {
		restore[key] = apply
		wrapped := cachePersist(key, apply, getCache, d.Logger)
		if pull {
			cfgloader.RegisterRawPull(l, key, wrapped)
		} else {
			cfgloader.RegisterRaw(l, key, wrapped)
		}
	}

	// Cat A — desired bytes ARE the data.
	reg("killswitch", d.KillSwitch, false)
	reg("agent_settings", d.AgentSettings, false)

	// Cat B — Loader HTTP-pulls before each apply.
	// `exemptions` is Cat B (not Cat A) because CP's write path uses
	// InvalidateConfig (signal-only); a Cat A WS push would carry empty
	// state and the agent would apply an empty payload on every signal,
	// silently overwriting admin grants. The HTTP-pull from Hub's
	// AgentExemptionsLoader is the canonical flow matching the CP write
	// contract.
	reg(configkey.Exemptions, d.Exemptions, true)
	reg("interception_domains", d.InterceptionDomains, true)
	reg(configkey.Hooks, d.HookConfig, true)
	reg("payload_capture", d.PayloadCapture, true)
	reg("streaming_compliance", d.StreamingCompliance, true)
	reg("installed_rule_packs", d.InstalledRulePacks, true)
	reg("user_context", d.UserContext, true)

	// diag_mode is a per-thing Cat A override carrying {until} (an RFC3339
	// timestamp). CP writes it via the Hub override API when an admin opens a
	// diagnostic-mode window; the applier (configappliers.go) decodes it and
	// drives the diag-mode log level, raising the agent to debug for the
	// window and restoring the baseline on expiry. Registered via RegisterRaw
	// (not the cachePersist-wrapping reg helper) because a log-level window is
	// not enforcement state — the offline cache only carries shadow state that
	// must survive a Hub outage.
	cfgloader.RegisterRaw(l, "diag_mode", d.DiagMode)

	return l, restore
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
