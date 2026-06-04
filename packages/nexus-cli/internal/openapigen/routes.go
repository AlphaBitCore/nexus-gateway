package openapigen

import (
	"go/ast"
	"go/types"
	"strings"
)

// httpVerbs are the Echo router methods the walker treats as route registrations.
var httpVerbs = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// discoverRoutes walks the registrar tree rooted at opts.RootFuncs and returns
// every HTTP route it can resolve. Routes it cannot fully resolve (non-literal
// paths, unresolved handlers) are recorded on rep.
func (l *loaded) discoverRoutes(opts Options, rep *Report) []route {
	var routes []route
	visited := map[*types.Func]bool{}
	for fn, fd := range l.funcDecl {
		if !contains(opts.RootFuncs, fn.Name()) {
			continue
		}
		env := map[*types.Var]string{}
		l.bindGroupParams(fd, opts.BasePrefix, env)
		l.analyzeFunc(fn, fd, env, visited, &routes, rep)
	}
	return routes
}

// bindGroupParams binds every *echo.Group parameter of fd to prefix in env,
// resolving each parameter identifier to its *types.Var via the owning
// package's type information.
func (l *loaded) bindGroupParams(fd *ast.FuncDecl, prefix string, env map[*types.Var]string) {
	if fd.Type.Params == nil {
		return
	}
	info := l.infoFor(fd.Pos())
	if info == nil {
		return
	}
	for _, field := range fd.Type.Params.List {
		if !isEchoGroupExpr(field.Type) {
			continue
		}
		for _, name := range field.Names {
			if v, ok := info.Defs[name].(*types.Var); ok {
				env[v] = prefix
			}
		}
	}
}

// analyzeFunc walks one registrar function body, emitting routes for verb calls
// on known group variables and recursing into callees that receive a group.
func (l *loaded) analyzeFunc(fn *types.Func, fd *ast.FuncDecl, env map[*types.Var]string, visited map[*types.Func]bool, routes *[]route, rep *Report) {
	if fn != nil {
		if visited[fn] {
			return
		}
		visited[fn] = true
	}
	if fd.Body == nil {
		return
	}
	info := l.infoFor(fd.Pos())
	if info == nil {
		return
	}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			l.bindGroupAssign(node, info, env)
		case *ast.CallExpr:
			l.handleCall(node, info, env, visited, routes, rep)
		}
		return true
	})
}

// bindGroupAssign records `sub := <group>.Group("/p")` bindings so routes
// registered on sub carry the extended prefix.
func (l *loaded) bindGroupAssign(as *ast.AssignStmt, info *types.Info, env map[*types.Var]string) {
	if len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return
	}
	prefix, ok := l.groupPrefix(as.Rhs[0], info, env)
	if !ok {
		return
	}
	lhs, ok := as.Lhs[0].(*ast.Ident)
	if !ok {
		return
	}
	if v, isVar := info.Defs[lhs].(*types.Var); isVar {
		env[v] = prefix
	} else if v, isVar := info.Uses[lhs].(*types.Var); isVar {
		env[v] = prefix
	}
}

// handleCall dispatches a call expression: a verb method on a group emits a
// route; a call passing a group to another registrar recurses into it.
func (l *loaded) handleCall(call *ast.CallExpr, info *types.Info, env map[*types.Var]string, visited map[*types.Func]bool, routes *[]route, rep *Report) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		l.maybeRecurseRegistrar(call, info, env, visited, routes, rep)
		return
	}
	if httpVerbs[sel.Sel.Name] {
		if prefix, ok := l.groupPrefix(sel.X, info, env); ok {
			l.emitRoute(sel.Sel.Name, prefix, call, info, routes, rep)
			return
		}
	}
	l.maybeRecurseRegistrar(call, info, env, visited, routes, rep)
}

