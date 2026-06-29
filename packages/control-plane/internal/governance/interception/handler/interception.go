package interception

import (
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/interception/interceptionstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// Interception-domain + interception-path admin CRUD.
//
// Compliance-Proxy has consumed these two tables via
// store.ListEnabledInterceptionDomains + the corresponding agent Cat B
// loader since day one, but until now the only way to seed or mutate the
// rows was the DB seed script. This handler set exposes the full CRUD
// surface so admins can manage the interception catalog from the console.
//
// The domain_allowlist shadow key is a derived projection: CP's reducer
// already re-computes it from enabled interception_domain.host_pattern
// rows whenever it receives an interception_domains invalidation, so every
// write below fires exactly ONE invalidation for
// compliance-proxy:interception_domains.
//
// Agent also consumes interception_domains via its own Cat B pull. CP's
// invalidation already fans out via Hub, so no extra agent call is needed
// here — mirroring the kill-switch pattern (one push covers the cascade).

// allowed enum values per Prisma schema. Keeping these as small whitelists
// here means malformed payloads never reach the DB, so we emit a clear 400
// instead of a cryptic ::cast error from Postgres.
var (
	validHostMatchTypes     = map[string]struct{}{"EXACT": {}, "PREFIX": {}, "GLOB": {}, "REGEX": {}}
	validPathMatchTypes     = map[string]struct{}{"EXACT": {}, "PREFIX": {}, "GLOB": {}, "REGEX": {}}
	validPathActions        = map[string]struct{}{"PROCESS": {}, "PASSTHROUGH": {}, "BLOCK": {}}
	validDefaultPathActions = map[string]struct{}{"PROCESS": {}, "PASSTHROUGH": {}, "BLOCK": {}}
	validFailureActions     = map[string]struct{}{"FAIL_OPEN": {}, "FAIL_CLOSED": {}}
	validNetworkZones       = map[string]struct{}{"PUBLIC": {}, "INTERNAL": {}}
)

// validateMatchRegex returns "" unless matchType is REGEX and pattern fails to
// compile (Go RE2). An empty pattern or a non-REGEX match type is skipped. The
// data plane (compliance-proxy / agent) compiles host/path patterns lazily at
// matcher-build time and silently drops a rule whose regex won't compile, so an
// admin who typed a bad regex into a "PII intercept" rule would get a 201 and a
// rule that never matches. Compile-checking here turns that into an authoring-
// time 400. RE2 has no catastrophic-backtracking class, so this is a
// validity check, not a ReDoS guard.
func validateMatchRegex(label, matchType, pattern string) string {
	if matchType != "REGEX" || pattern == "" {
		return ""
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return label + " is not a valid regular expression: " + err.Error()
	}
	return ""
}

// validateEnum returns "" when ok or a human-readable 400 message when the
// value is present but not in the whitelist. Empty strings are treated as
// "field omitted" and skipped (the store fills DB defaults).
func validateEnum(label, value string, whitelist map[string]struct{}) string {
	if value == "" {
		return ""
	}
	if _, ok := whitelist[value]; !ok {
		return label + " is not a valid value"
	}
	return ""
}

// RegisterInterceptionDomainRoutes wires the admin CRUD for
// InterceptionDomain + InterceptionPath under /api/admin/interception-domains.
// Gates use the canonical admin:interception-domain.<verb> action so that
// security/compliance can manage the inspection allowlist without inheriting
// the broader admin:settings.update scope (which also covers SSO/SAML/SIEM).
// Paths inherit the parent domain's verb because paths are sub-resources.
func (h *Handler) RegisterInterceptionDomainRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/interception-domains", h.ListInterceptionDomains, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbRead)))
	g.POST("/interception-domains", h.CreateInterceptionDomain, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbCreate)))
	g.GET("/interception-domains/:id", h.GetInterceptionDomain, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbRead)))
	g.PUT("/interception-domains/:id", h.UpdateInterceptionDomain, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbUpdate)))
	g.DELETE("/interception-domains/:id", h.DeleteInterceptionDomain, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbDelete)))

	g.POST("/interception-domains/:id/paths", h.CreateInterceptionPath, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbUpdate)))
	g.PUT("/interception-domains/:id/paths/:pathId", h.UpdateInterceptionPath, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbUpdate)))
	g.DELETE("/interception-domains/:id/paths/:pathId", h.DeleteInterceptionPath, iamMW(iam.ResourceInterceptionDomain.Action(iam.VerbUpdate)))
}

