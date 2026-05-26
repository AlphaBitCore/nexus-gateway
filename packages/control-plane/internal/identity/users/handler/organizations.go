package iam

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterOrganizationRoutes registers organization + project CRUD routes.
func (h *Handler) RegisterOrganizationRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/organizations", h.ListOrganizations, iamMW(iam.ResourceOrganization.Action(iam.VerbRead)))
	g.POST("/organizations", h.CreateOrganization, iamMW(iam.ResourceOrganization.Action(iam.VerbCreate)))
	g.GET("/organizations/:id", h.GetOrganization, iamMW(iam.ResourceOrganization.Action(iam.VerbRead)))
	g.PUT("/organizations/:id", h.UpdateOrganization, iamMW(iam.ResourceOrganization.Action(iam.VerbUpdate)))
	g.DELETE("/organizations/:id", h.DeleteOrganization, iamMW(iam.ResourceOrganization.Action(iam.VerbDelete)))

	g.GET("/projects", h.ListProjects, iamMW(iam.ResourceProject.Action(iam.VerbRead)))
	g.POST("/projects", h.CreateProject, iamMW(iam.ResourceProject.Action(iam.VerbCreate)))
	g.GET("/projects/:id", h.GetProject, iamMW(iam.ResourceProject.Action(iam.VerbRead)))
	g.PUT("/projects/:id", h.UpdateProject, iamMW(iam.ResourceProject.Action(iam.VerbUpdate)))
	g.DELETE("/projects/:id", h.DeleteProject, iamMW(iam.ResourceProject.Action(iam.VerbDelete)))
}

