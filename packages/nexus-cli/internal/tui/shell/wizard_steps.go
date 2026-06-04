package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

func (w *wizard) startLogin() (*wizard, tea.Cmd) {
	if w.deps.Login == nil {
		w.err = fmt.Errorf("no interactive login configured for this environment")
		return w, nil
	}
	w.loggingIn = true
	w.err = nil
	login := w.deps.Login
	ctx, cancel := context.WithTimeout(context.Background(), kit.LoginTimeout)
	w.loginCancel = cancel
	return w, func() tea.Msg {
		defer cancel()
		return loginDoneMsg{err: login(ctx)}
	}
}

// startCreateVK creates a personal VK the operator owns and finishes the wizard
// with the once-returned plaintext (no need to paste a secret).
func (w *wizard) startCreateVK() (*wizard, tea.Cmd) {
	if w.deps.CreateVK == nil {
		w.err = fmt.Errorf("creating a Virtual Key is not available in this build")
		return w, nil
	}
	w.loading = true
	w.err = nil
	create := w.deps.CreateVK
	return w, func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		id, name, secret, err := create(ctx, "nexus-cli")
		return vkCreatedMsg{id: id, name: name, secret: secret, err: err}
	}
}

// finish kicks off a probe against the live gateway to verify the pasted VK
// secret works BEFORE persisting it. A typo (or a secret that was rotated /
// revoked) would otherwise be saved and only surface as a 401 on the first
// real chat turn — by which point the operator has already left the wizard
// and is staring at a confusing "transport error (401)" on the dashboard.
//
// The probe is a minimal max_tokens=1 streaming chat completion against the
// selected model; cost is sub-cent and the round-trip is the same one a real
// chat would take, so 401/403/transport failures all surface here instead.
func (w *wizard) finish() (*wizard, tea.Cmd) {
	secret := strings.TrimSpace(w.secret.Value())
	if secret == "" {
		w.err = fmt.Errorf("a Virtual Key secret is required to chat / run the lab")
		return w, nil
	}
	w.err = nil
	w.loading = true
	gw, model := w.gateway, w.session.Model
	return w, func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		_, err := gw.ChatStream(ctx, secret, core.ChatRequest{
			Model:     model,
			Messages:  []core.ChatMessage{{Role: "user", Content: "."}},
			MaxTokens: 1,
		}, nil)
		return vkProbeMsg{secret: secret, err: classifyVKProbeError(err)}
	}
}

// classifyVKProbeError translates the raw chat-stream error from the probe
// into the message the operator sees on stageSecret. The aim is to make it
// obvious which of (bad secret, missing IAM, gateway unreachable) is wrong
// so the operator knows what to fix without reading a stack trace.
func classifyVKProbeError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, core.ErrUnauthorized):
		return fmt.Errorf("this Virtual Key was rejected (401) — re-check the secret you pasted")
	case errors.Is(err, core.ErrForbidden):
		return fmt.Errorf("this Virtual Key is not allowed to call /v1/chat/completions (403) — check its IAM policy or pick another key")
	case errors.Is(err, core.ErrNotFound):
		return fmt.Errorf("the selected model is not enabled on this gateway (404) — re-run the wizard and pick a different model")
	case errors.Is(err, core.ErrTransport):
		return fmt.Errorf("could not reach the AI Gateway to verify the key: %w — check the AI Gateway URL in your env config", err)
	default:
		return fmt.Errorf("gateway rejected the verification call: %w", err)
	}
}

// persistSecret stores the verified secret + the model/VK selection and
// signals the wizard is done. Split out so both the paste flow (after probe)
// and the create-VK flow (where the gateway just minted the key) share one
// codepath.
func (w *wizard) persistSecret(secret string) (*wizard, tea.Cmd) {
	if w.deps.SaveVKSecret != nil {
		if err := w.deps.SaveVKSecret(secret); err != nil {
			w.err = fmt.Errorf("store VK secret: %w", err)
			return w, nil
		}
	}
	if w.deps.SaveSelection != nil {
		if err := w.deps.SaveSelection(w.session.Model, w.session.VKID, w.session.VKName); err != nil {
			w.err = fmt.Errorf("save selection: %w", err)
			return w, nil
		}
	}
	w.session.VKSecret = secret
	w.done = true
	return w, nil
}

func (w *wizard) fetchModels() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		cat, err := w.gateway.AdminModels(ctx)
		if err != nil {
			return wizModelsMsg{err: err}
		}
		return wizModelsMsg{models: flattenModels(cat)}
	}
}

func (w *wizard) fetchVKs() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		vks, err := w.gateway.VirtualKeys(ctx)
		if err != nil {
			return wizVKsMsg{err: err}
		}
		return wizVKsMsg{vks: enabledVKs(vks)}
	}
}

// flattenModels turns the grouped catalog into a flat, selectable list of
// enabled chat models (the Playground + lab only call chat completions).
func flattenModels(cat *core.ModelCatalog) []modelChoice {
	var out []modelChoice
	if cat == nil {
		return out
	}
	for _, g := range cat.Data {
		for _, m := range g.Models {
			if !m.Enabled || (m.Type != "" && m.Type != "chat") {
				continue
			}
			out = append(out, modelChoice{code: m.Code, name: m.Name, provider: g.Provider.Label()})
		}
	}
	return out
}

// enabledVKs keeps only usable keys so the operator can't pick a disabled one.
func enabledVKs(vks []core.VirtualKey) []core.VirtualKey {
	out := make([]core.VirtualKey, 0, len(vks))
	for _, v := range vks {
		if v.Enabled {
			out = append(out, v)
		}
	}
	return out
}
