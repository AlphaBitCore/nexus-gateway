package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// recordingDeps builds Deps whose callbacks record what the wizard did, so a
// test can assert the persisted selection without a keychain or disk.
type recordingDeps struct {
	deps        Deps
	loginCalls  int
	logoutCalls int
	logoutErr   error
	savedSecret string
	savedModel  string
	savedVKID   string
	savedVKName string
	loginErr    error
	saveSecErr  error
	saveSelErr  error
	hasSession  bool
	createErr   error
	createCalls int

	// env-step recording
	switchedTo     string
	switchErr      error
	switchLoggedIn bool
	switchModel    string
	switchVKSecret string
	createdEnvName string
	createdURL     string
	createdAIGW    string
	createdProd    bool
	createEnvErr   error
}

// withEnvStep wires the env-step callbacks (SwitchEnv/CreateEnv) so Init starts
// at the env stage; recording lets tests assert which env was chosen/created.
func (r *recordingDeps) withEnvStep(names []string) *recordingDeps {
	r.deps.EnvNames = names
	r.deps.SwitchEnv = func(name string) (Gateway, Session, bool, error) {
		r.switchedTo = name
		if r.switchErr != nil {
			return nil, Session{}, false, r.switchErr
		}
		return r.deps.Gateway, Session{EnvName: name, Model: r.switchModel, VKSecret: r.switchVKSecret}, r.switchLoggedIn, nil
	}
	r.deps.CreateEnv = func(name, url, aigw string, prod bool) (Gateway, Session, error) {
		r.createdEnvName, r.createdURL, r.createdAIGW, r.createdProd = name, url, aigw, prod
		if r.createEnvErr != nil {
			return nil, Session{}, r.createEnvErr
		}
		return r.deps.Gateway, Session{EnvName: name}, nil
	}
	return r
}

func newRecordingDeps(gw Gateway) *recordingDeps {
	r := &recordingDeps{}
	r.deps = Deps{
		Gateway:    gw,
		Session:    Session{EnvName: "local"},
		HasSession: func() bool { return r.hasSession },
		Login: func(context.Context) error {
			r.loginCalls++
			return r.loginErr
		},
		Logout: func() error {
			r.logoutCalls++
			return r.logoutErr
		},
		SaveVKSecret: func(s string) error {
			if r.saveSecErr != nil {
				return r.saveSecErr
			}
			r.savedSecret = s
			return nil
		},
		SaveSelection: func(model, vkID, vkName string) error {
			if r.saveSelErr != nil {
				return r.saveSelErr
			}
			r.savedModel, r.savedVKID, r.savedVKName = model, vkID, vkName
			return nil
		},
		CreateVK: func(context.Context, string) (string, string, string, error) {
			r.createCalls++
			if r.createErr != nil {
				return "", "", "", r.createErr
			}
			return "vk-new", "nexus-cli", "nvk_created_secret", nil
		},
	}
	return r
}

// runStep executes a returned command and folds the resulting message back in,
// returning the updated wizard and any follow-up command.
func runStep(t *testing.T, w *wizard, cmd tea.Cmd) (*wizard, tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	return w.Update(cmd())
}

// TestWizard_PasteRoutesToInput covers the bug fix: a bracketed paste (PasteMsg,
// not a key) must reach the wizard's text input — e.g. pasting a Control Plane URL
// or a VK secret during `nexus setup`.
func TestWizard_PasteRoutesToInput(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	w := newWizard(r.deps)
	w.input.Focus() // a focused field is the state of the URL / secret entry stages
	w, _ = w.Update(tea.PasteMsg{Content: "https://cp.example.com"})
	if !strings.Contains(w.input.Value(), "https://cp.example.com") {
		t.Fatalf("a paste must be inserted into the wizard input, got %q", w.input.Value())
	}
}

// TestWizard_PasteAtSecretStage covers the bug: at stageSecret the focused input is
// w.secret (not w.input), so PasteMsg must route there — otherwise pasting a VK
// secret silently drops.
func TestWizard_PasteAtSecretStage(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	w := newWizard(r.deps)
	w.stage = stageSecret
	w.secret.Focus()
	w, _ = w.Update(tea.PasteMsg{Content: "nvk_pasted_vk_secret"})
	if !strings.Contains(w.secret.Value(), "nvk_pasted_vk_secret") {
		t.Fatalf("paste at stageSecret must reach w.secret, got %q", w.secret.Value())
	}
}

