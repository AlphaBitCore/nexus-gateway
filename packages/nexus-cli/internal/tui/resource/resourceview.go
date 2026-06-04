package resource

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	capres "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/restable"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// resourceView is the /resource cascade: a deterministic, local, zero-LLM browser
// over EVERY Control Plane admin resource, driven entirely by the embedded OpenAPI
// catalog. The operator picks a kind, then navigates a stack of frames — a
// collection table, a single record, or an operation menu — drilling into nested
// sub-resources at any depth (a record's sub-collections bind the next path
// parameter, and so on). Reads run immediately; writes (create/update/delete,
// singleton config, RPCs, item actions) are gated by an Allow/Deny confirm and
// collect their body/params through an OpenAPI-driven form. It is the generic
// counterpart to the dedicated views (Nodes/Keys/Rules/…), reaching the long tail
// of kinds that have no bespoke screen — the same engine the CLI `nexus resource`
// command and the agent's resource_* tools use.
type resourceView struct {
	gw      AdminClient
	session kit.Session
	cf      kit.Confirm
	// kind picker (the root of the cascade)
	kinds   []capres.KindInfo
	filter  textinput.Model
	kindCur int

	// navigation stack within a kind (empty => at the kind picker)
	stack []*resFrame

	// transient input overlay (filter / params / body); nil => none
	form *opForm

	busy        bool
	pendingKind string // kind being opened while busy (loading label)
	token       int    // guards against stale async fetches
	err         error
	note        string // transient success note after a write
}

type resFrameMode int

const (
	frameTable  resFrameMode = iota // a collection: rows + pagination
	frameRecord                     // a single record (raw JSON) + optional child-op menu
	frameMenu                       // an operations menu
)

const (
	resPageSize = 12 // table rows per page
	resTableCol = 6  // max inferred columns
)

// resFrame is one level of the navigation stack. params holds every path
// placeholder bound by this frame and its ancestors, so a child operation at any
// depth resolves fully. cursor indexes the page rows (table) or the menu (record /
// menu).
type resFrame struct {
	kind   string
	title  string
	mode   resFrameMode
	params map[string]string

	op   capres.OperationInfo // the GET that produced this frame's content
	rows []restable.Row
	raw  json.RawMessage
	cols []string
	page int

	menu   []capres.OperationInfo
	cursor int
	query  map[string]string
}

// resDataMsg carries a GET result back to the view. replace swaps the top frame
// (a re-filter) instead of pushing a new level.
type resDataMsg struct {
	token   int
	kind    string
	op      capres.OperationInfo
	params  map[string]string
	query   map[string]string
	menu    []capres.OperationInfo
	title   string
	raw     json.RawMessage
	err     error
	replace bool
}

// resWriteMsg carries a write result; it is shown as a record leaf plus a note.
type resWriteMsg struct {
	token  int
	kind   string
	label  string
	raw    json.RawMessage
	status int
	err    error
}

func newResource(gw AdminClient, s ...kit.Session) *resourceView {
	ti := textinput.New()
	ti.Placeholder = "filter kinds…"
	ti.Prompt = ""
	ti.SetVirtualCursor(false)
	ti.Focus()
	sess := kit.OptSession(s)
	return &resourceView{gw: gw, session: sess, cf: kit.NewConfirm(sess), kinds: capres.Kinds(), filter: ti}
}

func (r *resourceView) Init() tea.Cmd { return nil }

// capturing routes the keyboard to this view while it owns text input: the kind
// filter (picker), an open form, or the confirm gate. Inside the frame stack the
// view is NORMAL-mode, so the root's ↑/↓ + ←/esc drive it.
func (r *resourceView) Capturing() bool {
	return r.cf.Capturing() || r.form != nil || len(r.stack) == 0
}

func (r *resourceView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case resDataMsg:
		if msg.token != r.token {
			return r, nil // a stale fetch (the operator moved on); a newer one is still in flight
		}
		r.busy = false
		if msg.err != nil {
			r.err = msg.err
			return r, nil
		}
		r.err = nil
		fr := buildFrame(msg)
		if msg.replace && len(r.stack) > 0 {
			r.stack[len(r.stack)-1] = fr
		} else {
			r.stack = append(r.stack, fr)
		}
		return r, nil
	case resWriteMsg:
		if msg.token != r.token {
			return r, nil
		}
		r.busy = false
		if msg.err != nil {
			r.err = msg.err
			return r, nil
		}
		r.err = nil
		r.note = fmt.Sprintf("✓ %s → %d", msg.label, msg.status)
		r.stack = append(r.stack, &resFrame{kind: msg.kind, title: msg.label + " ✓", mode: frameRecord, raw: msg.raw})
		return r, nil
	case tea.KeyPressMsg:
		return r.handleKey(msg)
	}
	return r, nil
}

