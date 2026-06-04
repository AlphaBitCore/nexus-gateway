package iam

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// kebabCase matches the canonical kebab-case lowercase identifier shape.
// Resource names and verb values must satisfy this so the composed
// admin:<resource>.<verb> string is unambiguous when parsed back.
var kebabCase = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func TestCatalogResourceNamesUniqueAndKebabCase(t *testing.T) {
	seen := make(map[string]bool, len(Catalog))
	for _, r := range Catalog {
		if !kebabCase.MatchString(r.Name) {
			t.Errorf("resource %q: not canonical kebab-case lowercase", r.Name)
		}
		if seen[r.Name] {
			t.Errorf("resource %q appears more than once in Catalog", r.Name)
		}
		seen[r.Name] = true
	}
}

func TestCatalogServicesAreClosedEnum(t *testing.T) {
	allowed := map[Service]bool{
		ServiceGateway:    true,
		ServiceCompliance: true,
		ServiceAgent:      true,
		ServicePlatform:   true,
		ServiceIAM:        true,
	}
	for _, r := range Catalog {
		if !allowed[r.Service] {
			t.Errorf("resource %q: service %q is not in the closed Service enum", r.Name, r.Service)
		}
	}
}

func TestCatalogVerbsAreClosedEnumAndUniquePerResource(t *testing.T) {
	allVerbs := map[Verb]bool{
		VerbCreate: true, VerbRead: true, VerbUpdate: true, VerbDelete: true,
		VerbApprove: true, VerbReject: true, VerbRevoke: true, VerbRenew: true,
		VerbToggle: true, VerbExport: true, VerbSimulate: true,
		VerbForceResync: true, VerbWriteOverride: true,
		VerbWrite: true, VerbAcknowledge: true,
		VerbProbe: true, VerbRotate: true, VerbImport: true, VerbFulfill: true,
		VerbEnroll:          true,
		VerbManage:          true,
		VerbEmergencyEnable: true,
	}
	for _, r := range Catalog {
		seen := make(map[Verb]bool, len(r.Verbs))
		for _, v := range r.Verbs {
			if !allVerbs[v] {
				t.Errorf("resource %q: verb %q is not in the closed Verb enum", r.Name, v)
			}
			if !kebabCase.MatchString(string(v)) {
				t.Errorf("resource %q: verb %q is not kebab-case lowercase", r.Name, v)
			}
			if seen[v] {
				t.Errorf("resource %q: verb %q appears more than once", r.Name, v)
			}
			seen[v] = true
		}
	}
}

func TestActionFormat(t *testing.T) {
	got := ResourceVirtualKey.Action(VerbCreate)
	if want := "admin:virtual-key.create"; got != want {
		t.Errorf("Action: got %q, want %q", got, want)
	}
	if got := ResourceAuditLog.Action(VerbExport); got != "admin:audit-log.export" {
		t.Errorf("Action(audit-log,export) = %q", got)
	}
	if got := ResourceNode.Action(VerbForceResync); got != "admin:node.force-resync" {
		t.Errorf("Action(node,force-resync) = %q", got)
	}
}

func TestActionPanicsOnUndeclaredVerb(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Action did not panic on undeclared verb")
		}
	}()
	// provider does not declare VerbApprove
	_ = ResourceProvider.Action(VerbApprove)
}

func TestActionRegexShape(t *testing.T) {
	// The same regex the P6 frontend coverage test will enforce on
	// ACTION_MAP target strings — keeps Go and TS in sync.
	pat := regexp.MustCompile(`^admin:[a-z][a-z0-9-]*\.[a-z][a-z-]*$`)
	for _, a := range AllActions() {
		if !pat.MatchString(a) {
			t.Errorf("action %q does not match canonical regex", a)
		}
	}
}

func TestNRNFormat(t *testing.T) {
	cases := []struct {
		r          *ResourceDef
		scope, id  string
		wantSuffix string
	}{
		{ResourceProvider, "*", "openai", "nrn:nexus:gateway:*:provider/openai"},
		{ResourceProvider, "*", "*", "nrn:nexus:gateway:*:provider/*"},
		{ResourceAuditLog, "org-acme", "evt-123", "nrn:nexus:iam:org-acme:audit-log/evt-123"},
		{ResourceHook, "*", "*", "nrn:nexus:compliance:*:hook/*"},
	}
	for _, c := range cases {
		got := c.r.NRN(c.scope, c.id)
		if got != c.wantSuffix {
			t.Errorf("NRN(%s, %q, %q): got %q want %q", c.r.Name, c.scope, c.id, got, c.wantSuffix)
		}
	}
}

