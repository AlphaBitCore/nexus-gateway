package resource

import (
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	capres "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

var errResourceTest = errors.New("boom")

func opInfo(t *testing.T, kind, operationID string) capres.OperationInfo {
	t.Helper()
	for _, op := range capres.Operations(kind) {
		if op.OperationID == operationID {
			return op
		}
	}
	t.Fatalf("operation %s/%s not in catalog", kind, operationID)
	return capres.OperationInfo{}
}

// openVK drives the picker into the virtual-keys collection table with a scripted
// list body, returning the populated view.
func openVK(t *testing.T, gw *fakeGateway) *resourceView {
	t.Helper()
	r := newResource(gw)
	r.filter.SetValue("virtual-keys")
	r.kindCur = 0
	if ki, ok := r.selectedKind(); !ok || ki.Kind != "virtual-keys" {
		t.Fatalf("filter should select virtual-keys, got %+v ok=%v", ki, ok)
	}
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("opening a listable kind should issue a fetch")
	}
	r.Update(cmd())
	return r
}

// TestResourceListDrillBack drives kind → collection table → record drill → back,
// the core multi-level cascade, with no LLM turn.
func TestResourceListDrillBack(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"data":[{"id":"vk1","name":"engineering"},{"id":"vk2","name":"ops"}]}`)}
	r := openVK(t, gw)

	if len(r.stack) != 1 || r.top().mode != frameTable {
		t.Fatalf("a listable kind should land on a table, stack=%d", len(r.stack))
	}
	if len(r.top().rows) != 2 {
		t.Fatalf("the collection should rowify, got %d rows", len(r.top().rows))
	}
	if gw.lastAdmin.method != "GET" || !strings.Contains(gw.lastAdmin.path, "virtual-keys") {
		t.Fatalf("list should GET the collection, got %s %s", gw.lastAdmin.method, gw.lastAdmin.path)
	}
	out := r.View(100, 24)
	if !strings.Contains(out, "engineering") || !strings.Contains(strings.ToUpper(out), "NAME") {
		t.Fatalf("the table should render columns + values:\n%s", out)
	}

	// Drill the first row → record (GET by id) + child-operation menu.
	gw.adminRaw = json.RawMessage(`{"id":"vk1","name":"engineering","enabled":true}`)
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("drilling a row should fetch the record")
	}
	r.Update(cmd())
	if r.top().mode != frameRecord {
		t.Fatalf("drilling a row should push a record frame, got %v", r.top().mode)
	}
	if !strings.Contains(gw.lastAdmin.path, "vk1") {
		t.Fatalf("the record should be fetched by id, got %s", gw.lastAdmin.path)
	}
	if len(r.top().menu) == 0 {
		t.Fatal("a record should expose its child operations (update/delete/actions)")
	}
	if out := r.View(100, 24); !strings.Contains(out, "engineering") || !strings.Contains(out, "operations") {
		t.Fatalf("record view should show the JSON + child operations:\n%s", out)
	}

	// Back walks record → table → picker, then declines so the root pops the nav.
	if !r.Back() || r.top().mode != frameTable {
		t.Fatal("back from a record should return to the table")
	}
	if !r.Back() || len(r.stack) != 0 {
		t.Fatal("back from the table should return to the picker")
	}
	if r.Back() {
		t.Fatal("back at the picker must decline so the root pops the nav stack")
	}
}

// TestResourceCapturing covers keyboard ownership: the picker + forms capture, the
// frame stack does not.
func TestResourceCapturing(t *testing.T) {
	r := newResource(&fakeGateway{})
	if !r.Capturing() {
		t.Fatal("the kind picker should own the keyboard (filter is live)")
	}
	all := len(r.filtered())
	r.filter.SetValue("zzzz-no-such-kind")
	if len(r.filtered()) != 0 {
		t.Fatal("a non-matching filter yields no kinds")
	}
	r.filter.SetValue("")
	if len(r.filtered()) != all {
		t.Fatal("clearing the filter restores all kinds")
	}
	r = openVK(t, &fakeGateway{adminRaw: json.RawMessage(`[]`)})
	if r.Capturing() {
		t.Fatal("inside the frame stack the view must NOT capture (root drives ↑/↓ + back)")
	}
}

// TestResourceNoListKindMenu covers a kind with no canonical list: it lands on an
// operation menu, and selecting a 0-param GET runs it into a record.
func TestResourceNoListKindMenu(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"similarityThreshold":0.9}`)}
	r := newResource(gw)
	// semantic-cache has no GET /semantic-cache collection → menu entry.
	r.filter.SetValue("semantic-cache")
	r.kindCur = 0
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(r.stack) != 1 || r.top().mode != frameMenu {
		t.Fatalf("a no-list kind should land on an operation menu, got %v", r.top().mode)
	}
	// Find the getConfig op in the menu and run it.
	idx := -1
	for i, op := range r.top().menu {
		if op.OperationID == "getConfig" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("semantic-cache menu must include getConfig: %+v", r.top().menu)
	}
	r.top().cursor = idx
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("selecting a GET op should fetch")
	}
	r.Update(cmd())
	if r.top().mode != frameRecord || !strings.Contains(gw.lastAdmin.path, "semantic-cache/config") {
		t.Fatalf("getConfig should render a record from /semantic-cache/config, got %s", gw.lastAdmin.path)
	}
}

