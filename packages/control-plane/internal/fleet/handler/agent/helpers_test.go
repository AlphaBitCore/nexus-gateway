// Shared test plumbing for the agent admin handler package. Provides
// the pgxmock-backed *store.DB, an audit mq.Producer spy, the Echo
// context builders, a stub Hub, and the column lists / row builders
// that mirror the store projections used by handler tests.
package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// silentLogger discards output so error-path tests don't spam stderr.
// Tests can set NEXUS_AGENT_TEST_LOG=1 to surface handler errors on
// stderr while diagnosing failures.
func silentLogger() *slog.Logger {
	if os.Getenv("NEXUS_AGENT_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// auditSpy implements mq.Producer (Publish/Enqueue/Close).
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	a.calls = append(a.calls, cp)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *auditSpy) last() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(a.calls[len(a.calls)-1], &m)
	return m
}

// newAuditWriter returns a real Writer using the auditSpy producer so
// JSON marshalling executes end-to-end.
func newAuditWriter(spy *auditSpy) *audit.Writer {
	return audit.NewWriter(spy, "audit", silentLogger())
}

// fakeHub captures every Hub call so handler tests can assert on
// fan-out. Each method has a programmable error / response so tests
// can drive both happy + degraded paths.
type fakeHub struct {
	mu sync.Mutex

	notifyHits int
	notifyReq  hub.ConfigChangeRequest
	notifyResp *hub.ConfigChangeResponse
	notifyErr  error

	invalidateHits []string // "thingType/configKey"

	createTokenHits int
	createTokenReq  hub.CreateEnrollmentTokenRequest
	createTokenResp *hub.CreateEnrollmentTokenResponse
	createTokenErr  error

	forceResyncHits int
	forceResyncReq  string
	forceResyncResp map[string]any
	forceResyncErr  error
}

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyHits++
	f.notifyReq = req
	return f.notifyResp, f.notifyErr
}

func (f *fakeHub) InvalidateConfig(_ context.Context, thingType, configKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidateHits = append(f.invalidateHits, thingType+"/"+configKey)
}

func (f *fakeHub) CreateEnrollmentToken(_ context.Context, req hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createTokenHits++
	f.createTokenReq = req
	return f.createTokenResp, f.createTokenErr
}

func (f *fakeHub) ForceResyncAll(_ context.Context, thingID string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceResyncHits++
	f.forceResyncReq = thingID
	return f.forceResyncResp, f.forceResyncErr
}

// newMockPool returns a pgxmock pool for use in handler tests.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock
}

// newHandlerForTest constructs an agent.Handler with the supplied mock
// pool, hub, and audit spy. The pgxmock satisfies rawPool (agentstore.PgxPool)
// so all sub-store SQL expectations work without a live database.
func newHandlerForTest(mock pgxmock.PgxPoolIface, hub HubAPI, spy *auditSpy) *Handler {
	d := Deps{Pool: mock, Hub: hub, Logger: silentLogger()}
	if spy != nil {
		d.Audit = newAuditWriter(spy)
	} else {
		d.Audit = newAuditWriter(&auditSpy{})
	}
	return New(d)
}

// echoCtxAdmin builds an Echo context with an authenticated admin
// attached so the audit middleware extraction works end-to-end.
func echoCtxAdmin(req *http.Request, rec *httptest.ResponseRecorder, userID string) (echo.Context, *echo.Echo) {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           "admin-" + userID,
		AuthPrincipalType: "admin_user",
	})
	return c, e
}

// withAdminAuthMW wraps a handler so an AdminAuth principal is
// attached before the handler runs. Useful for routes mounted on an
// Echo group without a real auth middleware.
func withAdminAuthMW(userID string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			middleware.WithAdminAuth(c, &auth.AdminAuth{
				KeyID:             userID,
				KeyName:           "admin-" + userID,
				AuthPrincipalType: "admin_user",
			})
			return next(c)
		}
	}
}

// nowFixture returns a stable UTC timestamp suitable for fixture rows.
func nowFixture() time.Time { return time.Now().UTC().Truncate(time.Second) }