func TestWizard_FullFlow(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	w := newWizard(r.deps)
	if cmd := w.Init(); cmd != nil { // no session → starts at login, nil cmd
		t.Fatal("login stage Init should have no command")
	}
	if w.stage != stageLogin || !strings.Contains(w.View(100, 24), "Step 1") {
		t.Fatalf("should start at login, stage=%d", w.stage)
	}
	// enter → login (cmd returns loginDoneMsg) → folding it advances to model
	// stage and returns the fetchModels cmd → folding that loads the models.
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, cmd = runStep(t, w, cmd) // loginDoneMsg → stage=model, cmd=fetchModels
	if w.stage != stageModel {
		t.Fatalf("after login should be at model stage, got %d", w.stage)
	}
	w, _ = runStep(t, w, cmd) // wizModelsMsg
	if len(w.models) != 1 || w.models[0].code != "gpt-4o-mini" {
		t.Fatalf("models not loaded: %+v", w.models)
	}
	if r.loginCalls != 1 {
		t.Fatalf("login should be called once, got %d", r.loginCalls)
	}
	if !strings.Contains(w.View(100, 24), "gpt-4o-mini") {
		t.Fatal("model list should render")
	}
	// pick model → VK stage
	w, cmd = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageVK {
		t.Fatalf("after model pick should be VK stage, got %d", w.stage)
	}
	w, _ = runStep(t, w, cmd) // wizVKsMsg
	if len(w.vks) != 1 {
		t.Fatalf("vks not loaded: %+v", w.vks)
	}
	// pick VK → secret stage
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageSecret {
		t.Fatalf("after VK pick should be secret stage, got %d", w.stage)
	}
	// type the secret + enter → kicks off the probe (chat completion) against
	// the gateway. The probe cmd resolves to a vkProbeMsg which the wizard
	// then folds into a successful persist.
	w.secret.SetValue("nvk_topsecret")
	w, cmd = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // vkProbeMsg{nil} → persistSecret → done
	if !w.done {
		t.Fatal("wizard should be done after the secret is verified + saved")
	}
	if r.savedSecret != "nvk_topsecret" || w.session.VKSecret != "nvk_topsecret" {
		t.Fatalf("secret not stored: saved=%q session=%q", r.savedSecret, w.session.VKSecret)
	}
	if r.savedModel != "gpt-4o-mini" || r.savedVKID != "vk1" || r.savedVKName != "engineering" {
		t.Fatalf("selection not persisted: %q %q %q", r.savedModel, r.savedVKID, r.savedVKName)
	}
}

func TestWizard_CreateVKPath(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init()) // → stageModel
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // → stageVK with VKs loaded
	if w.stage != stageVK {
		t.Fatalf("expected stageVK, got %d", w.stage)
	}
	// press 'c' to create a new owned VK → vkCreatedMsg → done
	w, cmd = w.Update(keyRunes("c"))
	w, _ = runStep(t, w, cmd)
	if !w.done {
		t.Fatal("creating a VK should finish the wizard")
	}
	if r.createCalls != 1 {
		t.Fatalf("CreateVK should be called once, got %d", r.createCalls)
	}
	if w.session.VKSecret != "nvk_created_secret" || r.savedSecret != "nvk_created_secret" {
		t.Fatalf("created secret not used/stored: session=%q saved=%q", w.session.VKSecret, r.savedSecret)
	}
	if r.savedVKID != "vk-new" {
		t.Fatalf("created VK id not persisted: %q", r.savedVKID)
	}
}

func TestWizard_CreateVKError(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.createErr = errors.New("403 denied")
	w := newWizard(r.deps)
	w.stage = stageVK
	w, cmd := w.Update(keyRunes("c"))
	w, _ = runStep(t, w, cmd)
	if w.done || w.err == nil || !strings.Contains(w.err.Error(), "403 denied") {
		t.Fatalf("create error should block done: %v", w.err)
	}
}

func TestWizard_CreateVKUnavailable(t *testing.T) {
	d := Deps{Gateway: sampleGateway(), Session: Session{EnvName: "local"}, HasSession: func() bool { return true }}
	w := newWizard(d)
	w.stage = stageVK
	w, cmd := w.Update(keyRunes("c"))
	if cmd != nil || w.err == nil || !strings.Contains(w.err.Error(), "not available") {
		t.Fatalf("nil CreateVK should surface unavailable error, got %v", w.err)
	}
}

func TestWizard_SkipsLoginWhenSessionExists(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init()) // HasSession → straight to model fetch
	if w.stage != stageModel || len(w.models) != 1 {
		t.Fatalf("existing session should skip login and load models, stage=%d models=%d", w.stage, len(w.models))
	}
	if r.loginCalls != 0 {
		t.Fatal("login must not be called when a session exists")
	}
}

func TestWizard_LoginError(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.loginErr = errors.New("redirect mismatch")
	w := newWizard(r.deps)
	w.Init()
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // loginDoneMsg{err}
	if w.stage != stageLogin || w.err == nil || !strings.Contains(w.View(100, 24), "redirect mismatch") {
		t.Fatalf("login error should keep the wizard at login with the error shown")
	}
}

func TestWizard_NoLoginConfigured(t *testing.T) {
	d := Deps{Gateway: sampleGateway(), Session: Session{EnvName: "local"}, HasSession: func() bool { return false }}
	w := newWizard(d)
	w.Init()
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || w.err == nil || !strings.Contains(w.err.Error(), "no interactive login") {
		t.Fatalf("missing Login should surface an error, got err=%v", w.err)
	}
}

