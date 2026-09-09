package httperr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A guard that reports refusal by handing back `c.JSON(...)`'s result is a dead
// branch at every call site, because `c.JSON` returns nil when the write
// succeeds. The refusal goes out on the wire and the caller carries on.
//
// This has happened twice in this codebase:
//
//   - `assertScimGroup` (identity/scim/handler/scim.go) answered 403 and then
//     performed the rename / member replacement / delete anyway, because every
//     caller's `if errResp != nil` was unreachable.
//   - `parseGrantExpiry`, written in this same backport to FIX a different
//     defect, reproduced the shape from scratch. Its tests caught it; nothing
//     structural would have.
//
// What is flagged is the COMBINATION, not either half:
//
//   - writing a response and returning `error` is the normal idiom for a helper
//     like `internalServerError`, whose callers write `return h.internalServerError(c, …)`
//     and never inspect the value. That is correct and there are dozens of them.
//   - testing an `error` for nil is universal and correct.
//
// It is only a trap when a caller TESTS the result of a helper that writes the
// response itself. So the gate resolves the set of response-writing helpers
// tree-wide, then looks for a call site that compares one of their results
// against nil. Route handlers are excluded: their `error` goes to echo, not to a
// caller, and registration happens in a different file from the declaration, so
// the handler set is collected tree-wide too.
// reviewedBenignWriters are response-writing helpers whose tested result has
// been read call-by-call and found harmless. The gate guards the UNKNOWN; an
// entry here is a claim someone checked, and it must carry why.
//
// Empty on purpose. hubForward used to be the sole entry: it returned only
// `c.JSON(...)` results, so every `if err := h.hubForward(...); err != nil`
// was a dead branch for refusals and the callers read the outcome back off
// c.Response().Status instead. It now returns (ok bool, err error) — the
// helper answers the question rather than making callers infer it — so the
// exemption is gone rather than reworded.
var reviewedBenignWriters = map[string]bool{}