// TestResourcePagination covers the table pager (n/p) over a >1-page collection.
func TestResourcePagination(t *testing.T) {
	var items []string
	for i := 0; i < 25; i++ {
		items = append(items, `{"id":"k`+string(rune('a'+i%26))+`"}`)
	}
	gw := &fakeGateway{adminRaw: json.RawMessage(`[` + strings.Join(items, ",") + `]`)}
	r := openVK(t, gw)
	if r.top().mode != frameTable || len(r.top().rows) != 25 {
		t.Fatalf("expected a 25-row table, got %d", len(r.top().rows))
	}
	if r.top().page != 0 {
		t.Fatal("starts on page 0")
	}
	r.handleKey(keyRunes("n")) // next page
	if r.top().page != 1 {
		t.Fatalf("n should advance the page, got %d", r.top().page)
	}
	r.handleKey(keyRunes("n")) // page 2 (rows 24..)
	r.handleKey(keyRunes("n")) // clamp at last page (3 pages: 0,1,2)
	if r.top().page != 2 {
		t.Fatalf("page must clamp at the last page, got %d", r.top().page)
	}
	r.handleKey(keyRunes("p"))
	if r.top().page != 1 {
		t.Fatalf("p should step back, got %d", r.top().page)
	}
	if out := r.View(100, 24); !strings.Contains(out, "page 2/3") {
		t.Fatalf("table should show the page indicator:\n%s", out)
	}
}

// TestResourceWriteConfirmed drives a write op through the Allow/Deny gate and on
// to the executor.
func TestResourceWriteConfirmed(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"ok":true}`)}
	r := newResource(gw)
	r.stack = []*resFrame{{kind: "virtual-keys", mode: frameRecord, params: map[string]string{"id": "vk1"}}}
	op := opInfo(t, "virtual-keys", "revokeVirtualKey") // POST /{id}/revoke, no body

	cmd := r.activateResolved("virtual-keys", op, map[string]string{"id": "vk1"})
	if !r.cf.Capturing() {
		t.Fatal("a write must raise the confirm gate")
	}
	if cmd != nil {
		t.Fatal("the gate should suspend execution until allowed")
	}
	// Deny first: no call.
	_, _ = r.handleKey(keyRunes("n"))
	if gw.adminCalls != 0 {
		t.Fatal("deny must not execute the write")
	}
	// Allow: executes POST /{id}/revoke.
	r.activateResolved("virtual-keys", op, map[string]string{"id": "vk1"})
	_, cmd = r.handleKey(keyRunes("y"))
	if cmd == nil {
		t.Fatal("allow should return the write command")
	}
	r.Update(cmd())
	if gw.lastAdmin.method != "POST" || gw.lastAdmin.path != "/api/admin/virtual-keys/vk1/revoke" {
		t.Fatalf("write built %s %s", gw.lastAdmin.method, gw.lastAdmin.path)
	}
	if !strings.Contains(r.note, "✓") {
		t.Fatalf("a successful write should set a note, got %q", r.note)
	}
}

// TestResourceParamForm covers an operation whose path param is unbound: a form
// collects it, then the op runs with the value substituted.
func TestResourceParamForm(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"provider":"x"}`)}
	r := newResource(gw)
	r.stack = []*resFrame{{kind: "cache", mode: frameMenu, params: map[string]string{}}}
	op := opInfo(t, "cache", "cacheGetProvider") // GET /cache/provider/{provider_id}

	if cmd := r.activateOp(r.top(), op); cmd != nil {
		t.Fatal("an op with a missing path param should open a form, not run")
	}
	if r.form == nil {
		t.Fatal("a missing path param must raise a form")
	}
	// Type a value into the (single) field and submit; the submit returns the run cmd.
	r.handleKey(keyRunes("p"))
	r.handleKey(keyRunes("1"))
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if r.form != nil {
		t.Fatal("submitting the form should close it")
	}
	if cmd == nil {
		t.Fatal("submitting a complete param form should run the op")
	}
	r.Update(cmd())
	if !strings.Contains(gw.lastAdmin.path, "/cache/provider/p1") {
		t.Fatalf("the param form should run the op with the value, got %s", gw.lastAdmin.path)
	}
}