func TestWizard_EmptySecretRejected(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w.stage = stageSecret
	w.session.Model = "m"
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // empty secret
	if w.done || w.err == nil {
		t.Fatal("empty secret must be rejected")
	}
}

func TestWizard_PersistErrors(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.saveSecErr = errors.New("keychain locked")
	w := newWizard(r.deps)
	w.stage = stageSecret
	w.secret.SetValue("nvk_x")
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → probe cmd
	w, _ = runStep(t, w, cmd)                               // probe success → persist fails
	if w.done || w.err == nil || !strings.Contains(w.err.Error(), "keychain locked") {
		t.Fatalf("SaveVKSecret error should block done: %v", w.err)
	}
	// now secret OK but selection save fails
	r2 := newRecordingDeps(sampleGateway())
	r2.saveSelErr = errors.New("disk full")
	w2 := newWizard(r2.deps)
	w2.stage = stageSecret
	w2.secret.SetValue("nvk_x")
	w2, cmd2 := w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w2, _ = runStep(t, w2, cmd2)
	if w2.done || w2.err == nil || !strings.Contains(w2.err.Error(), "disk full") {
		t.Fatalf("SaveSelection error should block done: %v", w2.err)
	}
}

// TestWizard_VKProbe_Rejects401 asserts the new validation gate: when the
// gateway returns 401 on the verification chat, the wizard stays on
// stageSecret with a typed error and does NOT save a known-bad secret.
func TestWizard_VKProbe_Rejects401(t *testing.T) {
	gw := sampleGateway()
	// Wrap the sentinel with %w so errors.Is on the classifier finds it,
	// without reaching into core.APIError's unexported fields.
	gw.err = fmt.Errorf("401 from gateway: %w", core.ErrUnauthorized)
	r := newRecordingDeps(gw)
	r.hasSession = true
	w := newWizard(r.deps)
	w.stage = stageSecret
	w.session.Model = "gpt-4o-mini"
	w.secret.SetValue("nvk_wrong")
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → probe cmd
	w, _ = runStep(t, w, cmd)
	if w.done {
		t.Fatal("a 401 probe must not save the secret")
	}
	if r.savedSecret != "" {
		t.Fatalf("SaveVKSecret must not run on a failing probe; got %q", r.savedSecret)
	}
	if w.stage != stageSecret || w.err == nil || !strings.Contains(w.err.Error(), "401") {
		t.Fatalf("401 must surface a typed message on stageSecret: stage=%v err=%v", w.stage, w.err)
	}
}

// TestWizard_VKProbe_TransportError covers the case where the AI Gateway URL
// is wrong (the same misconfiguration that produced the prod 405 → user
// reported). The wizard must surface a message that points at the env config,
// not just a raw transport error string.
func TestWizard_VKProbe_TransportError(t *testing.T) {
	gw := sampleGateway()
	gw.err = fmt.Errorf("405 from upstream: %w", core.ErrTransport)
	r := newRecordingDeps(gw)
	r.hasSession = true
	w := newWizard(r.deps)
	w.stage = stageSecret
	w.session.Model = "gpt-4o-mini"
	w.secret.SetValue("nvk_x")
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd)
	if w.done {
		t.Fatal("a transport failure must not save the secret")
	}
	if w.err == nil || !strings.Contains(w.err.Error(), "AI Gateway URL") {
		t.Fatalf("transport failure must point at the AI Gateway URL: %v", w.err)
	}
}

func TestWizard_FetchErrors(t *testing.T) {
	r := newRecordingDeps(&fakeGateway{err: errors.New("403 denied")})
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init()) // model fetch errors
	if w.err == nil || !strings.Contains(w.View(100, 24), "403 denied") {
		t.Fatal("model fetch error should be shown")
	}
	// VK fetch error
	w.stage = stageVK
	w, _ = runStep(t, w, w.fetchVKs())
	if w.err == nil {
		t.Fatal("VK fetch error should be recorded")
	}
}

func TestWizard_NavClampsAndFilters(t *testing.T) {
	// up at top / down past end must not move the cursor out of range.
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init())
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // already at 0
	if w.modelCursor != 0 {
		t.Fatal("up at top should clamp")
	}
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // only 1 model
	if w.modelCursor != 0 {
		t.Fatal("down past end should clamp")
	}
	// filters
	cat := &core.ModelCatalog{Data: []core.ModelGroup{{Models: []core.Model{
		{Code: "chat-on", Type: "chat", Enabled: true},
		{Code: "embed", Type: "embedding", Enabled: true},
		{Code: "disabled", Type: "chat", Enabled: false},
	}}}}
	if got := flattenModels(cat); len(got) != 1 || got[0].code != "chat-on" {
		t.Fatalf("flattenModels should keep only enabled chat models: %+v", got)
	}
	vks := enabledVKs([]core.VirtualKey{{Name: "a", Enabled: true}, {Name: "b", Enabled: false}})
	if len(vks) != 1 || vks[0].Name != "a" {
		t.Fatalf("enabledVKs should drop disabled keys: %+v", vks)
	}
}