func (h *Handler) ListOrganizations(c echo.Context) error {
	orgs, err := h.orgs.ListOrganizations(c.Request().Context())
	if err != nil {
		h.logger.Error("list organizations", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": orgs})
}

func (h *Handler) GetOrganization(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	org, err := h.orgs.GetOrganization(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if org == nil {
		return c.JSON(http.StatusNotFound, errJSON("Organization not found", "not_found", ""))
	}

	// Enrich with projects, children, and parent for the detail view.
	projects, _, _ := h.orgs.ListProjects(ctx, orgstore.ProjectListParams{OrganizationID: id, Limit: 100})
	children, _ := h.orgs.ListChildOrganizations(ctx, id)

	var parent *orgstore.Organization
	if org.ParentID != nil {
		parent, _ = h.orgs.GetOrganization(ctx, *org.ParentID)
	}

	resp := map[string]any{
		"id": org.ID, "name": org.Name, "code": org.Code, "parentId": org.ParentID,
		"description": org.Description, "contactName": org.ContactName,
		"contactEmail": org.ContactEmail, "contactPhone": org.ContactPhone,
		"enabled": org.Enabled, "createdAt": org.CreatedAt, "updatedAt": org.UpdatedAt,
		"projects": projects, "children": children,
		"projectCount": len(projects), "childCount": len(children),
	}
	if parent != nil {
		resp["parent"] = map[string]any{"id": parent.ID, "name": parent.Name}
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) CreateOrganization(c echo.Context) error {
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	name, _ := body["name"].(string)
	code, _ := body["code"].(string)
	if name == "" || code == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name and code are required", "validation_error", ""))
	}

	org, err := h.orgs.CreateOrganization(c.Request().Context(), body)
	if err != nil {
		h.logger.Error("create organization", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create organization", "server_error", ""))
	}

	// e31-s10: invalidate ai-gateway's PolicyCache so OrgParents and the
	// quota policy list pick up the new org without an ai-gateway restart.
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceOrganization, iam.VerbCreate)
	ae.EntityID = org.ID
	ae.AfterState = org
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, org)
}

func (h *Handler) UpdateOrganization(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name         *string `json:"name"`
		Code         *string `json:"code"`
		ParentID     *string `json:"parentId"`
		Description  *string `json:"description"`
		ContactName  *string `json:"contactName"`
		ContactEmail *string `json:"contactEmail"`
		ContactPhone *string `json:"contactPhone"`
		Enabled      *bool   `json:"enabled"`
		Timezone     *string `json:"timezone"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	// Validate IANA TZ name to prevent typo'd values from poisoning
	// downstream business-rule computations (e.g. quota window rollup).
	if body.Timezone != nil && *body.Timezone != "" {
		if _, err := time.LoadLocation(*body.Timezone); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON(
				"timezone must be a valid IANA timezone name",
				"validation_error", ""))
		}
	}

	params := orgstore.UpdateOrganizationParams{
		Name:         body.Name,
		Code:         body.Code,
		ParentID:     body.ParentID,
		Description:  body.Description,
		ContactName:  body.ContactName,
		ContactEmail: body.ContactEmail,
		ContactPhone: body.ContactPhone,
		Enabled:      body.Enabled,
		Timezone:     body.Timezone,
	}

	org, err := h.orgs.UpdateOrganization(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update", "server_error", ""))
	}

	// e31-s10: invalidate ai-gateway's PolicyCache (re-parenting changes
	// the OrgParents map; rename/disable affects display + enabled flag).
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceOrganization, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = org
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, org)
}

func (h *Handler) DeleteOrganization(c echo.Context) error {
	id := c.Param("id")
	children, projects, _ := h.orgs.CountOrgDependents(c.Request().Context(), id)
	if children > 0 || projects > 0 {
		return c.JSON(http.StatusConflict, errJSON("Cannot delete: has children or projects", "validation_error", ""))
	}

	if err := h.orgs.DeleteOrganization(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete", "server_error", ""))
	}

	// e31-s10: invalidate ai-gateway's PolicyCache so the deleted org no
	// longer appears in OrgParents.
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceOrganization, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) ListProjects(c echo.Context) error {
	ctx := c.Request().Context()
	pg := parsePagination(c)
	params := orgstore.ProjectListParams{
		Q: c.QueryParam("q"), Status: c.QueryParam("status"),
		OrganizationID: c.QueryParam("organizationId"),
		Limit:          pg.Limit, Offset: pg.Offset,
	}
	projects, total, err := h.orgs.ListProjects(ctx, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	enriched := h.enrichProjects(ctx, projects)
	return c.JSON(http.StatusOK, map[string]any{"data": enriched, "total": total})
}

func (h *Handler) GetProject(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.orgs.GetProject(ctx, c.Param("id"))
	if err != nil || p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Project not found", "not_found", ""))
	}
	enriched := h.enrichProjects(ctx, []orgstore.Project{*p})
	resp := enriched[0]
	// Detail view: include VK list
	vks, _, _ := h.vk.ListVirtualKeys(ctx, vkstore.VirtualKeyListParams{ProjectID: p.ID, Limit: 100})
	resp["virtualKeys"] = vks
	return c.JSON(http.StatusOK, resp)
}

// enrichProjects adds organization name and VK count to each project.
func (h *Handler) enrichProjects(ctx context.Context, projects []orgstore.Project) []map[string]any {
	// Batch-fetch org names
	orgNames := make(map[string]string)
	for _, p := range projects {
		if _, ok := orgNames[p.OrganizationID]; !ok {
			org, err := h.orgs.GetOrganization(ctx, p.OrganizationID)
			if err == nil && org != nil {
				orgNames[p.OrganizationID] = org.Name
			}
		}
	}

	result := make([]map[string]any, len(projects))
	for i, p := range projects {
		vkCount, _ := h.orgs.CountProjectVirtualKeys(ctx, p.ID)
		result[i] = map[string]any{
			"id": p.ID, "name": p.Name, "code": p.Code,
			"organizationId": p.OrganizationID,
			"description":    p.Description, "contactName": p.ContactName,
			"contactEmail": p.ContactEmail, "status": p.Status,
			"createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
			"organization": map[string]any{
				"id":   p.OrganizationID,
				"name": orgNames[p.OrganizationID],
			},
			"_count": map[string]any{
				"virtualKeys": vkCount,
			},
		}
	}
	return result
}

func (h *Handler) CreateProject(c echo.Context) error {
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	name, _ := body["name"].(string)
	code, _ := body["code"].(string)
	orgID, _ := body["organizationId"].(string)
	if name == "" || code == "" || orgID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name, code, and organizationId are required", "validation_error", ""))
	}
	if body["status"] == nil {
		body["status"] = "active"
	}

	p, err := h.orgs.CreateProject(c.Request().Context(), body)
	if err != nil {
		h.logger.Error("create project", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create project", "server_error", ""))
	}

	// ai-gateway's policyCache.Load() folds projects into the
	// `organizations` snapshot it reads from DB; invalidate the same
	// Cat B key so a freshly-created project becomes routable on the
	// next request rather than the next gateway restart.
	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceProject, iam.VerbCreate)
	ae.EntityID = p.ID
	ae.AfterState = p
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, p)
}

func (h *Handler) UpdateProject(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name           *string `json:"name"`
		Code           *string `json:"code"`
		OrganizationID *string `json:"organizationId"`
		Description    *string `json:"description"`
		ContactName    *string `json:"contactName"`
		ContactEmail   *string `json:"contactEmail"`
		Status         *string `json:"status"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := orgstore.UpdateProjectParams{
		Name:           body.Name,
		Code:           body.Code,
		OrganizationID: body.OrganizationID,
		Description:    body.Description,
		ContactName:    body.ContactName,
		ContactEmail:   body.ContactEmail,
		Status:         body.Status,
	}

	p, err := h.orgs.UpdateProject(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceProject, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = p
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, p)
}

func (h *Handler) DeleteProject(c echo.Context) error {
	id := c.Param("id")
	vkCount, _ := h.orgs.CountProjectVirtualKeys(c.Request().Context(), id)
	if vkCount > 0 {
		return c.JSON(http.StatusConflict, errJSON("Cannot delete: project has virtual keys", "validation_error", ""))
	}

	if err := h.orgs.DeleteProject(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete", "server_error", ""))
	}

	if h.hub != nil {
		h.hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
	}

	ae := audit.EntryFor(c, iam.ResourceProject, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}
