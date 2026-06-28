package iam

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func allow(actions, resources []string) Statement {
	return Statement{Effect: "Allow", Action: actions, Resource: resources}
}

func doc(stmts ...Statement) PolicyDocument {
	return PolicyDocument{Version: PolicyVersion, Statement: stmts}
}

// engineFor builds an engine whose loader returns `held` as the caller's
// effective policies, regardless of principal — PrincipalCoversDocument only
// ever evaluates the one caller, so this is sufficient.
func engineFor(held PolicyDocument) *Engine {
	return NewEngine(&mockLoader{policies: []LoadedPolicy{{
		ID: "held", Name: "held", Document: held, Source: "direct",
	}}}, slog.Default())
}

func cover(t *testing.T, e *Engine, candidate PolicyDocument) (bool, string, string) {
	t.Helper()
	ok, a, r, err := e.PrincipalCoversDocument(context.Background(), "nexus_user", "caller", candidate)
	if err != nil {
		t.Fatalf("PrincipalCoversDocument error: %v", err)
	}
	return ok, a, r
}

// TestCovers_SuperAdminCoversAnything: a principal whose own policy Allows "*"
// on the universal NRN covers every candidate document, including admin:* and a
// bare "*" resource (normalised to the universal NRN). This is the no-special-
// casing super-admin path.
func TestCovers_SuperAdminCoversAnything(t *testing.T) {
	super := engineFor(doc(allow([]string{"*"}, []string{universalNRN})))
	for name, cand := range map[string]PolicyDocument{
		"admin-star-on-star":   doc(allow([]string{"admin:*"}, []string{"*"})),
		"star-on-star":         doc(allow([]string{"*"}, []string{"*"})),
		"star-on-universalNRN": doc(allow([]string{"*"}, []string{universalNRN})),
		"specific":             doc(allow([]string{"admin:credential.read"}, []string{"nrn:nexus:gateway:*:credential/*"})),
	} {
		if ok, a, r := cover(t, super, cand); !ok {
			t.Errorf("%s: super-admin must cover candidate; uncovered %s on %s", name, a, r)
		}
	}
}

// TestCovers_ScopedCallerBlockedFromEscalation is the core grant-ceiling guard: a
// delegated IAM operator holding only iam-policy.* / iam-group.* must NOT be able
// to author/attach a policy granting admin:* — the escalation-to-super-admin
// payload.
func TestCovers_ScopedCallerBlockedFromEscalation(t *testing.T) {
	operator := engineFor(doc(allow(
		[]string{"admin:iam-policy.*", "admin:iam-group.*"},
		[]string{"*"},
	)))
	escalation := doc(allow([]string{"admin:*"}, []string{"*"}))
	ok, missA, missR := cover(t, operator, escalation)
	if ok {
		t.Fatal("scoped IAM operator must NOT cover an admin:* grant — privilege escalation not blocked")
	}
	if missA == "" {
		t.Error("expected a concrete missing action for the 403 message")
	}
	if strings.HasPrefix(missA, "admin:iam-policy.") || strings.HasPrefix(missA, "admin:iam-group.") {
		t.Errorf("first miss %q is a permission the caller DOES hold — coverage logic is wrong", missA)
	}
	if missR != "*" {
		t.Errorf("missResource = %q; want the candidate's own pattern %q", missR, "*")
	}
}

// TestCovers_ScopedCallerWithinScopeAllowed: the same operator CAN delegate
// permissions it actually holds (the legitimate use case the separate verbs
// invite) — covering must not over-reject.
func TestCovers_ScopedCallerWithinScopeAllowed(t *testing.T) {
	operator := engineFor(doc(allow([]string{"admin:iam-policy.*"}, []string{"*"})))
	within := doc(allow([]string{"admin:iam-policy.read", "admin:iam-policy.create"}, []string{"*"}))
	if ok, a, r := cover(t, operator, within); !ok {
		t.Errorf("operator must cover a grant within its own scope; uncovered %s on %s", a, r)
	}
}