// invalidateInterceptionDomains fans `interception_domains` invalidation
// to BOTH Thing types that consume the key (compliance-proxy + agent).
// Hub.UpdateConfig broadcasts to one ThingType per call, so we issue
// two calls. The earlier comment claimed "CP's invalidation already
// fans out via Hub" — that assumption was incorrect (the agent's
// local domain ruleset stays stale on every CP edit until restart
// without both calls; same class of bug as pii-hooks / rule-pack).
func (h *Handler) invalidateInterceptionDomains(c echo.Context) {
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "compliance-proxy", "interception_domains")
		h.hub.InvalidateConfig(c.Request().Context(), "agent", "interception_domains")
	}
	h.incrementConfigVersion(c.Request().Context())
}

// GET /interception-domains

func (h *Handler) ListInterceptionDomains(c echo.Context) error {
	pg := parsePagination(c)
	params := interceptionstore.InterceptionDomainListParams{
		Search: c.QueryParam("search"),
		Limit:  pg.Limit,
		Offset: pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	res, err := h.store.ListInterceptionDomains(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list interception domains", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": res.Domains, "total": res.Total})
}

// GET /interception-domains/:id

func (h *Handler) GetInterceptionDomain(c echo.Context) error {
	d, err := h.store.GetInterceptionDomain(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("get interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if d == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, d)
}

// interceptionDomainBody is the wire payload for create / update.
type interceptionDomainBody struct {
	Name              string             `json:"name"`
	Description       *string            `json:"description"`
	HostPattern       string             `json:"hostPattern"`
	HostMatchType     string             `json:"hostMatchType"`
	AdapterID         string             `json:"adapterId"`
	AdapterConfig     json.RawMessage    `json:"adapterConfig"`
	Enabled           *bool              `json:"enabled"`
	Priority          int                `json:"priority"`
	DefaultPathAction string             `json:"defaultPathAction"`
	OnAdapterError    string             `json:"onAdapterError"`
	NetworkZone       string             `json:"networkZone"`
	Source            string             `json:"source"`
	Paths             []interceptionPath `json:"paths"`
}

type interceptionPath struct {
	PathPattern []string `json:"pathPattern"`
	MatchType   string   `json:"matchType"`
	Action      string   `json:"action"`
	Priority    int      `json:"priority"`
	Description *string  `json:"description"`
	Enabled     *bool    `json:"enabled"`
}

// POST /interception-domains

func (h *Handler) CreateInterceptionDomain(c echo.Context) error {
	var body interceptionDomainBody
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" || body.HostPattern == "" || body.AdapterID == "" {
		return c.JSON(http.StatusBadRequest,
			errJSON("name, hostPattern, and adapterId are required", "validation_error", ""))
	}
	if msg := validateDomainEnums(body); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if msg := validateMatchRegex("hostPattern", body.HostMatchType, body.HostPattern); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	pathInputs, msg := buildPathInputs(body.Paths)
	if msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	actor := actorFromContext(c)
	var createdBy *string
	if actor.UserID != "" {
		id := actor.UserID
		createdBy = &id
	}

	in := interceptionstore.CreateInterceptionDomainInput{
		Name:              body.Name,
		Description:       body.Description,
		HostPattern:       body.HostPattern,
		HostMatchType:     body.HostMatchType,
		AdapterID:         body.AdapterID,
		AdapterConfig:     body.AdapterConfig,
		Enabled:           body.Enabled,
		Priority:          body.Priority,
		DefaultPathAction: body.DefaultPathAction,
		OnAdapterError:    body.OnAdapterError,
		NetworkZone:       body.NetworkZone,
		Source:            body.Source,
		CreatedBy:         createdBy,
		Paths:             pathInputs,
	}
	d, err := h.store.CreateInterceptionDomain(c.Request().Context(), in)
	if err != nil {
		h.logger.Error("create interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbCreate)
	ae.EntityID = d.ID
	ae.AfterState = d
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, d)
}

// PUT /interception-domains/:id

func (h *Handler) UpdateInterceptionDomain(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.store.GetInterceptionDomain(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
	}

	var body struct {
		Name              *string         `json:"name"`
		Description       *string         `json:"description"`
		HostPattern       *string         `json:"hostPattern"`
		HostMatchType     *string         `json:"hostMatchType"`
		AdapterID         *string         `json:"adapterId"`
		AdapterConfig     json.RawMessage `json:"adapterConfig"`
		Enabled           *bool           `json:"enabled"`
		Priority          *int            `json:"priority"`
		DefaultPathAction *string         `json:"defaultPathAction"`
		OnAdapterError    *string         `json:"onAdapterError"`
		NetworkZone       *string         `json:"networkZone"`
		Source            *string         `json:"source"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := validateEnum("hostMatchType", deref(body.HostMatchType), validHostMatchTypes); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if msg := validateEnum("defaultPathAction", deref(body.DefaultPathAction), validDefaultPathActions); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if msg := validateEnum("onAdapterError", deref(body.OnAdapterError), validFailureActions); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if msg := validateEnum("networkZone", deref(body.NetworkZone), validNetworkZones); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	// Compile-check the host regex against the EFFECTIVE match type + pattern
	// (a PATCH may change one without the other), so a partial update can never
	// leave a REGEX host rule with an uncompilable pattern.
	effHostType := existing.HostMatchType
	if body.HostMatchType != nil {
		effHostType = *body.HostMatchType
	}
	effHostPattern := existing.HostPattern
	if body.HostPattern != nil {
		effHostPattern = *body.HostPattern
	}
	if msg := validateMatchRegex("hostPattern", effHostType, effHostPattern); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	updated, err := h.store.UpdateInterceptionDomain(c.Request().Context(), id, interceptionstore.UpdateInterceptionDomainInput{
		Name:              body.Name,
		Description:       body.Description,
		HostPattern:       body.HostPattern,
		HostMatchType:     body.HostMatchType,
		AdapterID:         body.AdapterID,
		AdapterConfig:     body.AdapterConfig,
		Enabled:           body.Enabled,
		Priority:          body.Priority,
		DefaultPathAction: body.DefaultPathAction,
		OnAdapterError:    body.OnAdapterError,
		NetworkZone:       body.NetworkZone,
		Source:            body.Source,
	})
	if err != nil {
		h.logger.Error("update interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if updated == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

// DELETE /interception-domains/:id

func (h *Handler) DeleteInterceptionDomain(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.store.GetInterceptionDomain(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
	}

	if err := h.store.DeleteInterceptionDomain(c.Request().Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
		}
		h.logger.Error("delete interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

// POST /interception-domains/:id/paths

func (h *Handler) CreateInterceptionPath(c echo.Context) error {
	domainID := c.Param("id")
	existing, err := h.store.GetInterceptionDomain(c.Request().Context(), domainID)
	if err != nil {
		h.logger.Error("get interception domain", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception domain not found", "not_found", ""))
	}

	var body interceptionPath
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	inputs, msg := buildPathInputs([]interceptionPath{body})
	if msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	p, err := h.store.CreateInterceptionPath(c.Request().Context(), domainID, inputs[0])
	if err != nil {
		h.logger.Error("create interception path", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbCreate)
	ae.EntityID = p.ID
	ae.AfterState = p
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, p)
}

// PUT /interception-domains/:id/paths/:pathId

func (h *Handler) UpdateInterceptionPath(c echo.Context) error {
	domainID := c.Param("id")
	pathID := c.Param("pathId")

	existing, err := h.store.GetInterceptionPath(c.Request().Context(), pathID)
	if err != nil {
		h.logger.Error("get interception path", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
	}
	// Defense in depth: the URL encodes the parent domain, so reject any
	// pathId that resolves to a different domain even if the row exists.
	if existing.DomainID != domainID {
		return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
	}

	var body struct {
		PathPattern []string `json:"pathPattern"`
		MatchType   *string  `json:"matchType"`
		Action      *string  `json:"action"`
		Priority    *int     `json:"priority"`
		Description *string  `json:"description"`
		Enabled     *bool    `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := validateEnum("matchType", deref(body.MatchType), validPathMatchTypes); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	if msg := validateEnum("action", deref(body.Action), validPathActions); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	// Compile-check path regexes against the EFFECTIVE match type — patterns
	// supplied in this PATCH if present, else the stored ones.
	effPathType := existing.MatchType
	if body.MatchType != nil {
		effPathType = *body.MatchType
	}
	effPatterns := existing.PathPattern
	if body.PathPattern != nil {
		effPatterns = body.PathPattern
	}
	for j, pat := range effPatterns {
		if msg := validateMatchRegex("pathPattern["+itoaPos(j)+"]", effPathType, pat); msg != "" {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
		}
	}

	updated, err := h.store.UpdateInterceptionPath(c.Request().Context(), pathID, interceptionstore.UpdateInterceptionPathInput{
		PathPattern: body.PathPattern,
		MatchType:   body.MatchType,
		Action:      body.Action,
		Priority:    body.Priority,
		Description: body.Description,
		Enabled:     body.Enabled,
	})
	if err != nil {
		h.logger.Error("update interception path", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if updated == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbUpdate)
	ae.EntityID = pathID
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

// DELETE /interception-domains/:id/paths/:pathId

func (h *Handler) DeleteInterceptionPath(c echo.Context) error {
	domainID := c.Param("id")
	pathID := c.Param("pathId")

	existing, err := h.store.GetInterceptionPath(c.Request().Context(), pathID)
	if err != nil {
		h.logger.Error("get interception path", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
	}
	if existing.DomainID != domainID {
		return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
	}

	if err := h.store.DeleteInterceptionPath(c.Request().Context(), pathID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Interception path not found", "not_found", ""))
		}
		h.logger.Error("delete interception path", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	h.invalidateInterceptionDomains(c)

	ae := audit.EntryFor(c, iam.ResourceInterceptionDomain, iam.VerbDelete)
	ae.EntityID = pathID
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

// validateDomainEnums checks every enum-shaped field on a create/update
// payload. Returns "" when everything is in range, or a 400-friendly
// message otherwise.
func validateDomainEnums(body interceptionDomainBody) string {
	if msg := validateEnum("hostMatchType", body.HostMatchType, validHostMatchTypes); msg != "" {
		return msg
	}
	if msg := validateEnum("defaultPathAction", body.DefaultPathAction, validDefaultPathActions); msg != "" {
		return msg
	}
	if msg := validateEnum("onAdapterError", body.OnAdapterError, validFailureActions); msg != "" {
		return msg
	}
	if msg := validateEnum("networkZone", body.NetworkZone, validNetworkZones); msg != "" {
		return msg
	}
	return ""
}

// buildPathInputs converts wire-level interceptionPath structs into the
// store's CreateInterceptionPathInput shape and validates each row. Returns
// a 400-friendly message when any path is malformed.
func buildPathInputs(paths []interceptionPath) ([]interceptionstore.CreateInterceptionPathInput, string) {
	out := make([]interceptionstore.CreateInterceptionPathInput, 0, len(paths))
	for i, p := range paths {
		if p.Action == "" {
			return nil, "paths[" + itoaPos(i) + "].action is required"
		}
		if msg := validateEnum("paths["+itoaPos(i)+"].matchType", p.MatchType, validPathMatchTypes); msg != "" {
			return nil, msg
		}
		if msg := validateEnum("paths["+itoaPos(i)+"].action", p.Action, validPathActions); msg != "" {
			return nil, msg
		}
		for j, pat := range p.PathPattern {
			if msg := validateMatchRegex("paths["+itoaPos(i)+"].pathPattern["+itoaPos(j)+"]", p.MatchType, pat); msg != "" {
				return nil, msg
			}
		}
		out = append(out, interceptionstore.CreateInterceptionPathInput{
			PathPattern: p.PathPattern,
			MatchType:   p.MatchType,
			Action:      p.Action,
			Priority:    p.Priority,
			Description: p.Description,
			Enabled:     p.Enabled,
		})
	}
	return out, ""
}

// itoaPos stringifies a small non-negative integer without pulling strconv
// into the import list for a single call site.
func itoaPos(n int) string {
	if n == 0 {
		return "0"
	}
	// small, bounded: path arrays above ~10 elements are vanishingly rare
	// in production configs, and this helper is only used for 400 messages.
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
