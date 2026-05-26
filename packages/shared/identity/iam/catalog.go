// Package iam provides the canonical resource/verb taxonomy that the IAM
// engine, audit log, SIEM bridge, and admin UI all derive from.
//
// This package is the single source of truth for:
//
//   - the resource type set (e.g. provider, virtual-key, audit-log)
//   - the verb set per resource (e.g. create/read/update/delete on virtual-key
//     plus approve/reject/revoke/renew)
//   - the IAM action string format admin:<resource>.<verb>
//   - the NRN template per resource
//   - the SIEM eventType format (<resource>.<verb>) — verified by a
//     consistency test against ClassifyAdminEvent
//
// Callers MUST construct identifiers through the helpers exposed here
// (ResourceDef.Action, ResourceDef.NRN, SIEMEventType). Hand-typed
// "admin:..." or audit "EntityType"/"Action" literals are prohibited in
// production code by the CI consistency gates.
package iam

import (
	"fmt"
	"sort"
	"strings"
)

// Verb is the action verb half of an IAM action identifier. The set is
// closed; adding a verb requires editing this file. Verb values are
// kebab-case lowercase so they compose cleanly with kebab-case resource
// names into the admin:<resource>.<verb> action format.
type Verb string

const (
	// Standard CRUD.
	VerbCreate Verb = "create"
	VerbRead   Verb = "read"
	VerbUpdate Verb = "update"
	VerbDelete Verb = "delete"

	// virtual-key lifecycle.
	VerbApprove Verb = "approve"
	VerbReject  Verb = "reject"
	VerbRevoke  Verb = "revoke"
	VerbRenew   Verb = "renew"

	// kill-switch operations.
	VerbToggle Verb = "toggle"

	// audit-log export (download a redacted slice for compliance review).
	VerbExport Verb = "export"

	// routing-rule what-if simulation against a candidate request.
	VerbSimulate Verb = "simulate"

	// node operations. force-resync triggers an immediate config push;
	// write-override sets a per-node override on the desired shadow.
	VerbForceResync   Verb = "force-resync"
	VerbWriteOverride Verb = "write-override"

	// settings / observability deep writes that bypass normal CRUD.
	// See SDD §3.4 — reserved for operations with broader blast radius
	// than VerbUpdate.
	VerbWrite Verb = "write"

	// alert lifecycle.
	VerbAcknowledge Verb = "acknowledge"

	// passthrough emergency-enable: the gate for flipping any
	// passthrough tier's enabled=true. Distinct from VerbWrite so we
	// can grant Provider/Compliance Admin the right to *disable*
	// passthrough (incident cleanup) and *delete* tier rows without
	// also granting the right to *enable* a fresh bypass — that
	// power stays with Incident Response role only.
	VerbEmergencyEnable Verb = "emergency-enable"

	// credential / api-key lifecycle. probe = active validity check (issues
	// a no-op request against the provider). rotate = generate a new
	// secret while keeping the credential record. Both carry distinct
	// SIEM-event significance from a plain update.
	VerbProbe  Verb = "probe"
	VerbRotate Verb = "rotate"

	// rule-pack lifecycle. import = ingest a vendor pack archive into the
	// store (distinct from update which edits in-place).
	VerbImport Verb = "import"

	// dsar lifecycle. fulfill = release subject-access response (a
	// compliance-significant event). Distinct from update so that SIEM
	// can alert on fulfillment specifically.
	VerbFulfill Verb = "fulfill"

	// device-enrollment. Sole verb on the carved-out resource that
	// gates POST /api/agent/sso-enroll. Granting it permits a CP user
	// to enroll a desktop agent into the fleet; kept distinct from any
	// other admin verb so the privilege can be scoped in isolation.
	VerbEnroll Verb = "enroll"

	// Reserved for coarse-grained operations that legitimately span
	// multiple verbs. Avoid in new resources; prefer splitting into
	// CRUD + specific verbs.
	VerbManage Verb = "manage"
)

