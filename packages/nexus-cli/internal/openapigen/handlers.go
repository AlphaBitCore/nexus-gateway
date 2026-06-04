package openapigen

import (
	"go/ast"
	"go/constant"
	"go/types"
)

// analyzeHandler resolves a route's handler expression to its function body and
// extracts the request body type (the argument to c.Bind) and the response
// bodies (the arguments to c.JSON, keyed by status code). path is used only to
// name a func-literal handler and to annotate unresolved-handler reports.
func (l *loaded) analyzeHandler(handlerExpr ast.Expr, info *types.Info, rep *Report, path string) (string, types.Type, []response, []string) {
	name, body := l.resolveHandlerBody(handlerExpr, info)
	if body == nil {
		rep.addUnresolved("handler for %s could not be resolved to a body", path)
		return name, nil, nil, nil
	}
	bodyInfo := l.infoFor(body.Pos())
	if bodyInfo == nil {
		bodyInfo = info
	}

	var req types.Type
	var resps []response
	var queryParams []string
	seenStatus := map[int]bool{}
	seenQuery := map[string]bool{}

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Bind":
			if req == nil && len(call.Args) == 1 {
				if t := bindTargetType(call.Args[0], bodyInfo); t != nil {
					req = t
				}
			}
		case "JSON", "JSONPretty":
			if len(call.Args) >= 2 {
				addResponse(&resps, seenStatus, constStatus(call.Args[0], bodyInfo), bodyInfo.TypeOf(call.Args[1]))
			}
		case "NoContent":
			// c.NoContent(code) — an empty-body response at the given status.
			if len(call.Args) >= 1 {
				addResponse(&resps, seenStatus, constStatus(call.Args[0], bodyInfo), nil)
			}
		case "QueryParam":
			if len(call.Args) == 1 {
				if q, ok := stringLit(call.Args[0]); ok && !seenQuery[q] {
					seenQuery[q] = true
					queryParams = append(queryParams, q)
				}
			}
		}
		return true
	})
	return name, req, resps, queryParams
}

// addResponse records a (status, body type) pair, keeping the first body seen
// for a given status.
func addResponse(resps *[]response, seen map[int]bool, status int, t types.Type) {
	if seen[status] {
		return
	}
	seen[status] = true
	*resps = append(*resps, response{Status: status, Type: t})
}

// resolveHandlerBody maps a handler expression (method value, package function,
// or func literal) to its name and body block.
func (l *loaded) resolveHandlerBody(expr ast.Expr, info *types.Info) (string, *ast.BlockStmt) {
	switch h := expr.(type) {
	case *ast.SelectorExpr:
		if fn, ok := info.Uses[h.Sel].(*types.Func); ok {
			if fd, ok := l.funcDecl[fn]; ok {
				return fn.Name(), fd.Body
			}
			return fn.Name(), nil
		}
	case *ast.Ident:
		if fn, ok := info.Uses[h].(*types.Func); ok {
			if fd, ok := l.funcDecl[fn]; ok {
				return fn.Name(), fd.Body
			}
			return fn.Name(), nil
		}
	case *ast.FuncLit:
		return "", h.Body
	}
	return "", nil
}

// bindTargetType returns the (dereferenced) type passed to c.Bind, i.e. the
// element type of the &req argument.
func bindTargetType(arg ast.Expr, info *types.Info) types.Type {
	unary, ok := arg.(*ast.UnaryExpr)
	if !ok {
		// c.Bind sometimes receives an already-pointer expression.
		t := info.TypeOf(arg)
		if p, ok := t.(*types.Pointer); ok {
			return p.Elem()
		}
		return t
	}
	return info.TypeOf(unary.X)
}

// constStatus evaluates a status-code expression (http.StatusOK, an int literal,
// or any constant) to its integer value, or 0 if it is not a constant.
func constStatus(expr ast.Expr, info *types.Info) int {
	tv, ok := info.Types[expr]
	if !ok || tv.Value == nil {
		return 0
	}
	if i, ok := constant.Int64Val(tv.Value); ok {
		return int(i)
	}
	return 0
}