// TestCovers_ResourceScopeEnforced: a caller scoped to one resource NRN may
// grant on that scope but NOT broaden to "*".
func TestCovers_ResourceScopeEnforced(t *testing.T) {
	scoped := engineFor(doc(allow(
		[]string{"admin:credential.read"},
		[]string{"nrn:nexus:gateway:*:credential/*"},
	)))
	if ok, _, _ := cover(t, scoped, doc(allow([]string{"admin:credential.read"}, []string{"nrn:nexus:gateway:*:credential/openai"}))); !ok {
		t.Error("caller must cover a grant narrower-or-equal to its own resource scope")
	}
	if ok, _, _ := cover(t, scoped, doc(allow([]string{"admin:credential.read"}, []string{"*"}))); ok {
		t.Error("caller scoped to one NRN must NOT cover a grant on the universal '*' scope")
	}
}

// TestCovers_DenyIgnoredConservatively: a candidate Deny does not narrow the
// ceiling — Allow admin:* with a Deny on one action still requires the caller to
// hold all of admin:*, so the scoped operator is still blocked.
func TestCovers_DenyIgnoredConservatively(t *testing.T) {
	operator := engineFor(doc(allow([]string{"admin:iam-policy.*"}, []string{"*"})))
	candidate := doc(
		allow([]string{"admin:*"}, []string{"*"}),
		Statement{Effect: "Deny", Action: []string{"admin:credential.read"}, Resource: []string{"*"}},
	)
	if ok, _, _ := cover(t, operator, candidate); ok {
		t.Error("Deny must be ignored (conservative) — admin:* grant must still be blocked for a scoped caller")
	}
}

// TestCovers_NonCatalogLiteralTokenBounded: a non-catalog action token (e.g. a
// role-identity / data-plane invoke marker) is bounded by its literal string, not
// silently skipped because it is absent from AllActions().
func TestCovers_NonCatalogLiteralTokenBounded(t *testing.T) {
	operator := engineFor(doc(allow([]string{"admin:iam-policy.*"}, []string{"*"})))
	cand := doc(allow([]string{"gateway:invoke:*"}, []string{"*"}))
	ok, missA, _ := cover(t, operator, cand)
	if ok {
		t.Fatal("operator must NOT cover a non-catalog token it does not hold")
	}
	if missA != "gateway:invoke:*" {
		t.Errorf("missAction = %q; want the literal non-catalog token", missA)
	}
	holder := engineFor(doc(allow([]string{"gateway:invoke:*"}, []string{"*"})))
	if ok, a, r := cover(t, holder, cand); !ok {
		t.Errorf("a caller holding the token must cover it; uncovered %s on %s", a, r)
	}
}

// TestCovers_EmptyAndDenyOnlyDocsCovered: a document with no Allow confers no
// permission, so any caller covers it.
func TestCovers_EmptyAndDenyOnlyDocsCovered(t *testing.T) {
	nobody := engineFor(doc()) // caller holds nothing
	if ok, _, _ := cover(t, nobody, doc()); !ok {
		t.Error("empty document confers nothing — must be covered")
	}
	denyOnly := doc(Statement{Effect: "Deny", Action: []string{"*"}, Resource: []string{"*"}})
	if ok, _, _ := cover(t, nobody, denyOnly); !ok {
		t.Error("deny-only document confers nothing — must be covered")
	}
}

// TestCovers_LoaderErrorFailsClosed: an engine/store failure surfaces as err so
// the caller fails closed (denies the grant) rather than treating it as covered.
func TestCovers_LoaderErrorFailsClosed(t *testing.T) {
	e := NewEngine(&errLoader{err: errors.New("db down")}, slog.Default())
	ok, _, _, err := e.PrincipalCoversDocument(context.Background(), "nexus_user", "caller",
		doc(allow([]string{"admin:iam-policy.read"}, []string{"*"})))
	if err == nil {
		t.Fatal("loader error must surface as a non-nil err (fail-closed)")
	}
	if ok {
		t.Error("covered must be false when evaluation errored")
	}
}

// byPrincipalLoader returns a distinct policy set per (type,id), so a single
// engine can model both a caller and a separate owner principal. Used by the
// PrincipalCoversPrincipal tests.
type byPrincipalLoader struct {
	byID map[string][]LoadedPolicy
	err  error
}

func (l *byPrincipalLoader) LoadPolicies(_ context.Context, pt, pid string) ([]LoadedPolicy, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byID[pt+":"+pid], nil
}

func loaded(name string, d PolicyDocument) LoadedPolicy {
	return LoadedPolicy{ID: name, Name: name, Document: d, Source: "direct"}
}