// Service is the high-level product domain that owns a resource. It
// becomes the second segment of the NRN (nrn:nexus:<service>:...) and
// is used for service-level wildcards in IAM resource patterns. Each
// Service value corresponds to one product capability the platform
// sells, so managed policies can be expressed as "this role owns
// services X and Y, reads service Z".
type Service string

const (
	// ServiceGateway — AI Traffic Gateway product line.
	// Hosts: providers, models, model pricing, credentials, virtual
	// keys, routing rules, quotas, analytics, traffic-log. Owned by
	// the provider-admin role; viewed by viewers and (for forensics)
	// security-admin.
	ServiceGateway Service = "gateway"

	// ServiceCompliance — Compliance Pipeline product line.
	// Hosts: hooks, rule packs, exemptions (agent + manual), DSAR,
	// interception domains, compliance reports, AI Guard config,
	// kill switch. Owned by the security-admin role; hooks and
	// rule packs are co-owned with provider-admin.
	ServiceCompliance Service = "compliance"

	// ServiceAgent — Agent Fleet product line (desktop client).
	// Hosts: agent-device, device-group, device-assignment.
	// Owned by security-admin (compliance segmentation duty).
	ServiceAgent Service = "agent"

	// ServicePlatform — platform operations.
	// Hosts: node, observability, settings, alert, diagnostic-mode.
	// Cross-cutting platform-ops surface used by every admin role in
	// some capacity (provider-admin owns AI Gateway node ops; security
	// owns alerts + observability; super-admin owns global settings).
	ServicePlatform Service = "platform"

	// ServiceIAM — Identity & Access (users, RBAC, audit).
	// Hosts: user, api-key, organization, project, iam-policy,
	// iam-group, audit-log, revocation, nexus-session. Owned by
	// super-admin for writes; security-admin reads for audit.
	ServiceIAM Service = "iam"
)

// ResourceDef is one row of the canonical Catalog. Values are immutable
// once Catalog is loaded; the convenience vars (ResourceProvider, etc.)
// are the recommended access path for handlers.
type ResourceDef struct {
	// Name is the canonical kebab-case resource identifier
	// (e.g. "virtual-key"). It is the value that appears in
	// admin:<resource>.<verb>, audit Entry.EntityType, and SIEM
	// eventType.
	Name string

	// Service routes the NRN template. See Service constants above.
	Service Service

	// Verbs is the closed set of operations permitted on this resource.
	// Empty means the resource is read-only-internal — referenced by
	// audit/SIEM but not gated by any iamMW call. New verbs must be
	// added both here and to the Verb constant set above.
	Verbs []Verb
}

// Action returns the canonical IAM action string for (resource, verb).
// Panics if verb is not declared in r.Verbs — this is a programmer error
// caught at server startup, not a runtime user input failure.
//
//	iam.ResourceVirtualKey.Action(iam.VerbCreate)
//	// → "admin:virtual-key.create"
func (r *ResourceDef) Action(v Verb) string {
	if !r.Allows(v) {
		panic(fmt.Sprintf("iam.Catalog: verb %q not declared on resource %q", v, r.Name))
	}
	return "admin:" + r.Name + "." + string(v)
}

// NRN returns the canonical NRN string for a specific resource instance.
// The scope segment is caller-provided (typically "*" for global,
// "<org>/<project>" for scoped). The id segment is the resource's
// primary key, or "*" for resource-set wildcards in policy resource
// patterns.
//
//	iam.ResourceProvider.NRN("*", "openai")
//	// → "nrn:nexus:gateway:*:provider/openai"
//	iam.ResourceProvider.NRN("*", "*")
//	// → "nrn:nexus:gateway:*:provider/*"
func (r *ResourceDef) NRN(scope, id string) string {
	return "nrn:nexus:" + string(r.Service) + ":" + scope + ":" + r.Name + "/" + id
}

