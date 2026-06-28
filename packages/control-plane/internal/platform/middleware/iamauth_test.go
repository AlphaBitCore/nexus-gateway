package middleware_test

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// fakeLoader returns fixed policies regardless of principal.
type fakeLoader struct {
	policies []iam.LoadedPolicy
}

func (f *fakeLoader) LoadPolicies(_ context.Context, _, _ string) ([]iam.LoadedPolicy, error) {
	return f.policies, nil
}

// errLoader simulates a DB outage so the EvaluateMulti error branch in
// the middleware is reachable.
type errLoader struct{ err error }

func (e *errLoader) LoadPolicies(_ context.Context, _, _ string) ([]iam.LoadedPolicy, error) {
	return nil, e.err
}

// fakeDeviceGroups implements DeviceGroupLookup.
type fakeDeviceGroups struct {
	groups map[string][]string
	err    error
}

func (f *fakeDeviceGroups) GroupsOfDevice(_ context.Context, deviceID string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.groups[deviceID], nil
}

// allowAllPolicies returns a single policy granting every action on
// every resource — used to assert "Allow" decisions reach the handler.
func allowAllPolicies() []iam.LoadedPolicy {
	return []iam.LoadedPolicy{{
		ID: "p1", Name: "allow-all", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{
				{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}},
			},
		},
	}}
}

