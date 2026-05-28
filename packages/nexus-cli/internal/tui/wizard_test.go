package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// recordingDeps builds Deps whose callbacks record what the wizard did, so a
// test can assert the persisted selection without a keychain or disk.
type recordingDeps struct {
	deps        Deps
	loginCalls  int
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
	r.deps.CreateEnv = func(name, url string, prod bool) (Gateway, Session, error) {
		r.createdEnvName, r.createdURL, r.createdProd = name, url, prod
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
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	w, cmd = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.stage != stageVK {
		t.Fatalf("after model pick should be VK stage, got %d", w.stage)
	}
	w, _ = runStep(t, w, cmd) // wizVKsMsg
	if len(w.vks) != 1 {
		t.Fatalf("vks not loaded: %+v", w.vks)
	}
	// pick VK → secret stage
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.stage != stageSecret {
		t.Fatalf("after VK pick should be secret stage, got %d", w.stage)
	}
	// type the secret + enter
	w.secret.SetValue("nvk_topsecret")
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !w.done {
		t.Fatal("wizard should be done after the secret is entered")
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
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // → stageVK with VKs loaded
	if w.stage != stageVK {
		t.Fatalf("expected stageVK, got %d", w.stage)
	}
	// press 'c' to create a new owned VK → vkCreatedMsg → done
	w, cmd = w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
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
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	w, _ = runStep(t, w, cmd)
	if w.done || w.err == nil || !strings.Contains(w.err.Error(), "403 denied") {
		t.Fatalf("create error should block done: %v", w.err)
	}
}

func TestWizard_CreateVKUnavailable(t *testing.T) {
	d := Deps{Gateway: sampleGateway(), Session: Session{EnvName: "local"}, HasSession: func() bool { return true }}
	w := newWizard(d)
	w.stage = stageVK
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
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
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	w, _ = runStep(t, w, cmd) // loginDoneMsg{err}
	if w.stage != stageLogin || w.err == nil || !strings.Contains(w.View(100, 24), "redirect mismatch") {
		t.Fatalf("login error should keep the wizard at login with the error shown")
	}
}

func TestWizard_NoLoginConfigured(t *testing.T) {
	d := Deps{Gateway: sampleGateway(), Session: Session{EnvName: "local"}, HasSession: func() bool { return false }}
	w := newWizard(d)
	w.Init()
	w, cmd := w.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter}) // empty secret
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.done || w.err == nil || !strings.Contains(w.err.Error(), "keychain locked") {
		t.Fatalf("SaveVKSecret error should block done: %v", w.err)
	}
	// now secret OK but selection save fails
	r2 := newRecordingDeps(sampleGateway())
	r2.saveSelErr = errors.New("disk full")
	w2 := newWizard(r2.deps)
	w2.stage = stageSecret
	w2.secret.SetValue("nvk_x")
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w2.done || w2.err == nil || !strings.Contains(w2.err.Error(), "disk full") {
		t.Fatalf("SaveSelection error should block done: %v", w2.err)
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyUp}) // already at 0
	if w.modelCursor != 0 {
		t.Fatal("up at top should clamp")
	}
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyDown}) // only 1 model
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyUp})
	if w.envCursor != 0 {
		t.Fatal("up at top should stay")
	}
	for i := 0; i < 5; i++ {
		w, _ = w.Update(tea.KeyMsg{Type: tea.KeyDown})
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyDown})    // → dev
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})   // select dev
	if r.switchedTo != "dev" || w.stage != stageLogin {
		t.Fatalf("not-logged-in switch should go to login; switched=%q stage=%v", r.switchedTo, w.stage)
	}

	// logged in but no model/VK → model fetch.
	r2 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r2.switchLoggedIn = true
	w2 := newWizard(r2.deps)
	w2.Init()
	w2, cmd := w2.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	w3, _ = w3.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !w3.done {
		t.Fatal("a fully-configured logged-in env should finish the wizard")
	}
}

func TestWizard_EnvStep_CreateFlow(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	w := newWizard(r.deps)
	w.Init()
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyDown})  // → create row
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → name entry
	if w.stage != stageEnvName {
		t.Fatalf("create row should enter the name stage, got %v", w.stage)
	}
	// empty name is rejected.
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnvName {
		t.Fatal("empty env name should error and stay on the name stage")
	}
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("staging")})
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.stage != stageEnvURL || w.newEnvName != "staging" {
		t.Fatalf("name → url; name=%q stage=%v", w.newEnvName, w.stage)
	}
	if !strings.Contains(w.View(100, 24), "Control Plane URL") {
		t.Fatal("url stage should render")
	}
	// empty URL is rejected.
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnvURL {
		t.Fatal("empty URL should error and stay")
	}
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("https://stg")})
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if r.createdEnvName != "staging" || r.createdURL != "https://stg" || r.createdProd {
		t.Fatalf("CreateEnv args wrong: name=%q url=%q prod=%v", r.createdEnvName, r.createdURL, r.createdProd)
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
	w, _ = w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if w.err == nil || w.stage != stageEnv {
		t.Fatalf("switch error should surface and stay on env stage: err=%v stage=%v", w.err, w.stage)
	}
	// CreateEnv error surfaces.
	r2 := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local"})
	r2.createEnvErr = errors.New("create boom")
	w2 := newWizard(r2.deps)
	w2.Init()
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyDown})
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter}) // name stage
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter}) // url stage
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("https://x")})
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter}) // create → error
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
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyDown})                                  // create row
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter})                                 // name stage
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})             // name
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter})                                 // url stage
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("https://x")})     // url
	w2, _ = w2.Update(tea.KeyMsg{Type: tea.KeyEnter})                                 // create → nil callback
	if w2.err == nil || !strings.Contains(w2.err.Error(), "not available") {
		t.Fatalf("nil CreateEnv should surface unavailable error, got %v", w2.err)
	}
}