// column lists + row builders mirroring store projections

// agentDeviceCols mirrors agentJoinColumns. ListThingNodes appends
// event_count; GetThingNode uses these unchanged.
var agentDeviceCols = []string{
	"id", "hostname", "os", "os_version", "version",
	"status", "last_seen_at", "enrolled_at", "enrolled_by",
	"cert_serial", "cert_expires_at", "metadata", "sysinfo",
	"primary_ip", "physical_id",
	"bound_user_id", "bound_user_display_name", "bound_user_email",
	"tags",
}

var agentDeviceListCols = append(append([]string{}, agentDeviceCols...), "event_count")

func makeAgentDeviceRow(now time.Time) []any {
	lastSeen := now.Add(-time.Minute)
	certSerial := "AA:BB:CC"
	certExpires := now.Add(365 * 24 * time.Hour)
	return []any{
		"agent-1", "macbook-pro", "darwin", "26.3.0", "1.2.3",
		"online", &lastSeen, now, "admin@nexus.com",
		&certSerial, &certExpires, json.RawMessage(`{}`), json.RawMessage(`{}`),
		"10.0.0.10", "AB-CD-EF",
		"u-1", "Alice", "alice@nexus.com",
		[]string{"vip"},
	}
}

// nexusUserCols mirrors nexusUserColumns for FindNexusUserByID.
var nexusUserCols = []string{
	"id", "organizationId", "displayName", "email", "status",
	"canAccessControlPlane", "source", "osUsername", "osDomain",
	"passwordHash", "lastLoginAt", "preferredTimezone", "createdAt", "updatedAt",
}

func makeAgentUserRow(now time.Time) []any {
	email := "alice@example.com"
	osu := "alice"
	osd := "CORP"
	pw := "$2a$10$abc"
	tz := "UTC"
	return []any{
		"u-1", "org-1", "Alice", &email, "active",
		false, "local", &osu, &osd,
		&pw, &now, &tz, now, now,
	}
}

func makeAdminUserRow(now time.Time) []any {
	// canAccessControlPlane = true → "user not found" in agent-user paths.
	email := "admin@nexus.com"
	row := makeAgentUserRow(now)
	row[3] = &email
	row[5] = true
	return row
}

// nexusUserSafeListCols mirrors the ListNexusUsers projection (10 user
// cols + organizationId + organization name).
var nexusUserSafeListCols = []string{
	"id", "displayName", "email", "status", "canAccessControlPlane",
	"source", "lastLoginAt", "preferredTimezone", "createdAt", "updatedAt",
	"organizationId", "organizationName",
}

func makeAgentUserSafeRow(now time.Time) []any {
	email := "alice@example.com"
	tz := "UTC"
	orgID := "org-1"
	orgName := "Acme"
	return []any{
		"u-1", "Alice", &email, "active", false,
		"local", &now, &tz, now, now,
		&orgID, &orgName,
	}
}

// nexusUserSafeCols is the projection UpdateNexusUser returns
// (RETURNING nexusUserSafeColumns).
var nexusUserSafeCols = []string{
	"id", "displayName", "email", "status", "canAccessControlPlane",
	"source", "lastLoginAt", "preferredTimezone", "createdAt", "updatedAt",
}

func makeAgentUserUpdateRow(now time.Time, status string) []any {
	email := "alice@example.com"
	tz := "UTC"
	return []any{
		"u-1", "Alice", &email, status, false,
		"local", &now, &tz, now, now,
	}
}

// fleetUserDeviceCols mirrors ListDevicesByUserID's projection.
var fleetUserDeviceCols = []string{
	"id", "hostname", "os", "os_version", "version", "status",
	"last_seen_at", "assignedAt", "source",
}

func makeFleetUserDeviceRow(now time.Time) []any {
	lastSeen := now.Add(-time.Minute)
	return []any{
		"agent-1", "macbook", "darwin", "26.3.0", "1.2.3", "online",
		&lastSeen, now, "enrollment",
	}
}

// auditEventCols mirrors ListAuditEventsBy*'s projection.
var auditEventCols = []string{
	"id", "source", "timestamp", "target_host", "latency_ms",
	"entity_id", "entity_type", "request_hook_decision", "details",
}

