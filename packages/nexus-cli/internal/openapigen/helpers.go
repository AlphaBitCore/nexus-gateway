package openapigen

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// contains reports whether s is present in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// stringLit extracts the Go string literal value of expr, if it is one.
func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// joinPath concatenates a group prefix and a route literal into a single
// slash-normalised path. Empty segments collapse so joinPath("/api/admin", "")
// yields "/api/admin" and joinPath("/api/admin", "/x") yields "/api/admin/x".
func joinPath(prefix, lit string) string {
	p := strings.TrimRight(prefix, "/")
	l := lit
	if l != "" && !strings.HasPrefix(l, "/") {
		l = "/" + l
	}
	joined := p + l
	if joined == "" {
		return "/"
	}
	return joined
}

// echoToOpenAPIPath converts Echo's `:param` path syntax to OpenAPI's
// `{param}` syntax and returns the parameter names in order.
func echoToOpenAPIPath(path string) (string, []string) {
	segs := strings.Split(path, "/")
	var params []string
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			name := s[1:]
			params = append(params, name)
			segs[i] = "{" + name + "}"
		} else if strings.HasPrefix(s, "*") {
			name := strings.TrimPrefix(s, "*")
			if name == "" {
				name = "wildcard"
			}
			params = append(params, name)
			segs[i] = "{" + name + "}"
		}
	}
	return strings.Join(segs, "/"), params
}
