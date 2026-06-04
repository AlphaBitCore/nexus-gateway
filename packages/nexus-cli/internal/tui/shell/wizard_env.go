package shell

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (w *wizard) selectEnv() (*wizard, tea.Cmd) {
	if w.envCursor >= len(w.deps.EnvNames) {
		w.stage = stageEnvName
		w.err = nil
		w.input.SetValue("")
		w.input.Placeholder = "new environment name (e.g. dev)"
		return w, w.input.Focus()
	}
	gw, sess, loggedIn, err := w.deps.SwitchEnv(w.deps.EnvNames[w.envCursor])
	if err != nil {
		w.err = err
		return w, nil
	}
	w.err = nil
	w.gateway = gw
	w.session = sess
	return w, w.afterEnv(loggedIn)
}

// startChangeVK re-keys the selected environment: it switches to that env (so the
// VK list + persistence target the right deployment) and drops into the model→VK→
// secret steps, keeping the env's URLs and login untouched. This is the only path
// to swap an already-configured env's Virtual Key — selecting a fully-configured
// env from the picker finishes the wizard immediately (afterEnv), and the
// dashboard's /model switch keeps the existing key. The model step is skipped when
// one is already remembered, so "change key" doesn't force re-picking the model.
func (w *wizard) startChangeVK() (*wizard, tea.Cmd) {
	if w.envCursor >= len(w.deps.EnvNames) {
		return w, nil // the trailing "create new" row has no env to re-key
	}
	gw, sess, loggedIn, err := w.deps.SwitchEnv(w.deps.EnvNames[w.envCursor])
	if err != nil {
		w.err = err
		return w, nil
	}
	w.err = nil
	w.gateway = gw
	w.session = sess
	if !loggedIn {
		// Listing Virtual Keys needs a session; log in first, after which the
		// login handler routes on through model → VK → secret.
		w.stage = stageLogin
		return w, nil
	}
	w.loading = true
	if strings.TrimSpace(w.session.Model) == "" {
		w.stage = stageModel
		return w, w.fetchModels()
	}
	w.stage = stageVK
	return w, w.fetchVKs()
}

// createEnv persists + switches to the new environment, then proceeds to login.
// The CP URL was captured at stageEnvURL; this step's input holds the AI
// Gateway URL (defaulted to the CP URL on entry). When the operator entered
// the URL flow via `e` (edit), createEnv defers to UpdateEnv and routes the
// outcome through afterEnv so a still-logged-in edit doesn't force a fresh
// browser login.
func (w *wizard) createEnv() (*wizard, tea.Cmd) {
	aigwURL := strings.TrimSpace(w.input.Value())
	if aigwURL == "" {
		w.err = fmt.Errorf("AI Gateway URL is required")
		return w, nil
	}
	if w.editingEnv {
		if w.deps.UpdateEnv == nil {
			w.err = fmt.Errorf("editing an environment is not available in this build")
			return w, nil
		}
		gw, sess, loggedIn, err := w.deps.UpdateEnv(w.newEnvName, w.newEnvCPURL, aigwURL, false)
		if err != nil {
			w.err = fmt.Errorf("update environment: %w", err)
			return w, nil
		}
		w.err = nil
		w.gateway = gw
		w.session = sess
		w.editingEnv = false
		return w, w.afterEnv(loggedIn)
	}
	if w.deps.CreateEnv == nil {
		w.err = fmt.Errorf("creating an environment is not available in this build")
		return w, nil
	}
	gw, sess, err := w.deps.CreateEnv(w.newEnvName, w.newEnvCPURL, aigwURL, false)
	if err != nil {
		w.err = fmt.Errorf("create environment: %w", err)
		return w, nil
	}
	w.err = nil
	w.gateway = gw
	w.session = sess
	w.stage = stageLogin // a new environment is never logged in yet
	return w, nil
}