// TestCoversPrincipal_SuperAdminCallerCoversOwner: a super-admin caller covers an
// owner who themselves holds full access — the cross-owner mint is allowed.
func TestCoversPrincipal_SuperAdminCallerCoversOwner(t *testing.T) {
	e := NewEngine(&byPrincipalLoader{byID: map[string][]LoadedPolicy{
		"nexus_user:caller": {loaded("c", doc(allow([]string{"*"}, []string{universalNRN})))},
		"nexus_user:owner":  {loaded("o", doc(allow([]string{"*"}, []string{universalNRN})))},
	}}, slog.Default())
	ok, a, r, err := e.PrincipalCoversPrincipal(context.Background(), "nexus_user", "caller", "nexus_user", "owner")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Errorf("super-admin caller must cover owner; miss=%s on %s", a, r)
	}
}

// TestCoversPrincipal_ScopedCallerBlockedFromPowerfulOwner: a narrowly-scoped
// caller does NOT cover a super-admin owner — the escalating mint is rejected
// and the first uncovered (action,resource) is reported.
func TestCoversPrincipal_ScopedCallerBlockedFromPowerfulOwner(t *testing.T) {
	e := NewEngine(&byPrincipalLoader{byID: map[string][]LoadedPolicy{
		"nexus_user:caller": {loaded("c", doc(allow([]string{"admin:api-key.create"}, []string{universalNRN})))},
		"nexus_user:owner":  {loaded("o", doc(allow([]string{"*"}, []string{universalNRN})))},
	}}, slog.Default())
	ok, a, _, err := e.PrincipalCoversPrincipal(context.Background(), "nexus_user", "caller", "nexus_user", "owner")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("scoped caller must NOT cover a super-admin owner")
	}
	if a == "" {
		t.Error("expected a reported missing action for the 403 message")
	}
}

// TestCoversPrincipal_OwnerNoPolicies_TriviallyCovered: an owner with no attached
// policies confers nothing, so any caller covers it.
func TestCoversPrincipal_OwnerNoPolicies_TriviallyCovered(t *testing.T) {
	e := NewEngine(&byPrincipalLoader{byID: map[string][]LoadedPolicy{
		"nexus_user:caller": {loaded("c", doc(allow([]string{"admin:api-key.create"}, []string{universalNRN})))},
		// owner absent → no policies
	}}, slog.Default())
	ok, _, _, err := e.PrincipalCoversPrincipal(context.Background(), "nexus_user", "caller", "nexus_user", "owner")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("owner with no policies must be trivially covered")
	}
}

// TestCoversPrincipal_OwnerLoadError_FailsClosed: a loader error fails closed —
// non-nil err and covered=false.
func TestCoversPrincipal_OwnerLoadError_FailsClosed(t *testing.T) {
	e := NewEngine(&byPrincipalLoader{err: errors.New("db down")}, slog.Default())
	ok, _, _, err := e.PrincipalCoversPrincipal(context.Background(), "nexus_user", "caller", "nexus_user", "owner")
	if err == nil {
		t.Fatal("loader error must surface as non-nil err (fail-closed)")
	}
	if ok {
		t.Error("covered must be false on load error")
	}
}

// callerErrLoader loads the OWNER's policies successfully but errors when the
// inner PrincipalCoversDocument loads the CALLER's policies — exercising the
// per-statement coverage error propagation (cerr) inside
// PrincipalCoversPrincipal, which must fail closed.
type callerErrLoader struct {
	ownerID  string
	ownerDoc PolicyDocument
}

func (l *callerErrLoader) LoadPolicies(_ context.Context, _, pid string) ([]LoadedPolicy, error) {
	if pid == l.ownerID {
		return []LoadedPolicy{loaded("o", l.ownerDoc)}, nil
	}
	return nil, errors.New("caller load failed")
}

// TestCoversPrincipal_CallerEvalError_FailsClosed: the owner loads, but the inner
// coverage check fails to load the caller — the error propagates and the mint
// fails closed.
func TestCoversPrincipal_CallerEvalError_FailsClosed(t *testing.T) {
	e := NewEngine(&callerErrLoader{
		ownerID:  "owner",
		ownerDoc: doc(allow([]string{"admin:api-key.create"}, []string{universalNRN})),
	}, slog.Default())
	ok, _, _, err := e.PrincipalCoversPrincipal(context.Background(), "nexus_user", "caller", "nexus_user", "owner")
	if err == nil {
		t.Fatal("inner caller-eval error must surface as non-nil err (fail-closed)")
	}
	if ok {
		t.Error("covered must be false on inner eval error")
	}
}
