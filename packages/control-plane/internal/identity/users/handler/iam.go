package iam

import (
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterIAMRoutes registers IAM policy/group management routes.
func (h *Handler) RegisterIAMRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Policies
	g.GET("/iam/policies", h.ListIAMPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.GET("/iam/policies/:id", h.GetIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.GET("/iam/policies/:id/attachments", h.ListPolicyAttachments, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.POST("/iam/policies", h.CreateIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbCreate)))
	g.PUT("/iam/policies/:id", h.UpdateIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	g.DELETE("/iam/policies/:id", h.DeleteIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbDelete)))
	// Groups — gated on the canonical admin:iam-group.<verb> action so the
	// catalog row in shared/iam.Catalog is reachable and an operator can
	// be granted group management without policy management (and vice
	// versa). Audit emissions already use ResourceIamGroup; the iamMW
	// gate now lines up with them.
	g.GET("/iam/groups", h.ListIAMGroups, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.GET("/iam/groups/:id", h.GetIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.POST("/iam/groups", h.CreateIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbCreate)))
	g.PUT("/iam/groups/:id", h.UpdateIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id", h.DeleteIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbDelete)))
	g.GET("/iam/groups/:id/members", h.ListIAMGroupMembers, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.POST("/iam/groups/:id/members", h.AddIAMGroupMember, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id/members/:membershipId", h.RemoveIAMGroupMember, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.POST("/iam/groups/:id/policies", h.AttachIAMGroupPolicy, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id/policies/:attachmentId", h.DetachIAMGroupPolicy, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	// Principal attachments
	g.GET("/iam/principals/:type/:id/policies", h.ListPrincipalPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.POST("/iam/principals/:type/:id/policies", h.AttachPrincipalPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	g.DELETE("/iam/principals/:type/:id/policies/:attachmentId", h.DetachPrincipalPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	// Simulator
	g.POST("/iam/simulate", h.SimulateIAM, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
}