func TestSIEMEventTypeMatchesActionBody(t *testing.T) {
	// AC-3 invariant: SIEM eventType is exactly the IAM action body with
	// the "admin:" prefix stripped. Catches future divergence between the
	// SIEM bridge's ClassifyAdminEvent format and the IAM action format.
	for i := range Catalog {
		r := &Catalog[i]
		for _, v := range r.Verbs {
			action := r.Action(v)
			body := strings.TrimPrefix(action, "admin:")
			siem := SIEMEventType(r.Name, v)
			if body != siem {
				t.Errorf("alignment broken on %s/%s: action body %q != SIEM eventType %q",
					r.Name, v, body, siem)
			}
		}
	}
}

func TestMustFindReturnsCatalogEntry(t *testing.T) {
	r := MustFind("virtual-key")
	if r != ResourceVirtualKey {
		t.Errorf("MustFind(virtual-key) returned %p, want %p", r, ResourceVirtualKey)
	}
	if r.Name != "virtual-key" {
		t.Errorf("MustFind returned wrong resource: %q", r.Name)
	}
}

func TestMustFindPanicsOnUnknownResource(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustFind did not panic on unknown resource")
		}
		// The panic message must include the missing name and an
		// "available:" hint — confirms the caller-debugging UX is intact.
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is not string: %T", r)
		}
		if !strings.Contains(msg, "no-such-resource") {
			t.Errorf("panic message %q missing requested name", msg)
		}
		if !strings.Contains(msg, "available:") {
			t.Errorf("panic message %q missing available-list hint", msg)
		}
	}()
	_ = MustFind("no-such-resource")
}

func TestAllActionsIsSortedAndUnique(t *testing.T) {
	all := AllActions()
	if len(all) == 0 {
		t.Fatal("AllActions returned empty slice")
	}
	if !sort.StringsAreSorted(all) {
		t.Error("AllActions output is not sorted")
	}
	seen := make(map[string]bool, len(all))
	for _, a := range all {
		if seen[a] {
			t.Errorf("duplicate action in AllActions: %q", a)
		}
		seen[a] = true
	}
}

func TestAllActionsCoversEveryCatalogVerb(t *testing.T) {
	all := make(map[string]bool, 128)
	for _, a := range AllActions() {
		all[a] = true
	}
	for i := range Catalog {
		r := &Catalog[i]
		for _, v := range r.Verbs {
			a := r.Action(v)
			if !all[a] {
				t.Errorf("AllActions missing %q", a)
			}
		}
	}
}

func TestParseActionRoundTrips(t *testing.T) {
	for i := range Catalog {
		r := &Catalog[i]
		for _, v := range r.Verbs {
			a := r.Action(v)
			gotResource, gotVerb, ok := ParseAction(a)
			if !ok {
				t.Errorf("ParseAction(%q) ok=false", a)
				continue
			}
			if gotResource != r.Name {
				t.Errorf("ParseAction(%q) resource=%q want %q", a, gotResource, r.Name)
			}
			if gotVerb != v {
				t.Errorf("ParseAction(%q) verb=%q want %q", a, gotVerb, v)
			}
		}
	}
}

func TestParseActionRejectsMalformedInput(t *testing.T) {
	cases := []string{
		"",
		"admin",
		"admin:",
		"admin:provider",        // no verb
		"provider.create",       // no admin: prefix
		"AdminProvider.create",  // wrong prefix
		"admin:provider:create", // wrong separator
	}
	for _, c := range cases {
		if _, _, ok := ParseAction(c); ok {
			t.Errorf("ParseAction(%q) should have rejected input", c)
		}
	}
}