func (r *resourceView) handleKey(msg tea.KeyPressMsg) (kit.ViewModel, tea.Cmd) {
	if handled, cmd := r.cf.Update(msg); handled {
		return r, cmd
	}
	if r.form != nil {
		if msg.String() == "esc" {
			r.form = nil
			return r, nil
		}
		cmd, submitted := r.form.update(msg)
		if submitted {
			r.form = nil
		}
		return r, cmd
	}
	if len(r.stack) == 0 {
		return r.handlePickerKey(msg)
	}
	return r.handleFrameKey(msg)
}

// --- kind picker ---

func (r *resourceView) handlePickerKey(k tea.KeyPressMsg) (kit.ViewModel, tea.Cmd) {
	switch k.String() {
	case "up":
		if r.kindCur > 0 {
			r.kindCur--
		}
		return r, nil
	case "down":
		if r.kindCur < len(r.filtered())-1 {
			r.kindCur++
		}
		return r, nil
	case "enter":
		ki, ok := r.selectedKind()
		if !ok {
			return r, nil
		}
		return r, r.enterKind(ki.Kind)
	case "esc":
		return r, func() tea.Msg { return kit.JumpMsg{Index: 0} } // back to the cockpit
	}
	var cmd tea.Cmd
	r.filter, cmd = r.filter.Update(k)
	r.kindCur = 0 // a filter change resets the cursor to the top match
	return r, cmd
}

func (r *resourceView) filtered() []capres.KindInfo {
	q := strings.ToLower(strings.TrimSpace(r.filter.Value()))
	if q == "" {
		return r.kinds
	}
	out := make([]capres.KindInfo, 0, len(r.kinds))
	for _, k := range r.kinds {
		if strings.Contains(strings.ToLower(k.Kind), q) {
			out = append(out, k)
		}
	}
	return out
}

func (r *resourceView) selectedKind() (capres.KindInfo, bool) {
	f := r.filtered()
	if r.kindCur < 0 || r.kindCur >= len(f) {
		return capres.KindInfo{}, false
	}
	return f[r.kindCur], true
}

// enterKind opens a kind: a canonical list lands straight on its collection table;
// a kind with no list (reports / singleton config / RPC / two-level resources)
// lands on an operation menu. Either way every operation is reachable.
func (r *resourceView) enterKind(kind string) tea.Cmd {
	r.stack, r.err, r.note = nil, nil, ""
	r.token++     // invalidate any fetch in flight so its late result can't land on the new kind
	r.cf.Cancel() // a confirm raised under the previous kind must never fire here
	r.busy = false
	for _, op := range capres.Operations(kind) {
		if op.Verb == "list" {
			r.pendingKind = kind
			return r.runGet(kind, op, map[string]string{}, nil, nil, kind)
		}
	}
	r.pushMenu(kind, map[string]string{}, capres.Operations(kind), kind)
	return nil
}

// --- frame navigation ---

func (r *resourceView) top() *resFrame { return r.stack[len(r.stack)-1] }

func (r *resourceView) handleFrameKey(k tea.KeyPressMsg) (kit.ViewModel, tea.Cmd) {
	fr := r.top()
	switch fr.mode {
	case frameTable:
		return r.handleTableKey(fr, k)
	default: // frameMenu / frameRecord both navigate a menu
		return r.handleMenuKey(fr, k)
	}
}

func (r *resourceView) handleTableKey(fr *resFrame, k tea.KeyPressMsg) (kit.ViewModel, tea.Cmd) {
	rows := fr.pageRows()
	switch k.String() {
	case "up":
		if fr.cursor > 0 {
			fr.cursor--
		}
	case "down":
		if fr.cursor < len(rows)-1 {
			fr.cursor++
		}
	case "n", "pgdown", "right":
		if (fr.page+1)*resPageSize < len(fr.rows) {
			fr.page++
			fr.cursor = 0
		}
	case "p", "pgup":
		if fr.page > 0 {
			fr.page--
			fr.cursor = 0
		}
	case "enter":
		return r, r.drillRow(fr)
	case "o":
		r.openCollectionMenu(fr)
	case "f":
		return r, r.openFilter(fr)
	}
	return r, nil
}

func (r *resourceView) handleMenuKey(fr *resFrame, k tea.KeyPressMsg) (kit.ViewModel, tea.Cmd) {
	switch k.String() {
	case "up":
		if fr.cursor > 0 {
			fr.cursor--
		}
	case "down":
		if fr.cursor < len(fr.menu)-1 {
			fr.cursor++
		}
	case "enter":
		if fr.cursor >= 0 && fr.cursor < len(fr.menu) {
			return r, r.activateOp(fr, fr.menu[fr.cursor])
		}
	}
	return r, nil
}

