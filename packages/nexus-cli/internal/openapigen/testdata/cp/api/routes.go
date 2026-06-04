// Package api is the fixture control-plane admin surface. Its shape mirrors the
// real RegisterAdminRoutes tree: a root registrar that delegates to a
// sub-registrar and also mounts a nested group, handlers that bind anonymous
// request structs and respond via c.JSON with http.Status* codes, and IAM
// middleware wrapping iam.Resource.Action(iam.Verb...) expressions.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"example.com/cp/echo"
	"example.com/cp/iam"
)

// Handler owns the fixture routes.
type Handler struct{}

// RegisterAdminRoutes is the generator's default root registrar.
func (h *Handler) RegisterAdminRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	h.registerWidgets(g, iamMW)

	// Nested group: routes here carry the "/nested" prefix on top of the base.
	sub := g.Group("/nested")
	sub.GET("/items", h.ListItems, iamMW(iam.ResourceWidget.Action(iam.VerbList)))

	// Root-level path: has no derivable resource kind and is reported.
	g.GET("/", h.Root, iamMW(iam.ResourceWidget.Action(iam.VerbRead)))

	// Package-level (non-method) registrar reached as an identifier call. It is
	// also invoked from registerWidgets; the second visit is short-circuited.
	registerExtras(g, iamMW)
}

// RegisterStandaloneRoutes is a SECOND top-level registrar wired outside
// RegisterAdminRoutes (mirroring the real assistant handler, which lives in the
// cmd wiring layer). It is reachable ONLY when named as an explicit walk root in
// Options.RootFuncs — never by recursion from RegisterAdminRoutes — so it
// exercises the multi-root walk. It takes a bare *echo.Group (no iamMW): a
// login-only surface with no IAM action.
func (h *Handler) RegisterStandaloneRoutes(g *echo.Group) {
	g.GET("/standalone/ping", h.GetWidget)
}

// registerExtras is a package-level registrar (called via an identifier, not a
// selector) registering package-function handlers — exercising the identifier
// branches of callee + handler resolution.
func registerExtras(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Non-group assignments the walker must ignore: a multi-assign (arity > 1)
	// and a single non-group assign.
	a, b := 1, 2
	_, _ = a, b
	n := computeCode()
	_ = n

	var passthru echo.MiddlewareFunc
	// Bare (non-call) middleware: no IAM action recoverable.
	g.GET("/extras", standaloneList, passthru)
	g.POST("/extras", standaloneCreate, iamMW(iam.ResourceWidget.Action(iam.VerbCreate)))
	// No-c.JSON handler: response set is empty, yielding a default response.
	g.GET("/extras/health", h2.Health, iamMW(iam.ResourceWidget.Action(iam.VerbRead)))
	// Call-expression handler: cannot be resolved to a body and is reported.
	g.GET("/extras/dyn", makeHandler(), iamMW(iam.ResourceWidget.Action(iam.VerbRead)))

	// Group bound via a plain assignment (not :=), exercising the Uses branch.
	var sub2 *echo.Group
	sub2 = g.Group("/extras-sub")
	sub2.GET("/leaf", standaloneList, iamMW(iam.ResourceWidget.Action(iam.VerbRead)))
}

// h2 backs the package-function handlers that need a receiver.
var h2 = &Handler{}

// Root has no c.JSON call (and no derivable kind).
func (h *Handler) Root(c echo.Context) error { return nil }

// Health resolves to a body with no c.JSON, exercising the default-response path.
func (h *Handler) Health(c echo.Context) error { _ = c; return nil }

// makeHandler returns a handler value; used as a non-identifier route argument.
func makeHandler() echo.HandlerFunc {
	return func(c echo.Context) error { return c.JSON(http.StatusOK, "ok") }
}

// Base is embedded into CreateExtra to exercise inline struct promotion,
// including an unexported field and a json:"-" field that must be dropped.
type Base struct {
	Common     string `json:"common"`
	internalID string
	Ignored    string `json:"-"`
}

