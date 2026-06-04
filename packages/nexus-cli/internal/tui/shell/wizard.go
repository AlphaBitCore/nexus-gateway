package shell

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// Deps is everything the entry shell needs that the dashboard does not: the
// auth + persistence callbacks, injected so the wizard is testable without a
// browser, keychain, or disk. The funcs are nil-safe (a nil Login means "no
// interactive login available", surfaced as an error rather than a panic).
type Deps struct {
	Gateway    Gateway
	Session    Session
	EnvNames   []string // configured environments, for the entry env step
	HasSession func() bool
	// SwitchEnv re-resolves to an existing environment, rebuilds the gateway, and
	// reports whether that env already has a stored credential. Nil disables the
	// env step (single-env builds / tests).
	SwitchEnv func(name string) (Gateway, Session, bool, error)
	// CreateEnv persists a new environment (name + Control Plane URL + AI Gateway
	// URL + prod flag, other fields defaulted), sets it as default, and switches to
	// it. The AI Gateway URL is collected separately because a typical deployment
	// fronts the two services at different hosts/ports; defaulting it to the CP URL
	// silently routes /v1/* traffic to nginx, which returns 405.
	CreateEnv func(name, cpBaseURL, aigwBaseURL string, prod bool) (Gateway, Session, error)
	// UpdateEnv overwrites an existing environment's URLs + prod flag in place
	// (keeping its credential, last-used model, last-used VK). Returns the
	// rebuilt Gateway/Session + whether the env still has a usable credential.
	// Nil disables in-wizard editing.
	UpdateEnv func(name, cpBaseURL, aigwBaseURL string, prod bool) (Gateway, Session, bool, error)
	// DeleteEnv removes an environment from the config and clears its stored
	// secrets. The wizard refuses to delete the only configured env (nothing to
	// fall back to) and never deletes the currently-active env without first
	// switching away. Nil disables in-wizard deletion.
	DeleteEnv func(name string) error
	// EnvDetail returns the CP + AI Gateway URLs + prod flag for the named env so
	// the env picker can show what each row points at. Nil means the picker
	// renders names only (older builds).
	EnvDetail     func(name string) (cpURL, aigwURL string, prod bool, err error)
	Login         func(context.Context) error
	SaveVKSecret  func(secret string) error
	SaveSelection func(model, vkID, vkName string) error
	// CreateVK creates a personal Virtual Key the operator owns and returns its
	// (id, name, plaintext secret). It is how an operator without a key gets one
	// without borrowing anyone else's traffic. Injected for testability.
	CreateVK func(ctx context.Context, name string) (id, name2, secret string, err error)
	// BuildAgent constructs the gateway agent the conversation drives. The CLI
	// implements it over capabilities.BuildAgent; nil disables the conversation
	// (the dashboard stays fully navigable). See AgentBuildFunc.
	BuildAgent AgentBuildFunc
	// Log is the CLI's diagnostic file logger. The conversation mirrors a
	// user-visible turn failure into it (with env + view) so a hang the operator
	// reports can be matched against the transport timings already logged. nil
	// disables this mirroring (tests / single-shot builds).
	Log *slog.Logger
}

// wizardStage is the wizard's linear step.
type wizardStage int

const (
	stageEnv        wizardStage = iota // pick an existing environment or create one
	stageEnvName                       // create: enter the new environment's name
	stageEnvURL                        // create: enter the Control Plane URL
	stageEnvAIGWURL                    // create: enter the AI Gateway URL (defaults to the CP URL)
	stageLogin
	stageModel
	stageVK
	stageSecret
)

// modelChoice is one selectable model row (flattened from the grouped catalog).
type modelChoice struct {
	code     string
	name     string
	provider string
}

// wizard is the first-run entry flow: login → pick model → pick VK + secret.
// It runs only when the stored selection is missing/invalid; otherwise the
// shell goes straight to the dashboard (FR-13).
type wizard struct {
	deps    Deps
	session Session
	stage   wizardStage

	// gateway is the live client; it starts as deps.Gateway and is replaced when
	// the env step switches/creates an environment (so later steps + the
	// dashboard talk to the chosen deployment).
	gateway Gateway

	loggingIn   bool
	loading     bool
	loginCancel context.CancelFunc // cancels an in-flight browser login when esc is pressed at stageLogin

	envCursor   int    // selection in the env list ([len] == the "create new" row)
	newEnvName  string // captured during the create flow
	newEnvCPURL string // captured after stageEnvURL so the next step can prefill the AI Gateway field
	editingEnv  bool   // true when stageEnvURL/AIGW is editing an existing env (UpdateEnv) instead of creating one
	editAIGW    string // stash for the existing AI Gateway URL during an edit (loaded on `e`, replayed at stageEnvAIGWURL)
	confirmDel  bool   // first `d` arms; second `d` performs (matches Claude-style two-step gates without a modal)

	models      []modelChoice
	modelCursor int

	vks      []core.VirtualKey
	vkCursor int

	input  textinput.Model // env name / URL entry (create flow)
	secret textinput.Model
	err    error
	done   bool
}