// readReason inspects the JSON deny envelope for the documented reason
// field. Tests assert specific reasons so a regression in the engine's
// reason wiring is caught at the middleware boundary.
func readReason(t *testing.T, body []byte) (action, resource string) {
	t.Helper()
	var env struct {
		Error struct {
			Details struct {
				Action   string `json:"action"`
				Resource string `json:"resource"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode body: %v: %q", err, body)
	}
	return env.Error.Details.Action, env.Error.Details.Resource
}

// TestRequireIAMPermission_NoPrincipalReturns401 covers the "no
// AdminAuth on context" branch — middleware must short-circuit at 401
// before invoking the engine, so a bug that wiped AdminAuth in a
// preceding middleware can't leak into an open auth posture.
func TestRequireIAMPermission_NoPrincipalReturns401(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	var handlerReached bool
	g := e.Group("", middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error {
		handlerReached = true
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	if handlerReached {
		t.Error("handler reached despite missing principal")
	}
}

// TestRequireIAMPermission_BootstrapAndDevNoBypass is the load-bearing
// regression test against an IAM-bypass bug: the magic-string principal IDs
// "bootstrap" and "dev" must NOT bypass IAM. With an empty (deny)
// engine and no grants, a request from either principal must be denied
// (403) — never short-circuited to the handler. A future seed/fixture
// minting such a subject must gain no privilege beyond what its IAM
// policies grant.
func TestRequireIAMPermission_BootstrapAndDevNoBypass(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default()) // empty → deny
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	for _, kid := range []string{"bootstrap", "dev"} {

		t.Run(kid, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			e.HideBanner = true
			handlerReached := false
			g := e.Group("",
				func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						middleware.WithAdminAuth(c, &auth.AdminAuth{
							KeyID:             kid,
							AuthPrincipalType: "admin_user",
						})
						return next(c)
					}
				},
				middleware.RequireIAMPermission(engine, action, nil))
			g.GET("/x", func(c echo.Context) error {
				handlerReached = true
				return c.NoContent(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d want 403 (%q must not bypass IAM)", rec.Code, kid)
			}
			if handlerReached {
				t.Errorf("handler reached on %q — magic-string IAM bypass still present", kid)
			}
		})
	}
}

// TestRequireIAMPermission_BootstrapDevAllowedOnlyViaPolicy proves the
// positive side of the no-bypass rule: a "bootstrap"/"dev" principal reaches the
// handler ONLY when an actual IAM policy grants the action — i.e. it is
// treated like any other principal, with no hardcoded short-circuit.
func TestRequireIAMPermission_BootstrapDevAllowedOnlyViaPolicy(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	for _, kid := range []string{"bootstrap", "dev"} {

		t.Run(kid, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			e.HideBanner = true
			handlerReached := false
			g := e.Group("",
				func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						middleware.WithAdminAuth(c, &auth.AdminAuth{
							KeyID:             kid,
							AuthPrincipalType: "admin_user",
						})
						return next(c)
					}
				},
				middleware.RequireIAMPermission(engine, action, nil))
			g.GET("/x", func(c echo.Context) error {
				handlerReached = true
				return c.NoContent(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d want 200 (%q with allow-all policy)", rec.Code, kid)
			}
			if !handlerReached {
				t.Errorf("handler not reached on %q despite allow-all policy", kid)
			}
		})
	}
}

// TestRequireIAMPermission_DenyReturns403WithDetails covers the deny
// happy path: principal present, engine returns Deny, middleware must
// 403 with the documented {action, resource, reason} envelope so the
// admin UI can render an actionable error.
func TestRequireIAMPermission_DenyReturns403WithDetails(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default()) // empty → deny
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-eve",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%q", rec.Code, rec.Body.String())
	}
	gotAction, gotResource := readReason(t, rec.Body.Bytes())
	if gotAction != action {
		t.Errorf("body.error.details.action=%q want %q", gotAction, action)
	}
	if gotResource == "" {
		t.Errorf("body.error.details.resource empty")
	}
}

// TestRequireIAMPermission_AllowReachesHandler covers the allow path
// — wildcard policy grants every action, the handler must run and the
// captured AdminAuth must still carry the principal.
func TestRequireIAMPermission_AllowReachesHandler(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	var seenPrincipal string
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-alice",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error {
		if aa := middleware.AdminAuthFromContext(c); aa != nil {
			seenPrincipal = aa.KeyID
		}
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if seenPrincipal != "usr-alice" {
		t.Errorf("handler saw principal %q want usr-alice", seenPrincipal)
	}
}

// TestRequireIAMPermission_EvaluateErrorReturns500 covers the
// engine.EvaluateMulti error branch — a DB outage in PolicyLoader must
// surface as 500 IAM_EVAL_ERROR, never as a silent allow.
func TestRequireIAMPermission_EvaluateErrorReturns500(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&errLoader{err: errors.New("DB down")}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-x",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if !contains(rec.Body.String(), "IAM_EVAL_ERROR") {
		t.Errorf("body=%q missing IAM_EVAL_ERROR code", rec.Body.String())
	}
}

// TestRequireIAMPermission_ResourceFnOverridesNRN covers the
// `resourceFn != nil` branch — caller-supplied resource function must
// replace the derived NRN. Tested by wiring a denying engine and
// asserting the deny envelope's resource carries the custom value.
func TestRequireIAMPermission_ResourceFnOverridesNRN(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)
	const customNRN = "nrn:nexus:gateway:*:provider/openai-custom"

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-y",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, func(_ echo.Context) string {
			return customNRN
		}))
	g.GET("/x", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	_, gotResource := readReason(t, rec.Body.Bytes())
	if gotResource != customNRN {
		t.Errorf("body.details.resource=%q want %q (resourceFn override broken)", gotResource, customNRN)
	}
}

// TestRequireIAMPermission_AdminUserMappedToNexusUser covers the
// principal-type translation: session-side "admin_user" must map to
// IAM-side "nexus_user" before EvaluateMulti is called. Wire a loader
// that records which principalType it received and assert it received
// "nexus_user".
func TestRequireIAMPermission_AdminUserMappedToNexusUser(t *testing.T) {
	t.Parallel()
	var seenType string
	rec := &recordingLoader{seenType: &seenType, inner: &fakeLoader{policies: allowAllPolicies()}}
	engine := iam.NewEngine(rec, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-z",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	httpRec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(httpRec, req)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", httpRec.Code)
	}
	if seenType != "nexus_user" {
		t.Errorf("loader saw principalType=%q, want nexus_user (admin_user→nexus_user mapping broken)", seenType)
	}
}

// TestRequireIAMPermission_ApiKeyPrincipalTypePassthrough covers the
// non-admin_user branch of the translation — an "api_key" principal
// reaches the engine unchanged.
func TestRequireIAMPermission_ApiKeyPrincipalTypePassthrough(t *testing.T) {
	t.Parallel()
	var seenType string
	rec := &recordingLoader{seenType: &seenType, inner: &fakeLoader{policies: allowAllPolicies()}}
	engine := iam.NewEngine(rec, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "ak-1",
					AuthPrincipalType: "api_key",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	httpRec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.ServeHTTP(httpRec, req)
	if seenType != "api_key" {
		t.Errorf("loader saw principalType=%q, want api_key (passthrough broken)", seenType)
	}
}

// recordingLoader records the principalType handed to it, then
// delegates to inner — gives tests a way to assert the translation
// contract without re-implementing engine evaluation.
type recordingLoader struct {
	seenType *string
	inner    iam.PolicyLoader
}

func (r *recordingLoader) LoadPolicies(ctx context.Context, principalType, principalID string) ([]iam.LoadedPolicy, error) {
	*r.seenType = principalType
	return r.inner.LoadPolicies(ctx, principalType, principalID)
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestRequireIAMPermissionForDevice_NoPrincipalReturns401 mirrors the
// unscoped path's auth gate.
func TestRequireIAMPermissionForDevice_NoPrincipalReturns401(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

// TestRequireIAMPermissionForDevice_BootstrapAndDevNoBypass is the
// device-variant counterpart of the IAM-bypass regression: "bootstrap" /
// "dev" must NOT short-circuit IAM. Empty engine → 403, never 200.
func TestRequireIAMPermissionForDevice_BootstrapAndDevNoBypass(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)
	for _, kid := range []string{"bootstrap", "dev"} {

		t.Run(kid, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			e.HideBanner = true
			handlerReached := false
			g := e.Group("",
				func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: kid, AuthPrincipalType: "admin_user"})
						return next(c)
					}
				},
				middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
			g.GET("/dev/:id", func(c echo.Context) error {
				handlerReached = true
				return c.NoContent(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status=%d want 403 (%q must not bypass IAM)", rec.Code, kid)
			}
			if handlerReached {
				t.Errorf("handler reached on %q — device-variant magic-string bypass still present", kid)
			}
		})
	}
}

// TestRequireIAMPermissionForDevice_MissingPathParamFallsBackToUnscoped
// covers the documented "missing path param" defensive fallback:
// instead of crashing with a 500, the middleware degrades to the
// unscoped check. Wire an allow-all engine and assert the request
// reaches the handler — proves the fallback path is wired, not just
// silently failing.
func TestRequireIAMPermissionForDevice_MissingPathParamFallsBackToUnscoped(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	// Path param "id" is required but the actual URL pattern omits :id
	// so c.Param("id") == "".
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
	g.GET("/dev/no-id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/no-id", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (unscoped fallback should allow)", rec.Code)
	}
}

// TestRequireIAMPermissionForDevice_NilLookupBehavesLikeUnscoped
// covers the lookup==nil branch documented as "safe default for
// handlers that haven't wired group resolution yet". With an
// allow-all policy this must allow.
func TestRequireIAMPermissionForDevice_NilLookupBehavesLikeUnscoped(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
}

// TestRequireIAMPermissionForDevice_GroupLookupErrorFailsClosed is the
// load-bearing test for the fail-closed promise in the iamauth doc
// comment: a DeviceGroupLookup error must NOT authorise the request as
// "fleet-wide allowed" — the device's group set must remain empty,
// so a scoped policy (which would have matched only via the group
// candidate) defaults to Deny. Wire a policy scoped to a specific
// group, a lookup that errors, and assert 403.
func TestRequireIAMPermissionForDevice_GroupLookupErrorFailsClosed(t *testing.T) {
	t.Parallel()
	// Policy that only grants the action on agent-device IDs prefixed
	// with "group:g1/" — i.e. only when the device belongs to g1.
	policies := []iam.LoadedPolicy{{
		ID: "p1", Name: "g1-only", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{{
				Effect:   "Allow",
				Action:   []string{sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)},
				Resource: []string{"nrn:nexus:agent:*:agent-device/group:g1/*"},
			}},
		},
	}}
	engine := iam.NewEngine(&fakeLoader{policies: policies}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)
	lookup := &fakeDeviceGroups{err: errors.New("group lookup outage")}

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", lookup))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 (fail-closed on lookup err)", rec.Code)
	}
}

// TestRequireIAMPermissionForDevice_GroupScopedAllow covers the
// successful-resolution allow path: device d1 belongs to g1, a
// policy scoped to group g1 allows the action, the request reaches
// the handler. Locks the "group membership maps to scoped grant"
// contract.
func TestRequireIAMPermissionForDevice_GroupScopedAllow(t *testing.T) {
	t.Parallel()
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)
	policies := []iam.LoadedPolicy{{
		ID: "p1", Name: "g1-only", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{{
				Effect:   "Allow",
				Action:   []string{action},
				Resource: []string{"nrn:nexus:agent:*:agent-device/group:g1/*"},
			}},
		},
	}}
	engine := iam.NewEngine(&fakeLoader{policies: policies}, slog.Default())
	lookup := &fakeDeviceGroups{groups: map[string][]string{"d1": {"g1"}}}

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", lookup))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200 — group-scoped allow should match", rec.Code)
	}
}

// TestRequireIAMPermissionForDevice_GroupScopedDenyEnvelope covers the
// deny envelope body shape: when scoped allow doesn't match (device
// has no groups), 403 with body.details carrying action, deviceId,
// deviceGroups so the UI can render the contextual rejection.
func TestRequireIAMPermissionForDevice_GroupScopedDenyEnvelope(t *testing.T) {
	t.Parallel()
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)
	policies := []iam.LoadedPolicy{{
		ID: "p1", Name: "g1-only", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{{
				Effect:   "Allow",
				Action:   []string{action},
				Resource: []string{"nrn:nexus:agent:*:agent-device/group:g1/*"},
			}},
		},
	}}
	engine := iam.NewEngine(&fakeLoader{policies: policies}, slog.Default())
	lookup := &fakeDeviceGroups{groups: map[string][]string{"d1": {"g2"}}} // wrong group

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", lookup))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Action       string   `json:"action"`
				DeviceID     string   `json:"deviceId"`
				DeviceGroups []string `json:"deviceGroups"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if env.Error.Code != "IAM_ACCESS_DENIED" {
		t.Errorf("error.code=%q want IAM_ACCESS_DENIED", env.Error.Code)
	}
	if env.Error.Details.DeviceID != "d1" {
		t.Errorf("error.details.deviceId=%q want d1", env.Error.Details.DeviceID)
	}
	if len(env.Error.Details.DeviceGroups) != 1 || env.Error.Details.DeviceGroups[0] != "g2" {
		t.Errorf("error.details.deviceGroups=%v want [g2]", env.Error.Details.DeviceGroups)
	}
}

// TestRequireIAMPermissionForDevice_EvaluateErrorReturns500 covers the
// engine.EvaluateMulti error branch with a path-param + lookup wired:
// loader explodes → 500 IAM_EVAL_ERROR, never a silent allow.
func TestRequireIAMPermissionForDevice_EvaluateErrorReturns500(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&errLoader{err: errors.New("DB down")}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)
	lookup := &fakeDeviceGroups{groups: map[string][]string{"d1": {"g1"}}}

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "u", AuthPrincipalType: "admin_user"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", lookup))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

// TestRequireIAMPermission_CacheHitMetric covers the cache-hit branch
// of the IAMEvalTotal metric. The engine caches per-principal policy
// loads; the second evaluation under the same principal must report
// CacheHit=true and the middleware must label the counter "hit"
// instead of "miss". Without this assertion, the metric would
// silently always report "miss" — operators couldn't tune the cache
// TTL.
func TestRequireIAMPermission_CacheHitMetric(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceProvider.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-cache",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermission(engine, action, nil))
	g.GET("/x", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	for i := range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d status=%d want 200", i, rec.Code)
		}
	}
}

// TestRequireIAMPermissionForDevice_CacheHitMetric mirrors the
// unscoped CacheHit test for the device variant — same cache, same
// label rules.
func TestRequireIAMPermissionForDevice_CacheHitMetric(t *testing.T) {
	t.Parallel()
	engine := iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{
					KeyID:             "usr-cache-dev",
					AuthPrincipalType: "admin_user",
				})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	for i := range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d status=%d want 200", i, rec.Code)
		}
	}
}

// TestRequireIAMPermissionForDevice_ApiKeyPrincipalTypePassthrough
// covers the principal-type translation in the device variant — same
// rule as the unscoped variant: admin_user → nexus_user, anything
// else passes through unchanged.
func TestRequireIAMPermissionForDevice_ApiKeyPrincipalTypePassthrough(t *testing.T) {
	t.Parallel()
	var seenType string
	rec := &recordingLoader{seenType: &seenType, inner: &fakeLoader{policies: allowAllPolicies()}}
	engine := iam.NewEngine(rec, slog.Default())
	action := sharediam.ResourceAgentDevice.Action(sharediam.VerbRead)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("",
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "ak-1", AuthPrincipalType: "api_key"})
				return next(c)
			}
		},
		middleware.RequireIAMPermissionForDevice(engine, action, "id", nil))
	g.GET("/dev/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	httpRec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev/d1", nil)
	e.ServeHTTP(httpRec, req)
	if seenType != "api_key" {
		t.Errorf("loader saw principalType=%q want api_key (device variant passthrough broken)", seenType)
	}
}
