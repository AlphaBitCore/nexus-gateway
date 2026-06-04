package openapigen

import (
	"fmt"
	"go/types"
)

// Tier classifies a route by side effect, mirroring the nexus-cli agent's
// permission model: reads run automatically, writes need confirmation.
type Tier string

const (
	tierAuto    Tier = "auto"
	tierConfirm Tier = "confirm"
)

// response pairs an HTTP status code with the Go type the handler passes to
// c.JSON for that code. A zero Type means the body type could not be resolved.
type response struct {
	Status int
	Type   types.Type
}

// route is one discovered HTTP endpoint: the method + fully-qualified path, the
// IAM action expression rendered from source, the resolved tier, the request
// body type (nil if the handler does not bind one), and the response bodies
// collected from the handler's c.JSON calls.
type route struct {
	Method    string // GET POST PUT PATCH DELETE
	Path      string // e.g. /api/admin/quota-policies/:id
	IAMAction string // rendered expression, e.g. iam.ResourceQuotaPolicy.Action(iam.VerbRead)
	Tier      Tier
	Request   types.Type
	Responses []response
	// QueryParams are the query-string parameters the handler reads via
	// c.QueryParam, best-effort (literal argument names only).
	QueryParams []string
	// handlerName is the resolved handler identifier (for operationId + report).
	handlerName string
	// OperationID is the unique operationId assigned by assignOperationIDs (a
	// handler bound to two routes would otherwise collide). Set before emit.
	OperationID string
}

// Options configures a generation run.
type Options struct {
	// SrcDir is the directory go/packages loads from; the go.work workspace
	// resolves dependencies. Typically the control-plane package root.
	SrcDir string
	// Patterns are the package patterns to load (default {"./..."}).
	Patterns []string
	// BasePrefix is prepended to every route path registered on a root group
	// param (default "/api/admin").
	BasePrefix string
	// RootFuncs names the route-registrar functions to start the walk from
	// (default {"RegisterAdminRoutes", "RegisterAssistantRoutes"} — the assistant
	// is wired outside RegisterAdminRoutes so it needs its own walk root).
	RootFuncs []string
	// OutDir is the directory the per-kind OpenAPI files are written to.
	OutDir string
	// Version stamps info.version in every emitted document. Callers pass a
	// fixed value (the package never reads the wall clock).
	Version string
	// Title is the info.title prefix (default "Nexus Control Plane Admin API").
	Title string
	// Env overrides the environment passed to go/packages. Empty inherits the
	// process environment (so the go.work workspace is auto-detected). Tests set
	// GOWORK=off here to load a self-contained fixture module.
	Env []string
}

func (o *Options) withDefaults() {
	if len(o.Patterns) == 0 {
		o.Patterns = []string{"./..."}
	}
	if o.BasePrefix == "" {
		o.BasePrefix = "/api/admin"
	}
	if len(o.RootFuncs) == 0 {
		o.RootFuncs = []string{"RegisterAdminRoutes", "RegisterAssistantRoutes"}
	}
	if o.Version == "" {
		o.Version = "0.0.0"
	}
	if o.Title == "" {
		o.Title = "Nexus Control Plane Admin API"
	}
}

// Report summarises a generation run for the caller and for drift checks.
type Report struct {
	// Kinds is the sorted list of resource kinds that produced a file.
	Kinds []string
	// Routes is the total number of routes successfully emitted.
	Routes int
	// FilesWritten lists the absolute paths written (per-kind files + index).
	FilesWritten []string
	// Unresolved records routes or handler facts the generator could not fully
	// analyse. These are surfaced, never silently dropped.
	Unresolved []string
}

// addUnresolved appends a human-readable note about something the generator
// could not recover, so the caller and the audit skill can follow up.
func (r *Report) addUnresolved(format string, args ...any) {
	r.Unresolved = append(r.Unresolved, fmt.Sprintf(format, args...))
}