func TestNoGuardReportsRefusalThroughTheResponseWriter(t *testing.T) {
	roots := serviceInternalDirs(t)
	if len(roots) < 2 {
		t.Fatalf("resolved %d service trees, want at least control-plane and nexus-hub — the gate is not looking where it claims", len(roots))
	}

	files := goFilesUnder(t, roots)
	if len(files) < 200 {
		t.Fatalf("only %d files found across %v — the gate is not looking at the tree it claims to police", len(files), roots)
	}

	// Pass 1 — tree-wide: which names are route handlers, and which are
	// response-writing bare-error helpers.
	handlers := map[string]bool{}
	writers := map[string]bool{}
	parsed := make(map[string]*ast.File, len(files))
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		parsed[path] = f
		collectRegisteredHandlers(f, handlers)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ctxName, ok := echoContextParam(fn)
			if !ok || !returnsBareError(fn) {
				continue
			}
			if _, writes := returnsWrittenResponse(fset, fn, ctxName); writes {
				writers[fn.Name.Name] = true
			}
		}
	}
	// Fixed point over one more shape: a helper that RELAYS a known writer's
	// error is itself a response-writing helper, and returnsWrittenResponse
	// cannot see it because that check is purely syntactic — it looks for a
	// literal `return c.JSON(...)` in the body.
	//
	// This is not hypothetical. hubForward's thirteen tail-position callers were
	// moved behind a hubProxy wrapper whose whole body is
	// `_, err := h.hubForward(...); return err`. Every refusal still travels as
	// a written response with a nil error, so `if err := h.hubProxy(...)` is the
	// same dead branch the gate exists to catch — one indirection deeper, where
	// the first pass could not see it. Renaming a hazard is not removing it.
	for {
		grew := false
		for _, f := range parsed {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || writers[fn.Name.Name] {
					continue
				}
				if _, ok := echoContextParam(fn); !ok || !returnsBareError(fn) {
					continue
				}
				if relaysWriterError(fn, writers) {
					writers[fn.Name.Name] = true
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}

	for name := range handlers {
		delete(writers, name)
	}
	for name := range reviewedBenignWriters {
		if !writers[name] {
			t.Fatalf("%q is allowlisted but the scan no longer resolves it as a response-writing helper — either it was renamed or the resolution broke; a stale allowlist entry hides whatever took its place", name)
		}
		delete(writers, name)
	}
	// Non-vacuity: the shape this gate is about must actually exist in the tree,
	// or a clean run proves nothing about the scan.
	if len(writers) < 5 {
		t.Fatalf("resolved only %d response-writing helpers tree-wide (%v) — the gate found nothing to police, which means the resolution is broken, not that the tree is clean",
			len(writers), sortedKeys(writers))
	}

	// Pass 2 — call sites that TEST the ERROR result of one of those helpers.
	//
	// Two narrowings, both learned from false positives on the first draft:
	//
	//   - only the LAST result counts. `vk, resp := h.ownedVKOrDeny(c)` followed
	//     by `if vk == nil` is SAFE — it keys off the value, not the response —
	//     and that is the shape four live call sites use.
	//   - the test must be ADJACENT to the assignment (the `if`'s own init, or
	//     the statement right after it). A function-wide map cannot tell which
	//     `err` a later `if err != nil` refers to, because `err` is reassigned
	//     constantly; scoping to the idiom removes the ambiguity instead of
	//     guessing at it.
	var offenders []string
	for path, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			// `if x, e := f(); e != nil {`
			if ifs, ok := n.(*ast.IfStmt); ok && ifs.Init != nil {
				if name, w, ok := writerAssignErrName(ifs.Init, writers); ok && testsNil(ifs.Cond, name) {
					offenders = append(offenders, offense(fset, path, ifs.Cond.Pos(), w))
				}
			}
			// `x, e := f()` immediately followed by `if e != nil {`
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i := 0; i+1 < len(block.List); i++ {
				name, w, ok := writerAssignErrName(block.List[i], writers)
				if !ok {
					continue
				}
				ifs, ok := block.List[i+1].(*ast.IfStmt)
				if !ok || ifs.Init != nil {
					continue
				}
				if testsNil(ifs.Cond, name) {
					offenders = append(offenders, offense(fset, path, ifs.Cond.Pos(), w))
				}
			}
			return true
		})
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("guards that report refusal through the response writer (c.JSON returns nil on success, so the branch is dead):\n  %s\n\nReport refusal with a bool the caller must read.",
			strings.Join(offenders, "\n  "))
	}
}

func offense(fset *token.FileSet, path string, pos token.Pos, writer string) string {
	p := fset.Position(pos)
	return path + ":" + strconv.Itoa(p.Line) +
		" tests the error result of " + writer + "(), which writes the response itself"
}

// writerAssignErrName returns the name bound to the LAST result of an assignment
// whose right-hand side calls one of the response-writing helpers.
func writerAssignErrName(st ast.Stmt, writers map[string]bool) (string, string, bool) {
	as, ok := st.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
		return "", "", false
	}
	w, ok := calleeName(as.Rhs[0])
	if !ok || !writers[w] {
		return "", "", false
	}
	last, ok := as.Lhs[len(as.Lhs)-1].(*ast.Ident)
	if !ok || last.Name == "_" {
		return "", "", false
	}
	return last.Name, w, true
}

// testsNil reports whether cond is `name != nil` or `name == nil`.
func testsNil(cond ast.Expr, name string) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || (bin.Op != token.NEQ && bin.Op != token.EQL) {
		return false
	}
	id, ok := bin.X.(*ast.Ident)
	if !ok || id.Name != name {
		return false
	}
	nilIdent, ok := bin.Y.(*ast.Ident)
	return ok && nilIdent.Name == "nil"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// calleeName returns the bare name of a called function, whether it is called as