// CreateExtra is a named request type embedding Base and carrying a
// time.Duration field (mapped to integer nanoseconds).
type CreateExtra struct {
	*Base
	Title   string        `json:"title"`
	Timeout time.Duration `json:"timeout"`
	hidden  int
}

// Extra is a small response model.
type Extra struct {
	ID string `json:"id"`
}

func standaloneList(c echo.Context) error {
	code := computeCode() // non-constant status expression
	return c.JSON(code, []string{})
}

func standaloneCreate(c echo.Context) error {
	body := &CreateExtra{}
	if err := c.Bind(body); err != nil { // already-pointer Bind argument
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "bad"})
	}
	_ = body
	return c.JSON(http.StatusCreated, Extra{})
}

func computeCode() int { return http.StatusOK }

// registerWidgets is a sub-registrar reached by passing the group along.
func (h *Handler) registerWidgets(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/widgets", h.ListWidgets, iamMW(iam.ResourceWidget.Action(iam.VerbList)))
	g.POST("/widgets", h.CreateWidget, iamMW(iam.ResourceWidget.Action(iam.VerbCreate)))
	g.GET("/widgets/:id", h.GetWidget, iamMW(iam.ResourceWidget.Action(iam.VerbRead)))
	g.PUT("/widgets/:id", h.UpdateWidget, iamMW(iam.ResourceWidget.Action(iam.VerbUpdate)))
	g.DELETE("/widgets/:id", h.DeleteWidget, iamMW(iam.ResourceWidget.Action(iam.VerbDelete)))

	// Reaching registerExtras a second time exercises the visited-guard.
	registerExtras(g, iamMW)

	// Func-literal handler: exercises operationId synthesis from the path.
	g.GET("/widgets/ping", func(c echo.Context) error { return c.JSON(http.StatusOK, "pong") },
		iamMW(iam.ResourceWidget.Action(iam.VerbRead)))

	// Non-literal path: the generator cannot resolve it and records it as
	// unresolved instead of emitting a bogus route.
	g.GET(dynamicPath(), h.ListWidgets, iamMW(iam.ResourceWidget.Action(iam.VerbList)))
}

func dynamicPath() string { return "/widgets/computed" }

// Widget is a response model exercising named-struct hoisting + time.Time.
type Widget struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListWidgets returns a wrapped collection filtered by query parameters.
func (h *Handler) ListWidgets(c echo.Context) error {
	_ = c.QueryParam("scope")
	_ = c.QueryParam("status")
	_ = c.QueryParam("scope") // duplicate read must not duplicate the parameter
	return c.JSON(http.StatusOK, map[string]any{"data": []Widget{}, "total": 0})
}

// CreateWidget binds an anonymous struct covering pointer/slice/map/RawMessage.
func (h *Handler) CreateWidget(c echo.Context) error {
	var body struct {
		Name        string            `json:"name"`
		Description *string           `json:"description"`
		Count       int               `json:"count"`
		Enabled     *bool             `json:"enabled,omitempty"`
		Tags        []string          `json:"tags"`
		Meta        json.RawMessage   `json:"meta"`
		Labels      map[string]string `json:"labels"`
		Secret      string            `json:"-"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "invalid request body"})
	}
	_ = body
	return c.JSON(http.StatusCreated, Widget{})
}

// GetWidget returns a single widget.
func (h *Handler) GetWidget(c echo.Context) error {
	return c.JSON(http.StatusOK, Widget{})
}

// UpdateWidget binds a small struct and returns the updated widget.
func (h *Handler) UpdateWidget(c echo.Context) error {
	var body struct {
		Name string `json:"name"`
	}
	_ = c.Bind(&body)
	return c.JSON(http.StatusOK, Widget{})
}

// DeleteWidget responds with no content (exercises c.NoContent extraction).
func (h *Handler) DeleteWidget(c echo.Context) error {
	return c.NoContent(http.StatusNoContent)
}

// ListItems is the nested-group handler.
func (h *Handler) ListItems(c echo.Context) error {
	return c.JSON(http.StatusOK, []Widget{})
}