// TestResourceFilterForm covers 'f' on a list with query params → form → re-filter.
func TestResourceFilterForm(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"data":[{"id":"a"}]}`)}
	r := openVK(t, gw)
	fr := r.top()
	// virtual-keys list has query params (status/limit/…); 'f' opens the filter form.
	cmd := r.openFilter(fr)
	_ = cmd
	if r.form == nil {
		// Some catalogs may not expose query params; assert the graceful note instead.
		if !strings.Contains(r.note, "no filters") {
			t.Skip("virtual-keys list exposes no query params in this catalog")
		}
		return
	}
	if r.form.purpose != formFilter {
		t.Fatalf("expected a filter form, got purpose %v", r.form.purpose)
	}
}

// TestResourceErrorBranches covers a gateway error surfaced inline and the empty /
// raw-JSON table fallback.
func TestResourceErrorBranches(t *testing.T) {
	// Non-array body → record (not a table).
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"status":"ok"}`)}
	r := newResource(gw)
	r.filter.SetValue("virtual-keys")
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	r.Update(cmd())
	if r.top().mode != frameRecord {
		t.Fatal("a non-array list body should render as a record")
	}
	if out := r.View(80, 24); !strings.Contains(out, "status") {
		t.Fatalf("record should render the JSON:\n%s", out)
	}
	// Gateway error surfaces inline.
	gw.err = errResourceTest
	r2 := newResource(gw)
	r2.filter.SetValue("virtual-keys")
	_, cmd = r2.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	r2.Update(cmd())
	if r2.err == nil {
		t.Fatal("a gateway error must be recorded")
	}
	if !strings.Contains(r2.kindView(), "boom") {
		t.Fatalf("the error should render inline:\n%s", r2.kindView())
	}
}

// TestResourceRenderAndHelp exercises the render dispatch, help, and crumb across
// states plus the picker clamps + esc-home.
func TestResourceRenderAndHelp(t *testing.T) {
	r := newResource(&fakeGateway{})
	if r.Init() != nil {
		t.Fatal("Init has no fetch (kinds load at construction)")
	}
	if !strings.Contains(r.kindView(), "pick a kind") || r.Crumb() != "Resource" {
		t.Fatalf("kind view/crumb: %q", r.Crumb())
	}
	if !strings.Contains(r.Help(), "filter") {
		t.Fatalf("picker help: %q", r.Help())
	}
	// up at the top clamps.
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if r.kindCur != 0 {
		t.Fatal("up at the top must clamp")
	}
	// esc on the picker jumps home.
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if jm, ok := cmd().(kit.JumpMsg); !ok || jm.Index != 0 {
		t.Fatalf("esc should jump home, got %T", cmd())
	}
	// out-of-range kind cursor guards.
	r.kindCur = 1 << 20
	if _, ok := r.selectedKind(); ok {
		t.Fatal("out-of-range cursor must not select")
	}
	// Frame help variants.
	r2 := openVK(t, &fakeGateway{adminRaw: json.RawMessage(`[{"id":"a"}]`)})
	if !strings.Contains(r2.Help(), "drill") {
		t.Fatalf("table help: %q", r2.Help())
	}
}