// `f(...)`, `h.f(...)` or `pkg.f(...)`.
func calleeName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, true
	case *ast.SelectorExpr:
		return fn.Sel.Name, true
	}
	return "", false
}

// collectRegisteredHandlers records every method named as the handler argument
// of an echo route registration — `g.POST("/x", h.Create, ...)` marks `Create`.
// Those legitimately return `c.JSON(...)`: echo is their caller.
func collectRegisteredHandlers(file *ast.File, out map[string]bool) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isEchoVerbName(sel.Sel.Name) {
			return true
		}
		switch h := call.Args[1].(type) {
		case *ast.SelectorExpr:
			out[h.Sel.Name] = true
		case *ast.Ident:
			out[h.Name] = true
		}
		return true
	})
}

func isEchoVerbName(name string) bool {
	switch name {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "Any", "Add":
		return true
	}
	return false
}

// echoContextParam returns the name of the function's echo.Context parameter.
func echoContextParam(fn *ast.FuncDecl) (string, bool) {
	if fn.Type.Params == nil {
		return "", false
	}
	for _, p := range fn.Type.Params.List {
		sel, ok := p.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Context" {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "echo" {
			continue
		}
		if len(p.Names) == 0 {
			return "", false // unnamed: the body cannot write through it
		}
		return p.Names[0].Name, true
	}
	return "", false
}

// returnsBareError reports whether `error` is the function's ONLY refusal
// channel. A helper that also returns a bool is the corrected shape.
func returnsBareError(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	var hasErr bool
	for _, r := range fn.Type.Results.List {
		id, ok := r.Type.(*ast.Ident)
		if !ok {
			continue
		}
		switch id.Name {
		case "error":
			hasErr = true
		case "bool":
			return false // refusal is reported out-of-band; not the trap
		}
	}
	return hasErr
}

// returnsWrittenResponse finds `return <ctx>.JSON(...)` (or another echo
// response writer) inside fn, and reports the line of the first one.
func returnsWrittenResponse(fset *token.FileSet, fn *ast.FuncDecl, ctxName string) (int, bool) {
	line, found := 0, false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			call, ok := res.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isResponseWriter(sel.Sel.Name) {
				continue
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != ctxName {
				continue
			}
			line, found = fset.Position(ret.Pos()).Line, true
			return false
		}
		return true
	})
	return line, found
}

func isResponseWriter(name string) bool {
	switch name {
	case "JSON", "JSONBlob", "String", "NoContent", "XML", "Blob", "HTML":
		return true
	}
	return false
}

func goFilesUnder(t *testing.T, roots []string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(out)
	return out
}