// maybeRecurseRegistrar follows a call that hands a known group to another
// function (e.g. handler.RegisterRoutes(g, iamMW)), binding the callee's group
// parameter to the same prefix.
func (l *loaded) maybeRecurseRegistrar(call *ast.CallExpr, info *types.Info, env map[*types.Var]string, visited map[*types.Func]bool, routes *[]route, rep *Report) {
	prefix := ""
	passes := false
	for _, arg := range call.Args {
		if p, ok := l.groupPrefix(arg, info, env); ok {
			prefix, passes = p, true
			break
		}
	}
	if !passes {
		return
	}
	fn := calleeFunc(call, info)
	if fn == nil {
		return
	}
	fd, ok := l.funcDecl[fn]
	if !ok {
		return // callee outside loaded source (e.g. echo internals)
	}
	childEnv := map[*types.Var]string{}
	l.bindGroupParams(fd, prefix, childEnv)
	l.analyzeFunc(fn, fd, childEnv, visited, routes, rep)
}

// emitRoute builds a route from a verb call: path literal, handler, IAM action.
func (l *loaded) emitRoute(method, prefix string, call *ast.CallExpr, info *types.Info, routes *[]route, rep *Report) {
	if len(call.Args) < 2 {
		rep.addUnresolved("%s route on prefix %q has too few args", method, prefix)
		return
	}
	lit, ok := stringLit(call.Args[0])
	if !ok {
		rep.addUnresolved("%s route on prefix %q has non-literal path", method, prefix)
		return
	}
	path := joinPath(prefix, lit)

	r := route{Method: method, Path: path}
	r.handlerName, r.Request, r.Responses, r.QueryParams = l.analyzeHandler(call.Args[1], info, rep, path)
	r.IAMAction = l.iamAction(call.Args[2:])
	r.Tier = deriveTier(method, r.IAMAction)
	*routes = append(*routes, r)
}

// iamAction renders the IAM action expression from the middleware arguments.
// It unwraps the common `iamMW(<action-expr>)` / `iamMWDevice(<action-expr>,…)`
// wrapper so the action expression itself is captured, not the wrapper call.
func (l *loaded) iamAction(mwArgs []ast.Expr) string {
	for _, arg := range mwArgs {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if len(inner.Args) >= 1 {
			return l.render(inner.Args[0])
		}
	}
	return ""
}

// deriveTier classifies a route: read-shaped IAM verbs (or GET) are auto, every
// mutating verb is confirm.
func deriveTier(method, iamAction string) Tier {
	switch {
	case strings.Contains(iamAction, "VerbRead"), strings.Contains(iamAction, "VerbList"):
		return tierAuto
	case strings.Contains(iamAction, "VerbCreate"), strings.Contains(iamAction, "VerbUpdate"),
		strings.Contains(iamAction, "VerbDelete"):
		return tierConfirm
	}
	if method == "GET" {
		return tierAuto
	}
	return tierConfirm
}

// groupPrefix resolves expr to a group prefix if it denotes an Echo group: a
// bound variable, or a `<group>.Group("/p")` chain.
func (l *loaded) groupPrefix(expr ast.Expr, info *types.Info, env map[*types.Var]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		// A group receiver/argument is always a *use* of a bound variable.
		if v, ok := info.Uses[e].(*types.Var); ok {
			if p, bound := env[v]; bound {
				return p, true
			}
		}
		return "", false
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Group" {
			return "", false
		}
		base, ok := l.groupPrefix(sel.X, info, env)
		if !ok {
			return "", false
		}
		if len(e.Args) >= 1 {
			if lit, ok := stringLit(e.Args[0]); ok {
				return joinPath(base, lit), true
			}
		}
		return base, true
	}
	return "", false
}

// calleeFunc resolves the *types.Func a call invokes (method or package func).
func calleeFunc(call *ast.CallExpr, info *types.Info) *types.Func {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fn, ok := info.Uses[fun.Sel].(*types.Func); ok {
			return fn
		}
	case *ast.Ident:
		if fn, ok := info.Uses[fun].(*types.Func); ok {
			return fn
		}
	}
	return nil
}

// isEchoGroupExpr reports whether an AST type expression denotes *echo.Group or
// *echo.Echo (matched by type name to tolerate the test shim's import path).
func isEchoGroupExpr(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Group" || sel.Sel.Name == "Echo"
}