func TestConvenienceVarsMatchCatalog(t *testing.T) {
	// Every entry in Catalog must have a corresponding convenience var
	// pointer in catalog_data.go. The reverse check (every convenience
	// var → catalog row) is implicit because MustFind would have panicked
	// at init time otherwise.
	expected := map[string]*ResourceDef{
		"provider":             ResourceProvider,
		"model":                ResourceModel,
		"model-pricing":        ResourceModelPricing,
		"credential":           ResourceCredential,
		"virtual-key":          ResourceVirtualKey,
		"routing-rule":         ResourceRoutingRule,
		"quota-policy":         ResourceQuotaPolicy,
		"quota-override":       ResourceQuotaOverride,
		"quota-analytics":      ResourceQuotaAnalytics,
		"analytics":            ResourceAnalytics,
		"traffic-log":          ResourceTrafficLog,
		"prompt-cache":         ResourcePromptCache,
		"passthrough":          ResourcePassthrough,
		"semantic-cache":       ResourceSemanticCache,
		"extract-cache":        ResourceExtractCache,
		"agent-device":         ResourceAgentDevice,
		"device-group":         ResourceDeviceGroup,
		"device-assignment":    ResourceDeviceAssignment,
		"device-defaults":      ResourceDeviceDefaults,
		"agent-attestation":    ResourceAgentAttestation,
		"user":                 ResourceUser,
		"api-key":              ResourceApiKey,
		"oauth-client":         ResourceOAuthClient,
		"organization":         ResourceOrganization,
		"project":              ResourceProject,
		"iam-policy":           ResourceIamPolicy,
		"iam-group":            ResourceIamGroup,
		"audit-log":            ResourceAuditLog,
		"revocation":           ResourceRevocation,
		"identity-provider":    ResourceIdentityProvider,
		"device-enrollment":    ResourceDeviceEnrollment,
		"alert":                ResourceAlert,
		"kill-switch":          ResourceKillSwitch,
		"ai-guard-config":      ResourceAIGuardConfig,
		"observability":        ResourceObservability,
		"observability-dlq":    ResourceObservabilityDLQ,
		"settings":             ResourceSettings,
		"diagnostic-mode":      ResourceDiagnosticMode,
		"node":                 ResourceNode,
		"nexus-session":        ResourceNexusSession,
		"hook":                 ResourceHook,
		"rule-pack":            ResourceRulePack,
		"compliance-exemption": ResourceComplianceExemption,
		"compliance-report":    ResourceComplianceReport,
		"interception-domain":  ResourceInterceptionDomain,
		"dsar":                 ResourceDSAR,
		"payload-capture":      ResourcePayloadCapture,
	}
	for _, r := range Catalog {
		got, ok := expected[r.Name]
		if !ok {
			t.Errorf("Catalog entry %q has no convenience var in catalog_data.go", r.Name)
			continue
		}
		if got.Name != r.Name {
			t.Errorf("convenience var for %q points at %q", r.Name, got.Name)
		}
	}
	if len(expected) != len(Catalog) {
		t.Errorf("convenience var count = %d; Catalog len = %d", len(expected), len(Catalog))
	}
}

func TestServiceForAction_CanonicalActionsResolve(t *testing.T) {
	// Every canonical admin:<resource>.<verb> action must resolve to the
	// catalog Service that owns it. Without this, IAM resource-NRN
	// construction defaults to a wildcard service segment and service-
	// scoped resource patterns in policy documents become unenforceable
	// (see [[project-iam-resource-nrn-bug]]).
	for i := range Catalog {
		r := &Catalog[i]
		for _, v := range r.Verbs {
			action := r.Action(v)
			svc, ok := ServiceForAction(action)
			if !ok {
				t.Errorf("ServiceForAction(%q): ok=false", action)
				continue
			}
			if svc != r.Service {
				t.Errorf("ServiceForAction(%q): got %q, want %q", action, svc, r.Service)
			}
		}
	}
}

func TestServiceForAction_RejectsRoleIdentityMarkers(t *testing.T) {
	// Non-canonical actions used as role identity markers must NOT
	// resolve — callers should fall back to wildcard NRN. Per the
	// docstring, these include gateway:invoke:*, ai-guard:invoke,
	// device:heartbeat.
	for _, marker := range []string{
		"gateway:invoke:*",
		"ai-guard:invoke",
		"device:heartbeat",
		"some:colon:action",
	} {
		if svc, ok := ServiceForAction(marker); ok {
			t.Errorf("ServiceForAction(%q) should reject; returned svc=%q", marker, svc)
		}
	}
}

func TestServiceForAction_RejectsUnknownCanonicalResource(t *testing.T) {
	// admin:<typo>.<verb> shape — must reject because catalog miss.
	if svc, ok := ServiceForAction("admin:no-such-resource.create"); ok {
		t.Errorf("unknown resource: got svc=%q ok=true; want ok=false", svc)
	}
}

func TestServiceForAction_RejectsMalformed(t *testing.T) {
	// Empty / missing-verb / missing-prefix variants must all reject.
	for _, bad := range []string{"", "admin:", "admin:resource", "provider.create"} {
		if _, ok := ServiceForAction(bad); ok {
			t.Errorf("ServiceForAction(%q) should reject", bad)
		}
	}
}

func TestAllowsRejectsUndeclaredVerb(t *testing.T) {
	// Direct Allows() test — exercises both the matching-verb path and
	// the no-match path without relying on Action's panic.
	if !ResourceProvider.Allows(VerbCreate) {
		t.Error("ResourceProvider should allow VerbCreate")
	}
	if ResourceProvider.Allows(VerbApprove) {
		t.Error("ResourceProvider should NOT allow VerbApprove (no approve lifecycle)")
	}
	// Bogus verb string.
	if ResourceVirtualKey.Allows(Verb("nonsense")) {
		t.Error("Allows should reject unknown verb string")
	}
}

func TestCrudHelperReturnsFreshSliceEachCall(t *testing.T) {
	// Catches a future bug where crud() is changed to return a shared
	// package var — the bug would surface as one resource's append()
	// silently mutating another's Verbs slice.
	a := crud()
	b := crud()
	a[0] = "MUTATED"
	if b[0] == "MUTATED" {
		t.Fatal("crud() returns aliased slices; verbs leak across resources via append")
	}
}
