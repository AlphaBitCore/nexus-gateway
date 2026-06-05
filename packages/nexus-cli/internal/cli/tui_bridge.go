package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
	tui "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/shell"
)

// tui_bridge.go is the CLI↔TUI seam: it turns the resolved App (config, env,
// gateway client, keychain) into the dependency set the Bubble Tea console needs —
// the entry-wizard callbacks (env switch/create/update/delete, login, VK-secret +
// selection persistence) and the conversation agent factory. Keeping this out of
// root.go means the command tree stays a command tree, not the TUI dependency
// factory (audit Phase 3.3). All of it reaches the gateway only through core, and
// the closures read a.Env at call time so a wizard env switch is reflected.

// launchTUI starts the operator console (or the injected test hook).
func (a *App) launchTUI() error {
	if a.LaunchTUI != nil {
		return a.LaunchTUI(a)
	}
	return tui.Run(a.tuiDeps())
}

// tuiSession builds the dashboard session from the currently-resolved env plus
// the VK secret stored for it. The closures below read a.Env (not a captured
// copy) so a wizard env switch is reflected everywhere.
func (a *App) tuiSession() tui.Session {
	vkSecret, _ := a.Store.Get(a.Env.Name, core.SecretVKSecret)
	return tui.Session{
		EnvName:       a.Env.Name,
		Addr:          a.Env.CPBaseURL,
		IsProd:        a.Env.IsProd,
		Model:         a.Env.LastModel,
		ContextWindow: a.modelContextWindow(a.Env.LastModel),
		VKID:          a.Env.LastVKID,
		VKName:        a.Env.LastVKName,
		VKSecret:      vkSecret,
	}
}

// tuiDeps assembles the shell dependencies: the typed gateway plus the
// auth/persistence callbacks the entry wizard needs (env switch/create, login,
// VK-secret storage, remembered-selection persistence). All reach the gateway
// only through core. The closures read a.Env dynamically so the env step's
// switch/create (which mutates a.Env and rebuilds the client) takes effect.
func (a *App) tuiDeps() tui.Deps {
	names := make([]string, 0, len(a.Cfg.Envs))
	for n := range a.Cfg.Envs {
		names = append(names, n)
	}
	sort.Strings(names)
	return tui.Deps{
		Gateway:  a.client(),
		Session:  a.tuiSession(),
		EnvNames: names,
		HasSession: func() bool {
			return a.loggedIn()
		},
		SwitchEnv: func(name string) (tui.Gateway, tui.Session, bool, error) {
			env, err := a.Cfg.Resolve(name, "")
			if err != nil {
				return nil, tui.Session{}, false, err
			}
			a.Env, a.Client = env, nil // force the client to rebuild for the new env
			return a.client(), a.tuiSession(), a.loggedIn(), nil
		},
		CreateEnv: func(name, cpBaseURL, aigwBaseURL string, prod bool) (tui.Gateway, tui.Session, error) {
			if err := local.ValidateBaseURL("Control Plane URL", cpBaseURL); err != nil {
				return nil, tui.Session{}, err
			}
			if err := local.ValidateBaseURL("AI Gateway URL", aigwBaseURL); err != nil {
				return nil, tui.Session{}, err
			}
			env := core.Env{
				Name:             name,
				CPBaseURL:        cpBaseURL,
				AIGatewayBaseURL: aigwBaseURL,
				OAuthClientID:    "tui",
				OAuthRedirectURI: "http://localhost:3000/auth/callback",
				IsProd:           prod,
			}
			a.Cfg.SetEnv(env)
			if err := a.Cfg.SetDefault(name); err != nil {
				return nil, tui.Session{}, err
			}
			if err := a.Cfg.Save(); err != nil {
				return nil, tui.Session{}, err
			}
			a.Env, a.Client = env, nil
			return a.client(), a.tuiSession(), nil
		},
		UpdateEnv: func(name, cpBaseURL, aigwBaseURL string, prod bool) (tui.Gateway, tui.Session, bool, error) {
			if err := local.ValidateBaseURL("Control Plane URL", cpBaseURL); err != nil {
				return nil, tui.Session{}, false, err
			}
			if err := local.ValidateBaseURL("AI Gateway URL", aigwBaseURL); err != nil {
				return nil, tui.Session{}, false, err
			}
			// Preserve OAuth client + redirect + remembered selections from the
			// existing row — we are only changing the URLs and prod flag.
			old, ok := a.Cfg.Envs[name]
			if !ok {
				return nil, tui.Session{}, false, fmt.Errorf("unknown environment %q", name)
			}
			old.CPBaseURL = cpBaseURL
			old.AIGatewayBaseURL = aigwBaseURL
			old.IsProd = prod
			a.Cfg.SetEnv(old)
			if err := a.Cfg.Save(); err != nil {
				return nil, tui.Session{}, false, err
			}
			a.Env, a.Client = old, nil
			return a.client(), a.tuiSession(), a.loggedIn(), nil
		},
		DeleteEnv: func(name string) error {
			if err := a.Cfg.RemoveEnv(name); err != nil {
				return err
			}
			// Drop any stored secrets for that env so a future env reusing the
			// same name does not inherit a stale credential. ErrSecretNotFound
			// is the no-op success case; surface anything else.
			for _, key := range []string{core.SecretVKSecret, core.SecretAccessToken, core.SecretRefreshToken, core.SecretAdminKey} {
				if err := a.Store.Delete(name, key); err != nil && !errors.Is(err, core.ErrSecretNotFound) {
					return err
				}
			}
			return a.Cfg.Save()
		},
		EnvDetail: func(name string) (string, string, bool, error) {
			env, ok := a.Cfg.Envs[name]
			if !ok {
				return "", "", false, fmt.Errorf("unknown environment %q", name)
			}
			return env.CPBaseURL, env.AIGatewayBaseURL, env.IsProd, nil
		},
		Login: func(ctx context.Context) error {
			return core.NewAuthenticator(a.Env, a.Store, a.HTTP).
				WithBrowserOpener(a.BrowserOpener).LoginBrowser(ctx)
		},
		SaveVKSecret: func(secret string) error {
			return a.Store.Set(a.Env.Name, core.SecretVKSecret, secret)
		},
		SaveSelection: func(model, vkID, vkName string) error {
			a.Env.LastModel, a.Env.LastVKID, a.Env.LastVKName = model, vkID, vkName
			a.Cfg.SetEnv(a.Env)
			return a.Cfg.Save()
		},
		CreateVK: func(ctx context.Context, name string) (string, string, string, error) {
			vk, err := a.client().CreateVK(ctx, name)
			if err != nil {
				return "", "", "", err
			}
			return vk.ID, vk.Name, vk.Key, nil
		},
		BuildAgent: a.buildConversationAgent,
		Log:        a.Log,
	}
}