type loginDoneMsg struct{ err error }
type wizModelsMsg struct {
	models []modelChoice
	err    error
}
type wizVKsMsg struct {
	vks []core.VirtualKey
	err error
}
type vkCreatedMsg struct {
	id, name, secret string
	err              error
}

// vkProbeMsg carries the result of validating the pasted VK secret against
// /v1/chat/completions before persisting it. err==nil → secret works → save +
// finish; err != nil → leave the operator on stageSecret with a typed reason
// instead of saving a secret that will 401 on the first real chat turn.
type vkProbeMsg struct {
	secret string
	err    error
}

func newWizard(d Deps) *wizard {
	ti := textinput.New()
	ti.Placeholder = "paste the Virtual Key secret (nvk_…)"
	ti.EchoMode = textinput.EchoPassword
	ti.CharLimit = 200
	ti.SetVirtualCursor(false)
	in := textinput.New()
	in.CharLimit = 200
	in.SetVirtualCursor(false)
	return &wizard{deps: d, session: d.Session, gateway: d.Gateway, secret: ti, input: in}
}

// Init starts at the env step when more than one environment is configurable
// (so the operator confirms/switches the target first); otherwise it preserves
// the original flow: straight to model selection if a session already exists,
// else login.
func (w *wizard) Init() tea.Cmd {
	w.gateway = w.deps.Gateway
	if w.deps.SwitchEnv != nil && len(w.deps.EnvNames) > 0 {
		w.stage = stageEnv
		w.envCursor = w.indexOfCurrentEnv()
		return nil
	}
	if w.deps.HasSession != nil && w.deps.HasSession() {
		w.stage = stageModel
		w.loading = true
		return w.fetchModels()
	}
	w.stage = stageLogin
	return nil
}

// indexOfCurrentEnv preselects the active environment in the env list.
func (w *wizard) indexOfCurrentEnv() int {
	for i, n := range w.deps.EnvNames {
		if n == w.session.EnvName {
			return i
		}
	}
	return 0
}