// Allows reports whether v is declared as a permitted verb on r. Callers
// constructing audit entries or arbitrary action strings outside the
// ResourceDef.Action helper should validate via Allows to catch typos
// at server start rather than at request time.
func (r *ResourceDef) Allows(v Verb) bool {
	for _, declared := range r.Verbs {
		if declared == v {
			return true
		}
	}
	return false
}

// SIEMEventType returns the SIEM event-type string for an admin audit
// event identified by canonical (resource, verb). The output is the IAM
// action body with the "admin:" prefix stripped — operators see the
// same string in IAM policy and in SIEM filter. The classifier in
// packages/nexus-hub/internal/observability/siem/classify.go must produce identical
// output when fed an audit Entry constructed via audit.EntryFor.
//
//	iam.SIEMEventType("virtual-key", iam.VerbCreate)
//	// → "virtual-key.create"
func SIEMEventType(resource string, v Verb) string {
	return resource + "." + string(v)
}

// AllActions returns every (resource, verb) action expressed as the
// canonical admin:<resource>.<verb> string, sorted for determinism.
// Used by GetMePermissions to enumerate the set of actions the IAM
// engine evaluates per principal.
func AllActions() []string {
	out := make([]string, 0, 128)
	for i := range Catalog {
		r := &Catalog[i]
		for _, v := range r.Verbs {
			out = append(out, r.Action(v))
		}
	}
	sort.Strings(out)
	return out
}

// MustFind returns the ResourceDef with the given canonical name, or
// panics if no such resource exists. The catalog is static and known at
// build time; a miss is a programmer error (typo in handler code, stale
// reference after a catalog edit, etc.) and surfacing it at server
// startup is preferable to silently routing the iamMW call to nil.
//
// Handlers should prefer the convenience vars (ResourceProvider,
// ResourceVirtualKey, …) — MustFind exists for code that operates over
// the catalog generically (CI tests, the action-catalog HTTP handler,
// etc.).
func MustFind(name string) *ResourceDef {
	if r := byName[name]; r != nil {
		return r
	}
	available := make([]string, 0, len(byName))
	for n := range byName {
		available = append(available, n)
	}
	sort.Strings(available)
	panic(fmt.Sprintf(
		"iam.Catalog: no resource named %q (available: %s)",
		name, strings.Join(available, ", "),
	))
}

// ParseAction splits a canonical "admin:<resource>.<verb>" string back
// into (resource, verb). Returns ok=false for malformed input. Used by
// the CI consistency tests and by debugging tooling; production code
// should compose actions via ResourceDef.Action and never need to
// re-parse them.
func ParseAction(action string) (resource string, verb Verb, ok bool) {
	rest, found := strings.CutPrefix(action, "admin:")
	if !found {
		return "", "", false
	}
	body, v, found := strings.Cut(rest, ".")
	if !found {
		return "", "", false
	}
	return body, Verb(v), true
}

// ServiceForAction parses a canonical admin:<resource>.<verb> action and
// returns the catalog Service that owns the resource. Returns ok=false
// for non-canonical actions (e.g. the role-identity markers
// "gateway:invoke:*", "ai-guard:invoke", "device:heartbeat") and for
// canonical-shaped actions whose resource is not in the catalog (typo,
// stale reference). Callers that need a fallback should treat
// ok=false as "use a wildcard NRN".
//
// Used by the iamauth middleware and GetMePermissions to compute the
// NRN that the IAM engine evaluates against — without this, every
// admin:<svc>.<verb> action would be checked under a single hardcoded
// service NRN, making service-scoped Resource patterns in policy
// documents unenforceable.
func ServiceForAction(action string) (Service, bool) {
	resource, _, ok := ParseAction(action)
	if !ok {
		return "", false
	}
	r, found := byName[resource]
	if !found {
		return "", false
	}
	return r.Service, true
}