// --- env step (E88 v1.1) ---

func TestWizard_EnvStep_StartsAndLists(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local", "dev"})
	w := newWizard(r.deps)
	if cmd := w.Init(); cmd != nil {
		t.Fatal("the env step has no init command")
	}
	if w.stage != stageEnv {
		t.Fatalf("with envs configured the wizard starts at the env step, got %v", w.stage)
	}
	if w.envCursor != 0 { // current env "local" preselected
		t.Fatalf("current env should be preselected, got cursor %d", w.envCursor)
	}
	out := w.View(100, 24)
	for _, want := range []string{"Choose an environment", "local", "dev", "create a new environment"} {
		if !strings.Contains(out, want) {
			t.Errorf("env view missing %q:\n%s", want, out)
		}
	}
	// up at top + down past the create row are no-ops/clamped.
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if w.envCursor != 0 {
		t.Fatal("up at top should stay")
	}
	for i := 0; i < 5; i++ {
		w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if w.envCursor != 2 { // 2 envs → indices 0,1 + create row at 2
		t.Fatalf("down should clamp at the create row, got %d", w.envCursor)
	}
}

func TestWizard_EnvStep_SwitchRouting(t *testing.T) {
	// not logged in → login.
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local", "dev"})
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})  // → dev
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // select dev
	if r.switchedTo != "dev" || w.stage != stageLogin {
		t.Fatalf("not-logged-in switch should go to login; switched=%q stage=%v", r.switchedTo, w.stage)
	}

	// logged in but no model/VK → model fetch.
	r2 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r2.switchLoggedIn = true
	w2 := newWizard(r2.deps)
	w2.Init()
	w2, cmd := w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w2.stage != stageModel || cmd == nil {
		t.Fatalf("logged-in env without a selection should fetch models, stage=%v", w2.stage)
	}

	// logged in + fully configured → wizard finishes immediately.
	r3 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r3.switchLoggedIn = true
	r3.switchModel = "gpt-4o-mini"
	r3.switchVKSecret = "nvk_x"
	w3 := newWizard(r3.deps)
	w3.Init()
	w3, _ = w3.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !w3.done {
		t.Fatal("a fully-configured logged-in env should finish the wizard")
	}
}

// TestWizard_EnvStep_ChangeVK covers the reported gap: an already-configured env
// could not change its Virtual Key (selecting it finished the wizard; `e` only
// edits URLs). `v` switches to the env and drops into the VK step (model
// preserved), and finishing persists the NEW key for that env.
func TestWizard_EnvStep_ChangeVK(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local", "dev"})
	r.switchLoggedIn = true
	r.switchModel = "gpt-4o-mini" // already has a model → skip the model step
	r.switchVKSecret = "nvk_old"
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → dev
	w, cmd := w.Update(keyRunes("v"))                   // re-key dev
	if r.switchedTo != "dev" {
		t.Fatalf("v should switch to the selected env first, got %q", r.switchedTo)
	}
	if w.stage != stageVK {
		t.Fatalf("a configured env with a model should jump straight to the VK step, got %v", w.stage)
	}
	w, _ = runStep(t, w, cmd) // wizVKsMsg → VK list loaded
	if len(w.vks) == 0 {
		t.Fatal("the VK list should load for the re-key step")
	}
	// pick a VK → secret → verify + persist the new key.
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageSecret {
		t.Fatalf("VK pick should go to the secret stage, got %v", w.stage)
	}
	w.secret.SetValue("nvk_rotated")
	w, cmd = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // vkProbeMsg{nil} → persist → done
	if !w.done {
		t.Fatal("re-key should finish after the new secret verifies")
	}
	if r.savedSecret != "nvk_rotated" {
		t.Fatalf("the new VK secret must be persisted, got %q", r.savedSecret)
	}
	if r.savedVKID != "vk1" || r.savedModel != "gpt-4o-mini" {
		t.Fatalf("re-key must persist the re-picked VK + preserve the model: vk=%q model=%q", r.savedVKID, r.savedModel)
	}
}

// TestWizard_EnvStep_ChangeVKRouting covers the `v` routing + guards: the
// create-new row is a no-op, a logged-out env logs in first, a model-less env
// re-picks the model first, and a switch failure surfaces without leaving the
// env stage.
func TestWizard_EnvStep_ChangeVKRouting(t *testing.T) {
	// create-new row → v is a no-op (no env to re-key).
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → create row
	w, cmd := w.Update(keyRunes("v"))
	if cmd != nil || w.stage != stageEnv || r.switchedTo != "" {
		t.Fatalf("v on the create-new row must be a no-op, stage=%v switched=%q", w.stage, r.switchedTo)
	}

	// not logged in → v routes to login.
	r2 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	w2 := newWizard(r2.deps)
	w2.Init()
	w2, _ = w2.Update(keyRunes("v"))
	if w2.stage != stageLogin {
		t.Fatalf("logged-out re-key should go to login, got %v", w2.stage)
	}

	// logged in but no remembered model → v fetches the model list first.
	r3 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r3.switchLoggedIn = true
	w3 := newWizard(r3.deps)
	w3.Init()
	w3, cmd3 := w3.Update(keyRunes("v"))
	if w3.stage != stageModel || cmd3 == nil {
		t.Fatalf("model-less re-key should fetch models, got %v", w3.stage)
	}

	// SwitchEnv error surfaces and stays on the env stage.
	r4 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r4.switchErr = errors.New("switch boom")
	w4 := newWizard(r4.deps)
	w4.Init()
	w4, _ = w4.Update(keyRunes("v"))
	if w4.err == nil || w4.stage != stageEnv {
		t.Fatalf("switch error during re-key should surface and stay, err=%v stage=%v", w4.err, w4.stage)
	}
}