func makeAuditEventRow(now time.Time) []any {
	host := "api.openai.com"
	lat := 142
	eid := "vk-1"
	etype := "virtual_key"
	dec := "allow"
	return []any{
		"ev-1", "ai-gateway", now, &host, &lat,
		&eid, &etype, &dec, json.RawMessage(`{}`),
	}
}

// agentTrafficEventCols mirrors ListAgentTrafficEvents's projection.
var agentTrafficEventCols = []string{
	"id", "thing_id", "timestamp", "source_process", "source_user",
	"entity_id", "target_host", "dest_ip", "dest_port", "action",
	"policy_rule_id", "bump_status", "bytes_in", "bytes_out",
	"latency_ms", "request_hook_decision", "created_at",
	"hostname", "os",
}

func makeAgentTrafficEventRow(now time.Time) []any {
	user := "alice"
	subj := "vk-1"
	rule := "rule-1"
	bump := "ok"
	bin := 100
	bout := 500
	dur := 42
	dec := "allow"
	host := "macbook"
	osStr := "darwin"
	return []any{
		"ev-1", "agent-1", now, "curl", &user,
		&subj, "api.openai.com", "10.0.0.1", 443, "egress",
		&rule, &bump, &bin, &bout,
		&dur, &dec, now,
		&host, &osStr,
	}
}

// deviceGroupCols mirrors dgColumns for GetDeviceGroup / Create / Update.
var deviceGroupCols = []string{
	"id", "name", "description", "createdBy", "createdAt", "updatedAt",
}

func makeDeviceGroupRow(now time.Time) []any {
	desc := "engineering laptops"
	createdBy := "u-admin"
	return []any{
		"grp-1", "Engineering", &desc, &createdBy, now, now,
	}
}

// deviceGroupListCols extends deviceGroupCols with member_count.
var deviceGroupListCols = append(append([]string{}, deviceGroupCols...), "member_count")

func makeDeviceGroupListRow(now time.Time) []any {
	return append(makeDeviceGroupRow(now), 3)
}

// deviceGroupMembershipDetailCols mirrors ListDeviceGroupMemberships.
var deviceGroupMembershipDetailCols = []string{
	"id", "groupId", "deviceId", "createdAt", "expires_at",
	"device_id", "device_status", "device_hostname", "device_os",
}

func makeDeviceGroupMembershipRow(now time.Time) []any {
	return []any{
		"m-1", "grp-1", "agent-1", now, (*time.Time)(nil),
		"agent-1", "online", "macbook", "darwin",
	}
}

// deviceAssignmentCols mirrors ListDeviceAssignmentsByDevice +
// ListDeviceAssignments.
var deviceAssignmentCols = []string{
	"id", "deviceId", "userId", "assignedAt", "releasedAt", "source",
	"login_method", "ip_address", "token_jti",
	"displayName", "osUsername", "osDomain",
}

func makeDeviceAssignmentRow(now time.Time) []any {
	loginMethod := "sso"
	ip := "10.0.0.1"
	jti := "jti-1"
	display := "Alice"
	osu := "alice"
	osd := "CORP"
	return []any{
		"a-1", "agent-1", "u-1", now, (*time.Time)(nil), "enrollment",
		&loginMethod, &ip, &jti,
		&display, &osu, &osd,
	}
}

// providerListCols mirrors ListProviders's data-query projection.
var providerListCols = []string{
	"id", "name", "displayName", "description", "adapter_type", "baseUrl",
	"pathPrefix", "apiVersion", "region", "enabled", "headers",
	"createdAt", "updatedAt", "model_count",
}

func makeProviderRow(now time.Time) []any {
	dn := "OpenAI"
	desc := "primary provider"
	apiVer := "2025-01-01"
	region := "us-east-1"
	return []any{
		"prov-1", "openai", &dn, &desc, "openai", "https://api.openai.com",
		"/v1", &apiVer, &region, true, json.RawMessage(`{}`),
		now, now, 5,
	}
}