func (w *wizard) Update(msg tea.Msg) (*wizard, tea.Cmd) {
	switch msg := msg.(type) {
	case loginDoneMsg:
		// A late callback after the operator has already pressed esc to abort
		// (stage moved off stageLogin) should be discarded rather than re-routing
		// them into the model picker or surfacing a context-cancelled error on a
		// screen they have already left.
		if w.stage != stageLogin {
			w.loggingIn = false
			w.loginCancel = nil
			return w, nil
		}
		w.loggingIn = false
		w.loginCancel = nil
		if msg.err != nil {
			w.err = msg.err
			return w, nil
		}
		w.err = nil
		w.stage = stageModel
		w.loading = true
		return w, w.fetchModels()
	case wizModelsMsg:
		w.loading = false
		w.err = msg.err
		w.models = msg.models
		return w, nil
	case wizVKsMsg:
		w.loading = false
		w.err = msg.err
		w.vks = msg.vks
		return w, nil
	case vkCreatedMsg:
		w.loading = false
		if msg.err != nil {
			w.err = fmt.Errorf("create VK: %w", msg.err)
			return w, nil
		}
		w.session.VKID, w.session.VKName = msg.id, msg.name
		w.session.VKSecret = msg.secret
		if w.deps.SaveVKSecret != nil {
			if err := w.deps.SaveVKSecret(msg.secret); err != nil {
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
		w.done = true
		return w, nil
	case vkProbeMsg:
		w.loading = false
		if msg.err != nil {
			// Stay on stageSecret so the operator can correct the paste — never
			// persist a secret that already failed against the live gateway.
			w.err = msg.err
			return w, w.secret.Focus()
		}
		return w.persistSecret(msg.secret)
	case tea.PasteMsg:
		// Bracketed paste into the active wizard field. The wizard has TWO inputs
		// (w.input for env-name + Control Plane URL; w.secret for the VK secret), and
		// only the focused one should receive the paste — routing to the wrong one
		// silently drops the paste (the bug pasting a VK secret hit). Route by stage.
		var cmd tea.Cmd
		if w.stage == stageSecret {
			w.secret, cmd = w.secret.Update(msg)
		} else {
			w.input, cmd = w.input.Update(msg)
		}
		return w, cmd
	case tea.KeyPressMsg:
		return w.updateKey(msg)
	}
	return w, nil
}

func (w *wizard) updateKey(msg tea.KeyPressMsg) (*wizard, tea.Cmd) {
	// esc walks back one stage so a wrong pick is recoverable without a restart;
	// from the env picker there is nothing to back out of (ctrl+c quits).
	if msg.String() == "esc" && w.stage != stageEnv {
		return w.goBack()
	}
	switch w.stage {
	case stageEnv:
		// Any keypress that is not the second `d` clears the delete-arming
		// gate so a stray keystroke after `d` cannot trigger a real delete.
		key := msg.String()
		if key != "d" {
			w.confirmDel = false
		}
		switch key {
		case "up", "k":
			if w.envCursor > 0 {
				w.envCursor--
			}
		case "down", "j":
			if w.envCursor < len(w.deps.EnvNames) { // last index = the "create new" row
				w.envCursor++
			}
		case "enter":
			return w.selectEnv()
		case "e":
			// Edit the URLs of the selected env in place (UpdateEnv at the end
			// of stageEnvAIGWURL instead of CreateEnv). The "create new" row is
			// skipped — there is no env to edit there.
			if w.envCursor >= len(w.deps.EnvNames) {
				return w, nil
			}
			return w.startEditEnv(w.deps.EnvNames[w.envCursor])
		case "v":
			// Change the Virtual Key for the selected env (model→VK→secret), the
			// only path to re-key an already-configured env. The "create new" row
			// has no env to re-key.
			if w.envCursor >= len(w.deps.EnvNames) {
				return w, nil
			}
			return w.startChangeVK()
		case "d":
			return w.handleDeleteEnv()
		}
	case stageEnvName:
		if msg.Code == tea.KeyEnter {
			name := strings.TrimSpace(w.input.Value())
			if name == "" {
				w.err = fmt.Errorf("environment name is required")
				return w, nil
			}
			w.err = nil
			w.newEnvName = name
			w.input.SetValue("")
			w.input.Placeholder = "Control Plane base URL (https://…)"
			w.stage = stageEnvURL
			return w, w.input.Focus()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return w, cmd
	case stageEnvURL:
		if msg.Code == tea.KeyEnter {
			cp := strings.TrimSpace(w.input.Value())
			if cp == "" {
				w.err = fmt.Errorf("Control Plane URL is required")
				return w, nil
			}
			w.err = nil
			w.newEnvCPURL = cp
			// Prefill the AI Gateway URL. In edit mode we replay the existing
			// AI Gateway URL so the operator does not lose a multi-host
			// configuration by pressing enter; in create mode we default it
			// to the CP URL so single-host deployments just hit enter.
			prefill := cp
			if w.editingEnv && strings.TrimSpace(w.editAIGW) != "" {
				prefill = w.editAIGW
			}
			w.input.SetValue(prefill)
			w.input.Placeholder = "AI Gateway base URL (https://…)"
			w.input.CursorEnd()
			w.stage = stageEnvAIGWURL
			return w, w.input.Focus()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return w, cmd
	case stageEnvAIGWURL:
		if msg.Code == tea.KeyEnter {
			return w.createEnv()
		}
		var cmd tea.Cmd
		w.input, cmd = w.input.Update(msg)
		return w, cmd
	case stageLogin:
		if msg.Code == tea.KeyEnter && !w.loggingIn {
			return w.startLogin()
		}
	case stageModel:
		switch msg.String() {
		case "up", "k":
			if w.modelCursor > 0 {
				w.modelCursor--
			}
		case "down", "j":
			if w.modelCursor < len(w.models)-1 {
				w.modelCursor++
			}
		case "enter":
			if w.modelCursor < len(w.models) {
				w.session.Model = w.models[w.modelCursor].code
				w.stage = stageVK
				w.loading = true
				return w, w.fetchVKs()
			}
		}
	case stageVK:
		switch msg.String() {
		case "up", "k":
			if w.vkCursor > 0 {
				w.vkCursor--
			}
		case "down", "j":
			if w.vkCursor < len(w.vks)-1 {
				w.vkCursor++
			}
		case "c":
			// Create a brand-new VK the operator owns (no borrowed traffic).
			return w.startCreateVK()
		case "enter":
			if w.vkCursor < len(w.vks) {
				w.session.VKID = w.vks[w.vkCursor].ID
				w.session.VKName = w.vks[w.vkCursor].Name
				w.stage = stageSecret
				return w, w.secret.Focus()
			}
		}
	case stageSecret:
		if msg.Code == tea.KeyEnter {
			return w.finish()
		}
		var cmd tea.Cmd
		w.secret, cmd = w.secret.Update(msg)
		return w, cmd
	}
	return w, nil
}

// selectEnv switches to the chosen existing environment (rebuilding the
// gateway) or, on the "create new" row, enters the create flow. A switched env
// that is already fully configured finishes the wizard immediately.