func TestWizard_EnvStep_CreateFlow(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})  // → create row
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → name entry
	if w.stage != stageEnvName {
		t.Fatalf("create row should enter the name stage, got %v", w.stage)
	}
	// empty name is rejected.
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnvName {
		t.Fatal("empty env name should error and stay on the name stage")
	}
	w, _ = w.Update(keyRunes("staging"))
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageEnvURL || w.newEnvName != "staging" {
		t.Fatalf("name → url; name=%q stage=%v", w.newEnvName, w.stage)
	}
	if !strings.Contains(w.View(100, 24), "Control Plane URL") {
		t.Fatal("url stage should render")
	}
	// empty URL is rejected.
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnvURL {
		t.Fatal("empty URL should error and stay")
	}
	w, _ = w.Update(keyRunes("https://stg"))
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	// Advancing the CP URL lands on a dedicated AI Gateway URL step that
	// pre-fills with the CP URL — single-host deployments just hit enter,
	// multi-host deployments edit before continuing.
	if w.stage != stageEnvAIGWURL {
		t.Fatalf("CP URL → AI Gateway stage; got %v", w.stage)
	}
	if got := w.input.Value(); got != "https://stg" {
		t.Fatalf("AI Gateway stage should prefill with CP URL, got %q", got)
	}
	if !strings.Contains(w.View(100, 24), "AI Gateway URL") {
		t.Fatal("AI Gateway stage should render its own heading")
	}
	// Editing the field — type a multi-host gateway URL — then submit.
	w.input.SetValue("https://aigw.stg")
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if r.createdEnvName != "staging" || r.createdURL != "https://stg" || r.createdAIGW != "https://aigw.stg" || r.createdProd {
		t.Fatalf("CreateEnv args wrong: name=%q cp=%q aigw=%q prod=%v",
			r.createdEnvName, r.createdURL, r.createdAIGW, r.createdProd)
	}
	if w.stage != stageLogin {
		t.Fatalf("a created env should proceed to login, got %v", w.stage)
	}
}

func TestWizard_EnvStep_Errors(t *testing.T) {
	// SwitchEnv error surfaces.
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r.switchErr = errors.New("switch boom")
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnv {
		t.Fatalf("switch error should surface and stay on env stage: err=%v stage=%v", w.err, w.stage)
	}
	// CreateEnv error surfaces.
	r2 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r2.createEnvErr = errors.New("create boom")
	w2 := newWizard(r2.deps)
	w2.Init()
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // name stage
	w2, _ = w2.Update(keyRunes("x"))
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // url stage
	w2, _ = w2.Update(keyRunes("https://x"))
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // aigw stage (prefilled)
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // create → error
	if w2.err == nil {
		t.Fatal("create error should surface")
	}
}

func TestWizard_EnvStep_HelpersAndNilCreate(t *testing.T) {
	// indexOfCurrentEnv falls back to 0 when the current env is not in the list.
	w := newWizard(newRecordingDeps(sampleGateway()).withEnvStep([]string{"a", "b"}).deps)
	w.session.EnvName = "not-listed"
	if w.indexOfCurrentEnv() != 0 {
		t.Fatal("unknown current env should preselect index 0")
	}
	// createEnv with a nil CreateEnv callback surfaces an unavailable error.
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r.deps.CreateEnv = nil
	w2 := newWizard(r.deps)
	w2.Init()
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyDown})  // create row
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // name stage
	w2, _ = w2.Update(keyRunes("x"))                       // name
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // url stage
	w2, _ = w2.Update(keyRunes("https://x"))               // url
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // aigw stage (prefilled)
	w2, _ = w2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // create → nil callback
	if w2.err == nil || !strings.Contains(w2.err.Error(), "not available") {
		t.Fatalf("nil CreateEnv should surface unavailable error, got %v", w2.err)
	}
}

