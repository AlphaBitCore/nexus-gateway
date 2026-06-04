// Package echo is a minimal stand-in for labstack/echo used by the openapigen
// fixture. It mirrors the method set the generator keys on (Group.GET/POST/...,
// Group.Group, Context.Bind/JSON) so the route walker exercises real type
// resolution without depending on the full echo module.
package echo

// Context is the per-request handler context.
type Context interface {
	Bind(any) error
	JSON(code int, v any) error
	NoContent(code int) error
	Param(name string) string
	QueryParam(name string) string
}

// HandlerFunc handles a request.
type HandlerFunc func(Context) error

// MiddlewareFunc wraps a handler.
type MiddlewareFunc func(HandlerFunc) HandlerFunc

// Group is a route group with an implicit path prefix.
type Group struct{}

func (g *Group) Group(prefix string, m ...MiddlewareFunc) *Group        { return &Group{} }
func (g *Group) GET(path string, h HandlerFunc, m ...MiddlewareFunc)    {}
func (g *Group) POST(path string, h HandlerFunc, m ...MiddlewareFunc)   {}
func (g *Group) PUT(path string, h HandlerFunc, m ...MiddlewareFunc)    {}
func (g *Group) PATCH(path string, h HandlerFunc, m ...MiddlewareFunc)  {}
func (g *Group) DELETE(path string, h HandlerFunc, m ...MiddlewareFunc) {}

// Echo is the root router.
type Echo struct{}

func (e *Echo) Group(prefix string, m ...MiddlewareFunc) *Group      { return &Group{} }
func (e *Echo) POST(path string, h HandlerFunc, m ...MiddlewareFunc) {}
func (e *Echo) GET(path string, h HandlerFunc, m ...MiddlewareFunc)  {}