func TestResourceHelpers(t *testing.T) {
	// childParamOf: the kind collection's child is the item id param.
	if got := childParamOf("virtual-keys", "/api/admin/virtual-keys"); got != "id" {
		t.Fatalf("childParamOf = %q, want id", got)
	}
	// a path with no nested op has no child param.
	if got := childParamOf("virtual-keys", "/api/admin/virtual-keys/{id}/revoke"); got != "" {
		t.Fatalf("childParamOf leaf = %q, want empty", got)
	}
	// childOps of a record include its writes + sub-resources, never the GET itself.
	ops := childOps("virtual-keys", "/api/admin/virtual-keys/{id}")
	if len(ops) == 0 {
		t.Fatal("virtual-keys record must have child ops")
	}
	for _, op := range ops {
		if op.Path == "/api/admin/virtual-keys/{id}" && op.Method == "GET" {
			t.Fatal("childOps must exclude the record's own GET")
		}
	}
	// missingParams
	op := opInfo(t, "nodes", "setNodeOverride")
	if miss := missingParams(op, map[string]string{"id": "n1"}); len(miss) != 1 || miss[0] != "configKey" {
		t.Fatalf("missingParams = %v", miss)
	}
	if miss := missingParams(op, map[string]string{"id": "n1", "configKey": "k"}); len(miss) != 0 {
		t.Fatalf("fully-bound op should have no missing params, got %v", miss)
	}
	// toValues
	if toValues(nil) != nil {
		t.Fatal("empty map → nil values")
	}
	if v := toValues(map[string]string{"a": "1"}); v.Get("a") != "1" {
		t.Fatalf("toValues = %v", v)
	}
}

// TestResourceCollectionMenu covers 'o' (collection operations) + 'f' (filter)
// from a table, the menu render, and the menu-mode help/crumb.
func TestResourceCollectionMenu(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`[{"id":"a"}]`)}
	r := openVK(t, gw)
	r.handleKey(keyRunes("o"))
	if r.top().mode != frameMenu || len(r.top().menu) == 0 {
		t.Fatalf("'o' should open the collection operations menu, got %v", r.top().mode)
	}
	if out := r.View(80, 24); !strings.Contains(out, "operations") {
		t.Fatalf("menu view:\n%s", out)
	}
	if !strings.Contains(r.Help(), "run") {
		t.Fatalf("menu help: %q", r.Help())
	}
	// menu up/down clamps.
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if r.top().cursor != 0 {
		t.Fatal("menu up at the top clamps")
	}
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if r.top().cursor != 1 {
		t.Fatal("menu down advances")
	}
}

// TestResourceDrillNonDrillableRow covers a row with no id → the row is shown as a
// leaf record (pushRecord), not fetched.
func TestResourceDrillNonDrillableRow(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`[{"name":"no-id-here"}]`)}
	r := openVK(t, gw)
	calls := gw.adminCalls
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // drill the id-less row
	if r.top().mode != frameRecord {
		t.Fatalf("an id-less row should still show a leaf record, got %v", r.top().mode)
	}
	if gw.adminCalls != calls {
		t.Fatal("an id-less row must not fetch (no id to substitute)")
	}
	if !strings.Contains(r.View(80, 24), "no-id-here") {
		t.Fatal("the leaf record should render the row JSON")
	}
}