// TestWizard_EscCancelsLogin covers the bug fix: while waiting at stageLogin
// for the browser callback, pressing esc must (1) cancel the underlying
// context so the loopback listener releases its port immediately, (2) flip
// loggingIn off and walk the wizard back to the env picker, and (3) discard
// any late loginDoneMsg that arrives after the abort so the operator is not
// silently routed into the model picker on a screen they have already left.
func TestWizard_EscCancelsLogin(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	// A Login that blocks until the test cancels the ctx and reports back so we
	// can assert the cancel actually propagated through to the goroutine.
	loginEntered := make(chan struct{}, 1)
	r.deps.Login = func(ctx context.Context) error {
		loginEntered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	w := newWizard(r.deps)
	w.Init()
	// Force the wizard into the login stage (the simplest path is to drive a
	// known existing env onto the login screen via afterEnv with loggedIn=false).
	w.stage = stageLogin
	// Kick the login off — the cmd's goroutine will hold on ctx.Done().
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !w.loggingIn || w.loginCancel == nil {
		t.Fatal("startLogin should mark loggingIn and store a cancel func")
	}
	if cmd == nil {
		t.Fatal("startLogin should return a command")
	}
	// Run the cmd in a goroutine — it will block on <-ctx.Done() until esc cancels.
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	<-loginEntered // confirm the goroutine entered Login before we cancel
	// Esc cancels the login + walks back to the env picker.
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if w.stage != stageEnv || w.loggingIn || w.loginCancel != nil {
		t.Fatalf("esc at stageLogin: stage=%v loggingIn=%v cancel=%v", w.stage, w.loggingIn, w.loginCancel)
	}
	// The blocked cmd should now have returned (cancel propagated through to
	// the goroutine, otherwise this select hangs the test up to 1s).
	select {
	case msg := <-done:
		// Feed the late message back through Update — the post-esc handler
		// must DISCARD it (stay on stageEnv) rather than advance to stageModel
		// or surface a context-cancelled error on a screen we already left.
		w, _ = w.Update(msg)
		if w.stage != stageEnv {
			t.Fatalf("late loginDoneMsg after esc must not advance the stage, got %v", w.stage)
		}
		if w.err != nil {
			t.Fatalf("late loginDoneMsg after esc must not surface an error, got %v", w.err)
		}
	case <-time.After(time.Second):
		t.Fatal("esc did not cancel the in-flight login")
	}
}

// TestWizard_EscFromAIGWStageGoesBack covers the new stageEnvAIGWURL back-step
// — esc must re-focus the CP URL input with the previously-typed value so the
// operator can correct it without re-typing.
func TestWizard_EscFromAIGWStageGoesBack(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})  // create row
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → name stage
	w, _ = w.Update(keyRunes("staging"))
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → url stage
	w, _ = w.Update(keyRunes("https://stg"))
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → aigw stage
	if w.stage != stageEnvAIGWURL {
		t.Fatalf("setup: want stageEnvAIGWURL, got %v", w.stage)
	}
	// Edit the prefill and then esc — should land back on the URL stage with
	// the CP URL restored, not the edited AI Gateway draft.
	w.input.SetValue("https://aigw.edited")
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if w.stage != stageEnvURL {
		t.Fatalf("esc from aigw stage should land on stageEnvURL, got %v", w.stage)
	}
	if got := w.input.Value(); got != "https://stg" {
		t.Fatalf("esc must restore the CP URL into the input, got %q", got)
	}
}

// envMgmtFixture wires DeleteEnv / UpdateEnv / EnvDetail callbacks that
// record what the wizard did so a test can assert the env was actually
// removed, updated, or rendered with the right URLs.
type envMgmtFixture struct {
	*recordingDeps
	envURLs        map[string]struct{ cp, aigw string }
	deletedEnv     string
	updatedName    string
	updatedCP      string
	updatedAIGW    string
	updatedProd    bool
	updateErr      error
	deleteErr      error
	updateLoggedIn bool
}

func newEnvMgmtFixture(names []string) *envMgmtFixture {
	r := newRecordingDeps(sampleGateway()).withEnvStep(names)
	f := &envMgmtFixture{recordingDeps: r, envURLs: map[string]struct{ cp, aigw string }{}}
	for _, n := range names {
		f.envURLs[n] = struct{ cp, aigw string }{cp: "https://cp." + n, aigw: "https://aigw." + n}
	}
	f.deps.EnvDetail = func(name string) (string, string, bool, error) {
		v, ok := f.envURLs[name]
		if !ok {
			return "", "", false, errors.New("unknown env")
		}
		return v.cp, v.aigw, false, nil
	}
	f.deps.DeleteEnv = func(name string) error {
		if f.deleteErr != nil {
			return f.deleteErr
		}
		f.deletedEnv = name
		delete(f.envURLs, name)
		return nil
	}
	f.deps.UpdateEnv = func(name, cp, aigw string, prod bool) (Gateway, Session, bool, error) {
		if f.updateErr != nil {
			return nil, Session{}, false, f.updateErr
		}
		f.updatedName, f.updatedCP, f.updatedAIGW, f.updatedProd = name, cp, aigw, prod
		f.envURLs[name] = struct{ cp, aigw string }{cp: cp, aigw: aigw}
		return f.deps.Gateway, Session{EnvName: name}, f.updateLoggedIn, nil
	}
	return f
}