// startEditEnv loads an existing env's URLs into the create-flow inputs so
// the operator can correct a wrong CP / AI Gateway URL without re-typing the
// name. The editingEnv flag tells stageEnvAIGWURL.createEnv() to call
// UpdateEnv instead of CreateEnv at the end.
func (w *wizard) startEditEnv(name string) (*wizard, tea.Cmd) {
	cpURL, aigwURL := "", ""
	if w.deps.EnvDetail != nil {
		var err error
		cpURL, aigwURL, _, err = w.deps.EnvDetail(name)
		if err != nil {
			w.err = fmt.Errorf("read env %q: %w", name, err)
			return w, nil
		}
	}
	w.err = nil
	w.editingEnv = true
	w.newEnvName = name
	w.newEnvCPURL = cpURL
	w.input.SetValue(cpURL)
	w.input.Placeholder = "Control Plane base URL (https://…)"
	w.input.CursorEnd()
	w.stage = stageEnvURL
	// Drop the prefilled AI Gateway value into the secret-input free state;
	// when the operator advances past stageEnvURL the URL prefill code in
	// updateKey will recopy the CP URL — but we want the old AI Gateway URL
	// preserved if it differs. Stash it on the wizard for the stageEnvURL
	// handler to pick up.
	w.editAIGW = aigwURL
	return w, w.input.Focus()
}

// handleDeleteEnv runs the two-step `d`/`d` confirm: first press arms the
// gate (banner in the View shows "press d again to delete"), second press
// performs the delete via Deps.DeleteEnv. The wizard refuses to delete the
// only configured env (nothing to fall back to) and refuses to delete the
// active env so the operator does not log themselves out mid-session.
func (w *wizard) handleDeleteEnv() (*wizard, tea.Cmd) {
	if w.envCursor >= len(w.deps.EnvNames) {
		return w, nil
	}
	name := w.deps.EnvNames[w.envCursor]
	if len(w.deps.EnvNames) <= 1 {
		w.err = fmt.Errorf("cannot delete the only configured environment — add another with `create new` first")
		w.confirmDel = false
		return w, nil
	}
	if name == w.session.EnvName {
		w.err = fmt.Errorf("cannot delete the active env (%q) — switch to another env first", name)
		w.confirmDel = false
		return w, nil
	}
	if !w.confirmDel {
		w.confirmDel = true
		w.err = nil
		return w, nil
	}
	if w.deps.DeleteEnv == nil {
		w.err = fmt.Errorf("deleting an environment is not available in this build")
		w.confirmDel = false
		return w, nil
	}
	if err := w.deps.DeleteEnv(name); err != nil {
		w.err = fmt.Errorf("delete env %q: %w", name, err)
		w.confirmDel = false
		return w, nil
	}
	// Splice the deleted name out so the picker re-renders without it; pin
	// the cursor inside the new range.
	out := make([]string, 0, len(w.deps.EnvNames)-1)
	for _, n := range w.deps.EnvNames {
		if n != name {
			out = append(out, n)
		}
	}
	w.deps.EnvNames = out
	if w.envCursor > len(out) {
		w.envCursor = len(out) // can still point at the trailing "create new" row
	}
	w.confirmDel = false
	w.err = nil
	return w, nil
}

// afterEnv routes past the env step: a fully-configured logged-in env finishes
// the wizard; a logged-in env still needing a selection goes to model; otherwise
// the operator logs in.
func (w *wizard) afterEnv(loggedIn bool) tea.Cmd {
	if loggedIn && w.session.Model != "" && strings.TrimSpace(w.session.VKSecret) != "" {
		w.done = true
		return nil
	}
	if loggedIn {
		w.stage = stageModel
		w.loading = true
		return w.fetchModels()
	}
	w.stage = stageLogin
	return nil
}

// startLogin runs the injected browser login as a command. The context's cancel
// func is parked on the wizard so esc at stageLogin can abort the wait without
// leaving the loopback listener and browser tab hanging until the 3-minute
// timeout.