// serviceInternalDirs resolves the internal/ tree of every Go service that
// registers echo routes. Path-based, so it crosses module boundaries.
func serviceInternalDirs(t *testing.T) []string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var packagesDir string
	for range 12 {
		if filepath.Base(dir) == "packages" {
			packagesDir = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if packagesDir == "" {
		t.Fatal("could not locate the packages/ directory from the test's working directory")
	}
	var out []string
	for _, svc := range []string{"control-plane", "nexus-hub", "ai-gateway", "compliance-proxy"} {
		p := filepath.Join(packagesDir, svc, "internal")
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// relaysWriterError reports whether fn hands a known writer's refusal onward
// UNCHANGED — the shape that makes the writer's dead branch reachable through a
// new name.
//
// Two narrowings, both learned from false positives on the first draft, which
// claimed breadth was safe "because classification is not an accusation". It is
// not safe: a wrongly classified helper turns every `if err != nil` on it into
// a reported offence, and both shapes below were reported.
//
//   - The call's receiver must be fn's OWN receiver. Matching a bare method
//     name made `h.vks.GetVirtualKey` (a store call inside resolveVK) collide
//     with the handler `(*Handler).GetVirtualKey`, which does write. Different
//     symbols, same last identifier.
//   - The returned error must BE the writer's. doEnroll writes a response and
//     then returns `fmt.Errorf(...)` — a real error, from a real failure, so
//     `if err := doEnroll(...); err != nil` is a live branch, not a dead one.
//     A relay returns the writer's own value and nothing else.
func relaysWriterError(fn *ast.FuncDecl, writers map[string]bool) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return false
	}
	recvName := fn.Recv.List[0].Names[0].Name

	// isWriterCall: `<recv>.<knownWriter>(...)`.
	isWriterCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !writers[sel.Sel.Name] {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == recvName
	}

	// Names assigned SOLELY from a writer call, so `return err` can be traced.
	relayed := map[string]bool{}
	tainted := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		fromWriter := isWriterCall(as.Rhs[0])
		for _, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name == "_" {
				continue
			}
			if fromWriter {
				relayed[id.Name] = true
			} else {
				tainted[id.Name] = true
			}
		}
		return true
	})

	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			if isWriterCall(res) {
				found = true
				return false
			}
			if id, ok := res.(*ast.Ident); ok && relayed[id.Name] && !tainted[id.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// The gate's own red proof.
//
// Every other guard in this file measures the real tree, and a clean tree
// cannot show that the detector still fires — `len(writers) >= 5` proves it is
// LOOKING, not that it can go RED. This runs the same two passes over source
// that never touches the repository, so the assertion is about the rule rather
// than about today's code.
func TestGuardShapeGate_FiresOnASyntheticViolation(t *testing.T) {
	// Methods on a receiver, because that is the shape the detector requires
	// and the shape the tree actually has. A fixture of package-level funcs
	// would pass the offender count while proving nothing about the narrowing
	// that keeps a store call from colliding with a same-named handler.
	const src = `package p

import "github.com/labstack/echo/v4"

type H struct{ store S }

type S struct{}

func (s S) writeRefusal(c echo.Context) error { return nil }

func (h *H) writeRefusal(c echo.Context) error {
	return c.JSON(403, "nope")
}

func (h *H) relayRefusal(c echo.Context) error {
	err := h.writeRefusal(c)
	return err
}

// Must NOT be classified: the receiver is h.store, a different symbol that
// merely shares the method name.
func (h *H) throughTheStore(c echo.Context) error {
	return h.store.writeRefusal(c)
}

func (h *H) offender(c echo.Context) error {
	if err := h.relayRefusal(c); err != nil {
		return err
	}
	return nil
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	writers := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ctxName, ok := echoContextParam(fn)
		if !ok || !returnsBareError(fn) {
			continue
		}
		if _, writes := returnsWrittenResponse(fset, fn, ctxName); writes {
			writers[fn.Name.Name] = true
		}
	}
	if !writers["writeRefusal"] {
		t.Fatal("pass 1 did not resolve the direct writer; the detector is broken, not the fixture")
	}

	// Pass 1b — the relay must be picked up, or the offender below is invisible.
	for {
		grew := false
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || writers[fn.Name.Name] {
				continue
			}
			if _, ok := echoContextParam(fn); !ok || !returnsBareError(fn) {
				continue
			}
			if relaysWriterError(fn, writers) {
				writers[fn.Name.Name] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	if writers["throughTheStore"] {
		t.Error("a call through a DIFFERENT receiver that merely shares a method name was " +
			"classified as a relay — that collision reported four innocent call sites")
	}
	if !writers["relayRefusal"] {
		t.Fatal("a helper that relays a writer's error was not classified as a writer — " +
			"the hazard survives by moving one indirection deeper, which is how it survived before")
	}

	var offenders int
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil {
			return true
		}
		if name, _, ok := writerAssignErrName(ifs.Init, writers); ok && testsNil(ifs.Cond, name) {
			offenders++
		}
		return true
	})
	if offenders != 1 {
		t.Fatalf("the synthetic violation produced %d offender(s), want 1 — this gate cannot "+
			"be shown to go red, so a clean run over the real tree proves nothing", offenders)
	}
}