// TestWizard_EnvStep_DeleteTwoStep covers the two-press delete gate: first
// `d` arms (no delete; the View shows a confirm banner), second `d` actually
// removes the env. After delete, the env disappears from the picker list so
// the operator cannot try to delete it twice.
func TestWizard_EnvStep_DeleteTwoStep(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	// Active env is "local" (recordingDeps default); cursor at 1 ("staging")
	// so we are deleting a non-active env (the wizard refuses to delete the
	// active env to avoid mid-session lockout).
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1 // staging row
	w, _ = w.Update(keyRunes("d"))
	if !w.confirmDel || f.deletedEnv != "" {
		t.Fatalf("first d should arm only: confirmDel=%v deletedEnv=%q", w.confirmDel, f.deletedEnv)
	}
	if !strings.Contains(w.View(120, 30), "press d again") {
		t.Fatal("first d must surface the confirm banner in the View")
	}
	w, _ = w.Update(keyRunes("d"))
	if f.deletedEnv != "staging" {
		t.Fatalf("second d should call DeleteEnv: got %q", f.deletedEnv)
	}
	if w.confirmDel {
		t.Fatal("confirmDel must reset after the delete fires")
	}
	if got := w.deps.EnvNames; len(got) != 1 || got[0] != "local" {
		t.Fatalf("env list should drop the deleted row: %v", got)
	}
}

// TestWizard_EnvStep_DeleteRefusesActive covers the safety guard: the wizard
// refuses to delete the env currently in use so the operator does not get
// dropped mid-session.
func TestWizard_EnvStep_DeleteRefusesActive(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 0 // local — the active env
	w, _ = w.Update(keyRunes("d"))
	if w.confirmDel || w.err == nil || !strings.Contains(w.err.Error(), "active") {
		t.Fatalf("delete on the active env must error: confirmDel=%v err=%v", w.confirmDel, w.err)
	}
	if f.deletedEnv != "" {
		t.Fatalf("active env must not be deleted: %q", f.deletedEnv)
	}
}

// TestWizard_EnvStep_DeleteRefusesOnlyEnv covers the second safety guard:
// the wizard refuses to delete the only configured env (nothing to fall
// back to). The operator must add another env first.
func TestWizard_EnvStep_DeleteRefusesOnlyEnv(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local"})
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 0
	w, _ = w.Update(keyRunes("d"))
	if w.err == nil || !strings.Contains(w.err.Error(), "only configured") {
		t.Fatalf("delete on the only env must error: %v", w.err)
	}
}

// TestWizard_EnvStep_EditUpdatesURLs covers `e`: load the env's URLs into
// the create-flow inputs, advance through both, and confirm UpdateEnv is
// called with the new values (CreateEnv is NOT called).
func TestWizard_EnvStep_EditUpdatesURLs(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1 // staging row
	w, _ = w.Update(keyRunes("e"))
	if w.stage != stageEnvURL || !w.editingEnv || w.newEnvName != "staging" {
		t.Fatalf("e should enter URL stage in edit mode for staging: stage=%v editing=%v name=%q",
			w.stage, w.editingEnv, w.newEnvName)
	}
	if got := w.input.Value(); got != "https://cp.staging" {
		t.Fatalf("URL input must prefill with the existing CP URL, got %q", got)
	}
	// Type over the prefill: change the CP URL.
	w.input.SetValue("https://cp.new-staging")
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageEnvAIGWURL {
		t.Fatalf("CP enter should advance to AIGW stage, got %v", w.stage)
	}
	// AIGW step prefills with the existing AI Gateway URL (NOT the new CP
	// URL) so the operator does not lose a multi-host config.
	if got := w.input.Value(); got != "https://aigw.staging" {
		t.Fatalf("AIGW step in edit mode must prefill with the existing AI Gateway URL, got %q", got)
	}
	w.input.SetValue("https://aigw.new-staging")
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if f.updatedName != "staging" || f.updatedCP != "https://cp.new-staging" || f.updatedAIGW != "https://aigw.new-staging" {
		t.Fatalf("UpdateEnv args wrong: name=%q cp=%q aigw=%q",
			f.updatedName, f.updatedCP, f.updatedAIGW)
	}
}

// TestWizard_EnvStep_ViewDetail covers the rendered URL detail under the
// cursor row — the operator needs to see at a glance which deployment each
// env points at before pressing enter / e / d.
func TestWizard_EnvStep_ViewDetail(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1 // staging
	view := w.View(120, 30)
	if !strings.Contains(view, "https://cp.staging") || !strings.Contains(view, "https://aigw.staging") {
		t.Fatalf("env detail must include both URLs for the cursor row, got:\n%s", view)
	}
}