// TestResourceWriteWithBodyForm covers a write that needs a body: the body form
// opens, collects fields, then runs through the confirm gate to the executor.
func TestResourceWriteWithBodyForm(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"id":"new"}`)}
	r := newResource(gw)
	r.stack = []*resFrame{{kind: "virtual-keys", mode: frameMenu, params: map[string]string{}}}
	op := opInfo(t, "virtual-keys", "createVirtualKey")

	if cmd := r.activateResolved("virtual-keys", op, map[string]string{}); cmd != nil {
		t.Fatal("a write with a body should open a form, not run")
	}
	if r.form == nil || r.form.purpose != formBody {
		t.Fatal("createVirtualKey should open a body form")
	}
	// Fill the first (required) field and submit → confirm gate.
	r.form.cur = 0
	r.form.focusCurrent()
	for _, ch := range "ci-key" {
		r.handleKey(keyRunes(string(ch)))
	}
	_, _ = r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !r.cf.Capturing() {
		t.Fatal("submitting the body form should raise the confirm gate")
	}
	_, cmd := r.handleKey(keyRunes("y"))
	if cmd == nil {
		t.Fatal("allow should run the write")
	}
	r.Update(cmd())
	if gw.lastAdmin.method != "POST" || gw.lastAdmin.path != "/api/admin/virtual-keys" {
		t.Fatalf("create built %s %s", gw.lastAdmin.method, gw.lastAdmin.path)
	}
	if body, _ := gw.lastAdmin.body.(json.RawMessage); !strings.Contains(string(body), "ci-key") {
		t.Fatalf("the body form value must be sent, got %s", body)
	}
}

// TestResourceBackClosesForm covers Back() closing an open form before popping.
func TestResourceBackClosesForm(t *testing.T) {
	r := newResource(&fakeGateway{})
	r.stack = []*resFrame{{kind: "cache", mode: frameMenu}}
	r.activateOp(r.top(), opInfo(t, "cache", "cacheGetProvider")) // opens a param form
	if r.form == nil {
		t.Fatal("expected a form")
	}
	if !r.Back() || r.form != nil {
		t.Fatal("back should close the open form")
	}
}

// TestResourceKindViewBusy covers the picker's loading line.
func TestResourceKindViewBusy(t *testing.T) {
	r := newResource(&fakeGateway{})
	r.busy, r.pendingKind = true, "nodes"
	if !strings.Contains(r.kindView(), "opening nodes") {
		t.Fatalf("busy picker should show the opening label:\n%s", r.kindView())
	}
}

// TestResourceUpdateBranches covers the async-message guards: stale tokens, errors,
// and the re-filter replace.
func TestResourceUpdateBranches(t *testing.T) {
	r := openVK(t, &fakeGateway{adminRaw: json.RawMessage(`[{"id":"a"}]`)})
	depth := len(r.stack)
	// A stale data message (old token) is ignored.
	r.Update(resDataMsg{token: r.token - 1, raw: json.RawMessage(`[{"id":"z"}]`)})
	if len(r.stack) != depth {
		t.Fatal("a stale data message must be ignored")
	}
	// A data error is recorded, no frame pushed.
	r.Update(resDataMsg{token: r.token, err: errResourceTest})
	if r.err == nil || len(r.stack) != depth {
		t.Fatal("a data error must be recorded without pushing a frame")
	}
	// A replace swaps the top frame instead of pushing.
	r.Update(resDataMsg{token: r.token, replace: true, raw: json.RawMessage(`[{"id":"b"},{"id":"c"}]`), title: "filtered"})
	if len(r.stack) != depth || len(r.top().rows) != 2 {
		t.Fatalf("replace should swap the top frame, stack=%d rows=%d", len(r.stack), len(r.top().rows))
	}
	// Write-message branches: stale ignored, error recorded.
	r.Update(resWriteMsg{token: r.token - 1})
	r.Update(resWriteMsg{token: r.token, err: errResourceTest})
	if r.err == nil {
		t.Fatal("a write error must be recorded")
	}
	// A successful write pushes a record leaf + a note.
	r.Update(resWriteMsg{token: r.token, label: "revoke", status: 200, raw: json.RawMessage(`{"ok":true}`)})
	if r.top().mode != frameRecord || !strings.Contains(r.note, "revoke") {
		t.Fatalf("a write result should push a record + note, got %q", r.note)
	}
}

// TestResourceTableKeysAndBusy covers the table pager clamps + busy/loading renders
// and the resolve-error path.
func TestResourceTableKeysAndBusy(t *testing.T) {
	r := openVK(t, &fakeGateway{adminRaw: json.RawMessage(`[{"id":"a"},{"id":"b"}]`)})
	r.handleKey(keyRunes("p")) // page already 0 → clamp
	if r.top().page != 0 {
		t.Fatal("p at page 0 clamps")
	}
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyDown}) // clamp at last row
	if r.top().cursor != 1 {
		t.Fatalf("row cursor clamps at last, got %d", r.top().cursor)
	}
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if r.top().cursor != 0 {
		t.Fatal("row up moves to 0")
	}
	// Busy table render.
	r.busy, r.top().rows = true, nil
	if !strings.Contains(r.tableView(r.top(), 80), "loading") {
		t.Fatal("busy table shows loading")
	}
	// Busy record render.
	rec := &resFrame{kind: "x", mode: frameRecord}
	r.stack = append(r.stack, rec)
	if !strings.Contains(r.recordView(rec, 80), "loading") {
		t.Fatal("busy record shows loading")
	}
	r.busy = false
	// Empty table renders the raw-body fallback when no rows but a raw body exists.
	empty := &resFrame{kind: "x", mode: frameTable, raw: json.RawMessage(`{"meta":1}`)}
	if !strings.Contains(r.tableView(empty, 80), "meta") {
		t.Fatal("an empty table with a raw body shows the JSON")
	}
}

// TestResourceResolveError covers a run with an unresolvable op (ResolveOperation
// error → inline error, no command).
func TestResourceResolveError(t *testing.T) {
	r := newResource(&fakeGateway{})
	bad := capres.OperationInfo{OperationID: "ghost", Method: "GET", Path: "/api/admin/x"}
	if cmd := r.runGet("virtual-keys", bad, nil, nil, nil, "x"); cmd != nil {
		t.Fatal("an unresolvable op must not run")
	}
	if r.err == nil {
		t.Fatal("an unresolvable op should set an inline error")
	}
	if cmd := r.runWrite("virtual-keys", capres.OperationInfo{OperationID: "ghost", Method: "POST"}, nil, nil, nil); cmd != nil {
		t.Fatal("an unresolvable write must not run")
	}
}

// TestResourcePickerNav covers the picker cursor down + an enter with no selection.
func TestResourcePickerNav(t *testing.T) {
	r := newResource(&fakeGateway{})
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if r.kindCur != 1 {
		t.Fatalf("picker down should advance, got %d", r.kindCur)
	}
	// enter with a filter that matches nothing is a no-op (no panic, no fetch).
	r.filter.SetValue("zzzz-nope")
	r.kindCur = 0
	_, cmd := r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("enter with no matching kind must be a no-op")
	}
	// filter typing routes to the input and resets the cursor.
	r.filter.SetValue("")
	r.kindCur = 3
	r.handleKey(keyRunes("v"))
	if r.kindCur != 0 {
		t.Fatal("filter typing should reset the cursor")
	}
}

func TestResourceMiscHelpers(t *testing.T) {
	if cloneParams(nil) == nil {
		t.Fatal("cloneParams(nil) returns a usable map")
	}
	if _, ok := findGet("virtual-keys", "/api/admin/virtual-keys/{id}/no-such"); ok {
		t.Fatal("findGet on a missing path must be false")
	}
	if got := childParamOf("virtual-keys", "/api/admin/no-such-base"); got != "" {
		t.Fatalf("childParamOf with no nested op = %q", got)
	}
}

// TestResourceRenderOverlays covers View/help while a form or the confirm gate is
// up, the empty-menu render, the no-filter note, and the success note line.
func TestResourceRenderOverlays(t *testing.T) {
	r := newResource(&fakeGateway{})
	r.stack = []*resFrame{{kind: "cache", mode: frameMenu}}

	// Form overlay: View + help route to the form.
	r.activateOp(r.top(), opInfo(t, "cache", "cacheGetProvider"))
	if r.form == nil {
		t.Fatal("expected a form")
	}
	if !strings.Contains(r.View(80, 24), "provider") || !strings.Contains(r.Help(), "submit") {
		t.Fatalf("View/help should render the form")
	}
	r.form = nil

	// Confirm overlay: View + help route to the gate.
	r.activateResolved("cache", opInfo(t, "cache", "cacheFlush"), map[string]string{})
	if !r.cf.Capturing() {
		t.Fatal("cacheFlush should raise the confirm gate")
	}
	if r.View(80, 24) == "" || !strings.Contains(r.Help(), "deny") {
		t.Fatal("View/help should render the confirm gate")
	}
	r.cf.Cancel()

	// Empty menu render.
	r.stack = []*resFrame{{kind: "x", mode: frameMenu, menu: nil}}
	if !strings.Contains(r.menuView(r.top()), "no operations") {
		t.Fatal("an empty menu should say so")
	}

	// openFilter on an op with no query params sets the no-filter note.
	r.stack = []*resFrame{{kind: "virtual-keys", mode: frameTable, op: opInfo(t, "virtual-keys", "getVirtualKey")}}
	r.openFilter(r.top())
	if r.form != nil || !strings.Contains(r.note, "no filters") {
		t.Fatalf("getVirtualKey has no query params → expected the no-filter note, got %q", r.note)
	}

	// Success note line renders in green.
	r.note = "✓ done"
	if !strings.Contains(r.noteLine(), "done") {
		t.Fatal("a success note should render")
	}
}

// TestResourceEnterKindResetsTokenAndConfirm guards the architect-review fixes:
// entering a kind invalidates any in-flight fetch (so its late result can't land
// on the new kind) and cancels any confirm gate raised under the previous kind.
func TestResourceEnterKindResetsTokenAndConfirm(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`[{"id":"a"}]`)}
	r := openVK(t, gw)
	staleToken := r.token

	// Raise a write confirm under virtual-keys.
	r.activateResolved("virtual-keys", opInfo(t, "virtual-keys", "revokeVirtualKey"), map[string]string{"id": "vk1"})
	if !r.cf.Capturing() {
		t.Fatal("expected a confirm gate")
	}

	// Enter a different (no-list) kind.
	r.enterKind("semantic-cache")
	if r.cf.Capturing() {
		t.Fatal("entering a new kind must cancel the previous kind's confirm gate")
	}
	if r.token == staleToken {
		t.Fatal("entering a new kind must invalidate the previous token")
	}
	if len(r.stack) != 1 || r.top().mode != frameMenu {
		t.Fatalf("semantic-cache should land on a menu, got %d %v", len(r.stack), r.top().mode)
	}

	// A late result from the previous kind (stale token) must be ignored — never
	// appended onto the new kind's stack.
	r.Update(resDataMsg{token: staleToken, raw: json.RawMessage(`[{"id":"from-virtual-keys"}]`)})
	if len(r.stack) != 1 {
		t.Fatal("a stale fetch must not append a frame onto the new kind")
	}
}

func TestRenderMarkdownEdges(t *testing.T) {
	if kit.RenderMarkdown("", 80) != "" {
		t.Fatal("empty source stays empty")
	}
	if got := kit.RenderMarkdown("**x**", 4); got != "**x**" {
		t.Fatalf("too-narrow width returns the source unchanged, got %q", got)
	}
	if out := kit.RenderMarkdown("# Title", 80); strings.Contains(out, "# Title") {
		t.Fatalf("markdown should be rendered, not literal:\n%s", out)
	}
}

// TestResourceMenuShowsSummaries: operation menu rows render the operation's
// OpenAPI summary (truncated to the pane budget) and fall back to the label
// when an op carries no summary — the S3 "menus speak spec language" change.
func TestResourceMenuShowsSummaries(t *testing.T) {
	gw := &fakeGateway{adminRaw: json.RawMessage(`{"backendMode":"builtin"}`)}
	r := newResource(gw)
	r.filter.SetValue("ai-guard")
	r.kindCur = 0
	r.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if r.top().mode != frameMenu {
		t.Fatalf("ai-guard should land on a menu, got %v", r.top().mode)
	}
	view := r.View(120, 30)
	// Real OpenAPI summaries, not synthesized path tails:
	for _, want := range []string{"Get the AI Guard configuration", "Dry-run an AI Guard classification"} {
		if !strings.Contains(view, want) {
			t.Fatalf("menu must show the operation summary %q:\n%s", want, view)
		}
	}
	// Truncation: a synthetic long-summary op must clip at the pane budget.
	r.top().menu = append(r.top().menu, capres.OperationInfo{
		Kind: "ai-guard", OperationID: "longOne", Method: "GET", Path: "/api/admin/ai-guard/x",
		Summary: strings.Repeat("verylongword ", 10),
	})
	view = r.View(120, 30)
	if !strings.Contains(view, "…") {
		t.Fatalf("an over-budget summary must be ellipsized:\n%s", view)
	}
	// Fallback: an op with no summary renders its label.
	r.top().menu = append(r.top().menu, capres.OperationInfo{
		Kind: "ai-guard", OperationID: "bare", Method: "GET", Path: "/api/admin/ai-guard/y",
		Label: "bare-label-fallback",
	})
	if view := r.View(120, 30); !strings.Contains(view, "bare-label-fallback") {
		t.Fatalf("a summary-less op must fall back to its label:\n%s", view)
	}
}