// drillRow descends into the selected collection row, binding its id to the next
// path parameter and showing that record plus its child operations. A row of a
// collection with no deeper operation is shown as a leaf record.
func (r *resourceView) drillRow(fr *resFrame) tea.Cmd {
	rows := fr.pageRows()
	if fr.cursor < 0 || fr.cursor >= len(rows) {
		return nil
	}
	row := rows[fr.cursor]
	id := restable.ID(row)
	childParam := childParamOf(fr.kind, fr.op.Path)
	if childParam == "" || id == "" {
		raw, _ := json.MarshalIndent(row, "", "  ")
		r.pushRecord(fr.kind, fr.params, raw, nil, restable.Label(row))
		return nil
	}
	params := cloneParams(fr.params)
	params[childParam] = id
	itemBase := fr.op.Path + "/{" + childParam + "}"
	menu := childOps(fr.kind, itemBase)
	if getOp, ok := findGet(fr.kind, itemBase); ok {
		return r.runGet(fr.kind, getOp, params, nil, menu, restable.Label(row))
	}
	raw, _ := json.MarshalIndent(row, "", "  ")
	r.pushRecord(fr.kind, params, raw, menu, restable.Label(row))
	return nil
}

// openCollectionMenu shows the collection-level operations (create, reports, RPCs,
// singleton config) — every op with no path parameter — for the current kind.
func (r *resourceView) openCollectionMenu(fr *resFrame) {
	var ops []capres.OperationInfo
	for _, op := range capres.Operations(fr.kind) {
		if len(op.Params) == 0 {
			ops = append(ops, op)
		}
	}
	r.pushMenu(fr.kind, map[string]string{}, ops, fr.kind+" · operations")
}

// openFilter raises a form over the list's query parameters (from the OpenAPI
// spec) and re-runs the list with the chosen filters.
func (r *resourceView) openFilter(fr *resFrame) tea.Cmd {
	schema, _ := capres.DescribeOperation(fr.kind, fr.op.OperationID)
	var q []capres.FieldInfo
	for _, p := range schema.Params {
		if p.In == "query" {
			q = append(q, p)
		}
	}
	if len(q) == 0 {
		r.note = "this list has no filters"
		return nil
	}
	kind, op, params, title := fr.kind, fr.op, fr.params, fr.title
	r.form = newOpForm("Filter · "+fr.kind, formFilter, q, func(vals map[string]string) tea.Cmd {
		return r.runGetMode(kind, op, params, vals, nil, title, true)
	})
	return nil
}

// activateOp runs (or drills into) a menu operation: missing path parameters are
// collected by a form first; a write collects its body and confirms; a read runs
// and pushes the resulting frame.
func (r *resourceView) activateOp(fr *resFrame, op capres.OperationInfo) tea.Cmd {
	if miss := missingParams(op, fr.params); len(miss) > 0 {
		base := cloneParams(fr.params)
		kind := fr.kind
		r.form = paramForm("Parameters · "+op.Label, miss, func(vals map[string]string) tea.Cmd {
			merged := cloneParams(base)
			for k, v := range vals {
				merged[k] = v
			}
			return r.activateResolved(kind, op, merged)
		})
		return nil
	}
	return r.activateResolved(fr.kind, op, fr.params)
}

func (r *resourceView) activateResolved(kind string, op capres.OperationInfo, params map[string]string) tea.Cmd {
	if op.Mutating {
		schema, _ := capres.DescribeOperation(kind, op.OperationID)
		if len(schema.Body) > 0 {
			var form *opForm
			form = newOpForm("Body · "+op.Label, formBody, schema.Body, func(_ map[string]string) tea.Cmd {
				return r.runWrite(kind, op, params, nil, form.bodyJSON())
			})
			r.form = form
			return nil
		}
		return r.runWrite(kind, op, params, nil, nil)
	}
	return r.runGet(kind, op, params, nil, childOps(kind, op.Path), op.Label)
}

// --- execution ---

func (r *resourceView) runGet(kind string, op capres.OperationInfo, params, query map[string]string, menu []capres.OperationInfo, title string) tea.Cmd {
	return r.runGetMode(kind, op, params, query, menu, title, false)
}

func (r *resourceView) runGetMode(kind string, op capres.OperationInfo, params, query map[string]string, menu []capres.OperationInfo, title string, replace bool) tea.Cmd {
	method, path, _, err := capres.ResolveOperation(kind, op.OperationID, params)
	if err != nil {
		r.err = err
		return nil
	}
	r.busy, r.err = true, nil
	r.token++
	token := r.token
	q := toValues(query)
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		raw, _, err := r.gw.AdminRequest(ctx, method, path, q, nil)
		return resDataMsg{token: token, kind: kind, op: op, params: params, query: query, menu: menu, title: title, raw: raw, err: err, replace: replace}
	}
}

func (r *resourceView) runWrite(kind string, op capres.OperationInfo, params, query map[string]string, body json.RawMessage) tea.Cmd {
	method, path, _, err := capres.ResolveOperation(kind, op.OperationID, params)
	if err != nil {
		r.err = err
		return nil
	}
	q := toValues(query)
	label := op.Label
	return r.cf.Begin(fmt.Sprintf("%s %s", method, path), func() tea.Cmd {
		r.busy = true
		r.token++
		token := r.token
		var bodyArg any
		if len(body) > 0 {
			bodyArg = body
		}
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
			defer cancel()
			raw, status, err := r.gw.AdminRequest(ctx, method, path, q, bodyArg)
			return resWriteMsg{token: token, kind: kind, label: label, raw: raw, status: status, err: err}
		}
	})
}

// --- frame builders ---