// TestWizard_VKProbe_ClassifierBranches covers every branch of the probe
// error classifier so the messages the operator sees for 403 / 404 / unknown
// (`!errors.Is(...)`) are typed and stable, not opaque go error strings.
func TestWizard_VKProbe_ClassifierBranches(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSub string
	}{
		{name: "nil passes through", err: nil, wantSub: ""},
		{name: "403 → IAM hint", err: fmt.Errorf("denied: %w", core.ErrForbidden), wantSub: "IAM"},
		{name: "404 → model hint", err: fmt.Errorf("not enabled: %w", core.ErrNotFound), wantSub: "model is not enabled"},
		{name: "unknown error wrapped", err: errors.New("strange failure"), wantSub: "gateway rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyVKProbeError(tc.err)
			if tc.wantSub == "" {
				if got != nil {
					t.Fatalf("nil err must classify to nil, got %v", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tc.wantSub) {
				t.Fatalf("want %q substring, got %v", tc.wantSub, got)
			}
		})
	}
}

// TestWizard_EscFromLaterStagesGoesBack covers every goBack branch that a
// non-trivial wizard run can hit: env-name → env, AIGW-URL → URL, model →
// env, VK → model, secret → VK. Each step must reset the right input state
// so the previous stage renders with the previously-typed value rather than
// the abandoned draft.
func TestWizard_EscFromLaterStagesGoesBack(t *testing.T) {
	cases := []struct {
		from, to wizardStage
	}{
		{stageEnvName, stageEnv},
		{stageModel, stageEnv},
		{stageVK, stageModel},
		{stageSecret, stageVK},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("from-%d", tc.from), func(t *testing.T) {
			r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
			w := newWizard(r.deps)
			w.Init()
			w.stage = tc.from
			w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			if w.stage != tc.to {
				t.Fatalf("esc from %v should land on %v, got %v", tc.from, tc.to, w.stage)
			}
		})
	}
}

// TestWizard_EnvStep_EditWithoutCallback covers the safety branch: pressing
// `e` on a build that did not wire UpdateEnv must surface "not available"
// when the operator advances all the way through, not silently no-op.
func TestWizard_EnvStep_EditWithoutCallback(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	f.deps.UpdateEnv = nil // simulate a build without edit support
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1
	w, _ = w.Update(keyRunes("e"))
	if w.stage != stageEnvURL {
		t.Fatalf("e should still enter the URL stage, got %v", w.stage)
	}
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → AIGW
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // → createEnv (UpdateEnv nil)
	if w.err == nil || !strings.Contains(w.err.Error(), "not available") {
		t.Fatalf("nil UpdateEnv must surface unavailable error, got %v", w.err)
	}
}

// TestWizard_EnvStep_EditDetailError covers the EnvDetail failure path —
// e.g. an env row disappeared between picker render and edit. The wizard
// surfaces the error and does NOT enter the URL stage on a bad lookup, so
// the operator does not type into a doomed flow.
func TestWizard_EnvStep_EditDetailError(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	f.deps.EnvDetail = func(_ string) (string, string, bool, error) {
		return "", "", false, errors.New("config file gone")
	}
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1
	w, _ = w.Update(keyRunes("e"))
	if w.stage != stageEnv || w.err == nil {
		t.Fatalf("e on a failing EnvDetail must surface an error and stay on stageEnv, got stage=%v err=%v", w.stage, w.err)
	}
}

// TestWizard_EnvStep_DeleteWithoutCallback covers the third safety branch:
// confirming a delete on a build without DeleteEnv must surface "not
// available" rather than silently ignoring the operator's confirm.
func TestWizard_EnvStep_DeleteWithoutCallback(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	f.deps.DeleteEnv = nil
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1
	w, _ = w.Update(keyRunes("d")) // arm
	w, _ = w.Update(keyRunes("d")) // confirm → nil callback
	if w.err == nil || !strings.Contains(w.err.Error(), "not available") {
		t.Fatalf("nil DeleteEnv must surface unavailable error, got %v", w.err)
	}
}

// TestWizard_EnvStep_DeleteErrorPropagates covers the DeleteEnv error path —
// e.g. the config file went read-only between the picker render and the
// confirm. The operator sees the typed error and the env stays in the list.
func TestWizard_EnvStep_DeleteErrorPropagates(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	f.deleteErr = errors.New("permission denied")
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1
	w, _ = w.Update(keyRunes("d"))
	w, _ = w.Update(keyRunes("d"))
	if w.err == nil || !strings.Contains(w.err.Error(), "permission denied") {
		t.Fatalf("delete error must surface: %v", w.err)
	}
	if got := w.deps.EnvNames; len(got) != 2 {
		t.Fatalf("env list must not change when DeleteEnv errors, got %v", got)
	}
}

// TestWizard_EnvStep_DeleteCancelOnOtherKey covers the arm-reset path: any
// keypress that is not the second `d` clears the delete-arming gate so a
// stray keystroke after `d` cannot trigger a real delete.
func TestWizard_EnvStep_DeleteCancelOnOtherKey(t *testing.T) {
	f := newEnvMgmtFixture([]string{"local", "staging"})
	w := newWizard(f.deps)
	w.Init()
	w.envCursor = 1
	w, _ = w.Update(keyRunes("d"))
	if !w.confirmDel {
		t.Fatal("first d must arm")
	}
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if w.confirmDel {
		t.Fatal("any non-d key must clear the arm")
	}
}