// buildConversationAgent is the tui.AgentBuildFunc the dashboard's conversation
// sidebar uses to construct the gateway agent it drives. The TUI owns the bridge
// and supplies the canvas (view-driving), the blocking Allow/Deny confirm gate, and the
// streaming callbacks; this closure supplies what only the CLI knows — the live
// env's model/VK and the on-disk memory/session/skill paths — and assembles the
// agent via capabilities.BuildAgent. It reads a.Env at call time (the conversation
// builds the agent lazily on the first turn, so a wizard env switch is reflected).
// Mitigation tools are enabled: every write the agent proposes is gated by the
// confirm callback (the Allow/Deny gate, raised in every environment), so the agent
// can act on the operator's behalf without bypassing the safety gate.
func (a *App) buildConversationAgent(canvas capabilities.Canvas, confirm agent.ConfirmFunc, onText, onReasoning func(string), onToolStart func(name string, input []byte), onToolEnd func(name string, output []byte, isError bool), onContext func(stats agent.ContextStats, window int), onCompact func(agent.CompactStat)) (tui.AgentRunner, error) {
	memDir, err := capabilities.DefaultMemoryDir()
	if err != nil {
		return nil, err
	}
	sessionDir, err := capabilities.DefaultSessionDir(a.Env.Name)
	if err != nil {
		return nil, err
	}
	skillDir, err := a.skillDir()
	if err != nil {
		return nil, err
	}
	vkSecret, _ := a.Store.Get(a.Env.Name, core.SecretVKSecret)
	// Resolve the selected model's context window once (best-effort) so the
	// conversation's context indicator can show used/window; 0 means "unknown".
	window := a.modelContextWindow(a.Env.LastModel)
	ag, err := capabilities.BuildAgent(context.Background(), capabilities.AgentDeps{
		Streamer:       a.client(),
		Gateway:        a.client(),
		Canvas:         canvas,
		Confirm:        confirm,
		VKSecret:       vkSecret,
		Model:          a.Env.LastModel,
		Env:            a.Env.Name,
		IsProd:         a.Env.IsProd,
		ContextWindow:  window,
		SkillDir:       skillDir,
		MemoryDir:      memDir,
		SessionDir:     sessionDir,
		EnableMitigate: true,
		OnText:         onText,
		OnReasoning:    onReasoning,
		OnToolStart:    onToolStart,
		OnToolEnd:      onToolEnd,
		OnContext:      func(cs agent.ContextStats) { onContext(cs, window) },
		OnCompact:      onCompact,
	})
	if err != nil {
		return nil, err
	}
	return ag, nil
}

// modelContextWindow returns the model's max context tokens from the catalog, or 0
// (unknown) when the catalog is unavailable or the model is not listed.
func (a *App) modelContextWindow(code string) int {
	if code == "" {
		return 0
	}
	cat, err := a.client().AdminModels(context.Background())
	if err != nil || cat == nil {
		return 0
	}
	for _, g := range cat.Data {
		for _, m := range g.Models {
			if m.Code == code {
				return m.MaxContextTokens
			}
		}
	}
	return 0
}
