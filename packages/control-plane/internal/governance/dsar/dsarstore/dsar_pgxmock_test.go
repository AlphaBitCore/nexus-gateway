package dsarstore

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func sp(s string) *string   { return &s }
func ip(i int) *int         { return &i }
func fp(f float64) *float64 { return &f }

var dsarCols = []string{"id", "subjectId", "contact", "type", "status", "notes", "completedAt", "outcome", "createdAt", "createdBy", "updatedAt", "updatedBy"}

func dsarRow(id string) []any {
	return []any{id, "subj1", sp("a@x.com"), "ACCESS", "PENDING", sp("n"), (*time.Time)(nil), json.RawMessage(`{}`), tNow, "admin", tNow, (*string)(nil)}
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

func TestListDSARRequests(t *testing.T) {
	s, m := newMock(t)
	// status filter present → 1 filter arg; limit defaults nothing (passed 10).
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM dsar_request`).WithArgs("PENDING").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m.ExpectQuery(`FROM dsar_request`).WithArgs("PENDING", 10, 0).
		WillReturnRows(pgxmock.NewRows(dsarCols).AddRow(dsarRow("d1")...))
	reqs, total, err := s.ListDSARRequests(context.Background(), "PENDING", 10, 0)
	if err != nil || total != 1 || len(reqs) != 1 || reqs[0].ID != "d1" || reqs[0].Type != "ACCESS" {
		t.Fatalf("ListDSARRequests: %+v total=%d err=%v", reqs, total, err)
	}
	// limit<=0 defaults to 20 (no status filter → 0 filter args, then 20,0).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	m2.ExpectQuery(`FROM dsar_request`).WithArgs(20, 0).WillReturnRows(pgxmock.NewRows(dsarCols))
	if _, _, err := s2.ListDSARRequests(context.Background(), "", 0, 0); err != nil {
		t.Fatalf("limit default: %v", err)
	}
	if err := m2.ExpectationsWereMet(); err != nil {
		t.Fatalf("limit default 20 not applied: %v", err)
	}
}

func TestListDSARRequests_Errors(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnError(errors.New("boom"))
	if _, _, err := s.ListDSARRequests(context.Background(), "", 10, 0); err == nil {
		t.Fatal("count error should surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m2.ExpectQuery(`FROM dsar_request`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("q"))
	if _, _, err := s2.ListDSARRequests(context.Background(), "", 10, 0); err == nil {
		t.Fatal("data query error should surface")
	}
	s3, m3 := newMock(t)
	bad := dsarRow("d1")
	bad[8] = "not-a-time"
	m3.ExpectQuery(`SELECT COUNT`).WithArgs().WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	m3.ExpectQuery(`FROM dsar_request`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(dsarCols).AddRow(bad...))
	if _, _, err := s3.ListDSARRequests(context.Background(), "", 10, 0); err == nil {
		t.Fatal("scan error should surface")
	}
}

func TestGetCreateUpdateDSARRequest(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM dsar_request WHERE id = \$1`).WithArgs("d1").
		WillReturnRows(pgxmock.NewRows(dsarCols).AddRow(dsarRow("d1")...))
	if d, err := s.GetDSARRequest(context.Background(), "d1"); err != nil || d == nil || d.ID != "d1" {
		t.Fatalf("GetDSARRequest: %+v %v", d, err)
	}
	m.ExpectQuery(`FROM dsar_request WHERE id`).WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	if d, err := s.GetDSARRequest(context.Background(), "missing"); err != nil || d != nil {
		t.Fatalf("missing → (nil,nil), got %+v %v", d, err)
	}
	m.ExpectQuery(`INSERT INTO dsar_request`).WithArgs("subj1", sp("a@x.com"), "ACCESS", sp("n"), "admin").
		WillReturnRows(pgxmock.NewRows(dsarCols).AddRow(dsarRow("d1")...))
	if d, err := s.CreateDSARRequest(context.Background(), CreateDSARRequestParams{SubjectID: "subj1", Contact: sp("a@x.com"), Type: "ACCESS", Notes: sp("n"), CreatedBy: "admin"}); err != nil || d == nil {
		t.Fatalf("CreateDSARRequest: %+v %v", d, err)
	}
	m.ExpectQuery(`INSERT INTO dsar_request`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("boom"))
	if _, err := s.CreateDSARRequest(context.Background(), CreateDSARRequestParams{}); err == nil {
		t.Fatal("create error should surface")
	}
	m.ExpectQuery(`UPDATE dsar_request SET`).WithArgs(anyArgs(6)...).
		WillReturnRows(pgxmock.NewRows(dsarCols).AddRow(dsarRow("d1")...))
	if _, err := s.UpdateDSARRequest(context.Background(), "d1", UpdateDSARParams{Status: sp("COMPLETED")}); err != nil {
		t.Fatalf("UpdateDSARRequest: %v", err)
	}
	m.ExpectQuery(`UPDATE dsar_request`).WithArgs(anyArgs(6)...).WillReturnError(errors.New("boom"))
	if _, err := s.UpdateDSARRequest(context.Background(), "d1", UpdateDSARParams{}); err == nil {
		t.Fatal("update error should surface")
	}
}

var userCols = []string{"id", "organizationId", "displayName", "email", "status", "source", "osUsername", "osDomain", "preferredTimezone", "lastLoginAt", "createdAt", "updatedAt"}

func userRow() []any {
	return []any{"subj1", "org1", "Jane Doe", sp("jane@x.com"), "active", "local", sp("jane"), sp("CORP"), sp("UTC"), &tNow, tNow, tNow}
}

// expectAccessHappy queues the full 9-statement happy-path ACCESS aggregation.
func expectAccessHappy(m pgxmock.PgxPoolIface) {
	m.ExpectQuery(`FROM "NexusUser" WHERE id = \$1`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows(userCols).AddRow(userRow()...))
	m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"groupId", "groupName", "joinedAt"}).AddRow("g1", "Compliance", tNow))
	m.ExpectQuery(`source = 'ai-gateway' AND entity_id = \$1`).WithArgs("subj1", 10000).
		WillReturnRows(pgxmock.NewRows([]string{"id", "timestamp", "provider", "method", "path", "statusCode", "model", "cost", "pt", "ct"}).
			AddRow("e1", tNow, "openai", sp("POST"), sp("/v1"), ip(200), sp("gpt-4o"), fp(0.01), ip(10), ip(20)))
	m.ExpectQuery(`JOIN "DeviceAssignment" da ON da."deviceId" = t.thing_id`).WithArgs("subj1", 10000).
		WillReturnRows(pgxmock.NewRows([]string{"id", "timestamp", "thingId", "sourceProcess", "targetHost", "action", "hookDecision", "latencyMs"}).
			AddRow("a1", tNow, sp("dev1"), "curl", "api.x", sp("allow"), sp("pass"), ip(50)))
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"deviceId", "hostname", "assignedAt", "releasedAt"}).
			AddRow("dev1", "host1", tNow, (*time.Time)(nil)))
	m.ExpectQuery(`FROM traffic_event_payload p\s+JOIN traffic_event t`).WithArgs("subj1", 1000).
		WillReturnRows(pgxmock.NewRows([]string{"id", "source", "req", "resp", "reqEnc", "respEnc"}).
			AddRow("e1", "ai-gateway", []byte(`{"prompt":"hi"}`), []byte(`{"text":"yo"}`), "text", "text"))
	m.ExpectQuery(`FROM "AssistantSession"`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "msgCount", "createdAt", "updatedAt"}).
			AddRow("ses1", "Chat", 3, tNow, tNow))
	m.ExpectQuery(`FROM "AssistantMemory"`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body", "updatedAt"}).
			AddRow("pref", "note", "likes brevity", tNow))
	m.ExpectQuery(`FROM "AssistantFile"`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sessionId", "name", "size", "contentType", "createdAt"}).
			AddRow("f1", "ses1", "out.txt", 12, "text/plain", tNow))
}

func TestSubjectExists(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT EXISTS .*FROM "NexusUser" WHERE id = \$1`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	ok, err := s.SubjectExists(context.Background(), "subj1")
	if err != nil || !ok {
		t.Fatalf("SubjectExists existing = (%v,%v); want (true,nil)", ok, err)
	}
	m.ExpectQuery(`FROM "NexusUser" WHERE id = \$1`).WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	ok, err = s.SubjectExists(context.Background(), "ghost")
	if err != nil || ok {
		t.Fatalf("SubjectExists missing = (%v,%v); want (false,nil)", ok, err)
	}
	m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnError(errors.New("boom"))
	if _, err := s.SubjectExists(context.Background(), "subj1"); err == nil {
		t.Fatal("SubjectExists query error should surface")
	}
}

func TestFulfillDSARAccess(t *testing.T) {
	s, m := newMock(t)
	expectAccessHappy(m)
	exp, err := s.FulfillDSARAccess(context.Background(), "subj1")
	if err != nil {
		t.Fatalf("FulfillDSARAccess: %v", err)
	}
	// Art.15 export must now cover every personal-data surface, not just
	// VK/agent/device traffic.
	if exp.User == nil || exp.User["displayName"] != "Jane Doe" {
		t.Errorf("User record missing/incorrect: %+v", exp.User)
	}
	if len(exp.IAMGroups) != 1 || exp.IAMGroups[0]["groupName"] != "Compliance" {
		t.Errorf("IAMGroups: %+v", exp.IAMGroups)
	}
	if len(exp.VKRows) != 1 || len(exp.AgentRows) != 1 || len(exp.Devices) != 1 {
		t.Errorf("traffic/device counts: vk=%d agent=%d dev=%d", len(exp.VKRows), len(exp.AgentRows), len(exp.Devices))
	}
	if len(exp.Payloads) != 1 || exp.Payloads[0]["trafficEventId"] != "e1" {
		t.Errorf("Payloads (prompt/response bodies) missing: %+v", exp.Payloads)
	}
	if len(exp.Assistant.Sessions) != 1 || len(exp.Assistant.Memory) != 1 || len(exp.Assistant.Files) != 1 {
		t.Errorf("Assistant data missing: %+v", exp.Assistant)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("not all access queries ran: %v", err)
	}
}

func TestFulfillDSARAccess_UserMissing_LeavesUserNil(t *testing.T) {
	s, m := newMock(t)
	// User row vanished between the existence gate and the read → User stays nil,
	// the rest of the export still proceeds.
	m.ExpectQuery(`FROM "NexusUser" WHERE id = \$1`).WithArgs("subj1").WillReturnError(pgx.ErrNoRows)
	m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"groupId", "groupName", "joinedAt"}))
	m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"id", "timestamp", "provider", "method", "path", "statusCode", "model", "cost", "pt", "ct"}))
	m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"id", "timestamp", "thingId", "sourceProcess", "targetHost", "action", "hookDecision", "latencyMs"}))
	m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"deviceId", "hostname", "assignedAt", "releasedAt"}))
	m.ExpectQuery(`FROM traffic_event_payload p`).WithArgs("subj1", 1000).WillReturnRows(pgxmock.NewRows([]string{"id", "source", "req", "resp"}))
	m.ExpectQuery(`FROM "AssistantSession"`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"id", "title", "msgCount", "createdAt", "updatedAt"}))
	m.ExpectQuery(`FROM "AssistantMemory"`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body", "updatedAt"}))
	m.ExpectQuery(`FROM "AssistantFile"`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"id", "sessionId", "name", "size", "contentType", "createdAt"}))
	exp, err := s.FulfillDSARAccess(context.Background(), "subj1")
	if err != nil {
		t.Fatalf("FulfillDSARAccess: %v", err)
	}
	if exp.User != nil {
		t.Errorf("User should be nil when the row is gone, got %+v", exp.User)
	}
}

func TestFulfillDSARAccess_QueryErrors(t *testing.T) {
	// Each new surface's query error must surface. Build the prefix up to the
	// failing statement, then fail that one.
	emptyUser := func() *pgxmock.Rows { return pgxmock.NewRows(userCols).AddRow(userRow()...) }
	emptyGroups := func() *pgxmock.Rows { return pgxmock.NewRows([]string{"groupId", "groupName", "joinedAt"}) }
	emptyVK := func() *pgxmock.Rows {
		return pgxmock.NewRows([]string{"id", "timestamp", "provider", "method", "path", "statusCode", "model", "cost", "pt", "ct"})
	}
	emptyAgent := func() *pgxmock.Rows {
		return pgxmock.NewRows([]string{"id", "timestamp", "thingId", "sourceProcess", "targetHost", "action", "hookDecision", "latencyMs"})
	}
	emptyDev := func() *pgxmock.Rows {
		return pgxmock.NewRows([]string{"deviceId", "hostname", "assignedAt", "releasedAt"})
	}
	emptyPayload := func() *pgxmock.Rows { return pgxmock.NewRows([]string{"id", "source", "req", "resp"}) }
	emptySess := func() *pgxmock.Rows {
		return pgxmock.NewRows([]string{"id", "title", "msgCount", "createdAt", "updatedAt"})
	}
	emptyMem := func() *pgxmock.Rows { return pgxmock.NewRows([]string{"name", "type", "body", "updatedAt"}) }

	for _, tc := range []struct {
		name  string
		setup func(m pgxmock.PgxPoolIface)
	}{
		{"user", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnError(errors.New("user"))
		}},
		{"iam-groups", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnError(errors.New("iam"))
		}},
		{"vk", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("vk"))
		}},
		{"agent", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("agent"))
		}},
		{"device", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(emptyAgent())
			m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnError(errors.New("dev"))
		}},
		{"payload", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(emptyAgent())
			m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnRows(emptyDev())
			m.ExpectQuery(`FROM traffic_event_payload p`).WithArgs("subj1", 1000).WillReturnError(errors.New("payload"))
		}},
		{"assistant-sessions", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(emptyAgent())
			m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnRows(emptyDev())
			m.ExpectQuery(`FROM traffic_event_payload p`).WithArgs("subj1", 1000).WillReturnRows(emptyPayload())
			m.ExpectQuery(`FROM "AssistantSession"`).WithArgs("subj1").WillReturnError(errors.New("sessions"))
		}},
		{"assistant-memory", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(emptyAgent())
			m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnRows(emptyDev())
			m.ExpectQuery(`FROM traffic_event_payload p`).WithArgs("subj1", 1000).WillReturnRows(emptyPayload())
			m.ExpectQuery(`FROM "AssistantSession"`).WithArgs("subj1").WillReturnRows(emptySess())
			m.ExpectQuery(`FROM "AssistantMemory"`).WithArgs("subj1").WillReturnError(errors.New("memory"))
		}},
		{"assistant-files", func(m pgxmock.PgxPoolIface) {
			m.ExpectQuery(`FROM "NexusUser"`).WithArgs("subj1").WillReturnRows(emptyUser())
			m.ExpectQuery(`FROM "IamGroupMembership"`).WithArgs("subj1").WillReturnRows(emptyGroups())
			m.ExpectQuery(`source = 'ai-gateway'`).WithArgs(anyArgs(2)...).WillReturnRows(emptyVK())
			m.ExpectQuery(`JOIN "DeviceAssignment" da ON`).WithArgs(anyArgs(2)...).WillReturnRows(emptyAgent())
			m.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("subj1").WillReturnRows(emptyDev())
			m.ExpectQuery(`FROM traffic_event_payload p`).WithArgs("subj1", 1000).WillReturnRows(emptyPayload())
			m.ExpectQuery(`FROM "AssistantSession"`).WithArgs("subj1").WillReturnRows(emptySess())
			m.ExpectQuery(`FROM "AssistantMemory"`).WithArgs("subj1").WillReturnRows(emptyMem())
			m.ExpectQuery(`FROM "AssistantFile"`).WithArgs("subj1").WillReturnError(errors.New("files"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMock(t)
			tc.setup(m)
			if _, err := s.FulfillDSARAccess(context.Background(), "subj1"); err == nil {
				t.Fatalf("%s query error should surface", tc.name)
			}
		})
	}
}

// expectErasureHappy sets up the full 7-statement erasure transaction on m.
func expectErasureHappy(m pgxmock.PgxPoolIface) {
	m.ExpectBegin()
	m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
	m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 4))
	m.ExpectExec(`UPDATE traffic_event_normalized`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 5))
	m.ExpectExec(`UPDATE traffic_event\s+SET entity_id = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	m.ExpectExec(`UPDATE traffic_event t\s+SET source_ip = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	m.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectExec(`DELETE FROM "AssistantSession"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m.ExpectExec(`DELETE FROM "AssistantFile"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 3))
	m.ExpectExec(`DELETE FROM "AssistantPendingConfirm"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 4))
	m.ExpectExec(`DELETE FROM "AssistantChatEvent"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m.ExpectExec(`UPDATE dsar_request\s+SET outcome = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// Account-record deletion stage: owned keys, identity/credential
	// rows, then the NexusUser itself. The admin-audit hash chain is NOT touched
	// — there is deliberately NO DELETE on AdminAuditLog queued, and pgxmock's
	// ExpectationsWereMet would fail if the code issued one.
	m.ExpectExec(`DELETE FROM "VirtualKey" WHERE "ownerId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m.ExpectExec(`DELETE FROM "AdminApiKey" WHERE "ownerUserId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectExec(`DELETE FROM "UserFederatedIdentity" WHERE "userId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 3))
	m.ExpectExec(`DELETE FROM "RefreshToken" WHERE "userId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 4))
	m.ExpectExec(`DELETE FROM "ScimToken" WHERE "createdBy" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectExec(`DELETE FROM "IamGroupMembership" WHERE "principalType" = 'admin_user' AND "principalId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 2))
	m.ExpectExec(`DELETE FROM "IamPolicyAttachment" WHERE "principalType" = 'admin_user' AND "principalId" = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectExec(`DELETE FROM "NexusUser" WHERE id = \$1`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
}

func TestFulfillDSARErasure(t *testing.T) {
	s, m := newMock(t)
	expectErasureHappy(m)
	m.ExpectCommit()
	res, err := s.FulfillDSARErasure(context.Background(), "subj1")
	if err != nil {
		t.Fatalf("FulfillDSARErasure: %v", err)
	}
	// Full Art.17 erasure: traffic identifiers anonymised, payload bodies +
	// spill refs scrubbed, assistant data deleted, orphaned spill refs counted.
	if res.VKAnonymised != 3 || res.AgentAnonymised != 2 || res.TotalAnonymised != 5 {
		t.Errorf("anonymise counts: %+v", res)
	}
	if res.PayloadsScrubbed != 4 {
		t.Errorf("PayloadsScrubbed = %d; want 4", res.PayloadsScrubbed)
	}
	if res.NormalizedScrubbed != 5 {
		t.Errorf("NormalizedScrubbed = %d; want 5", res.NormalizedScrubbed)
	}
	if res.SpillRefsOrphaned != 2 {
		t.Errorf("SpillRefsOrphaned = %d; want 2", res.SpillRefsOrphaned)
	}
	if res.AssistantErased != 12 { // 1 + 2 + 3 + 4 + 2
		t.Errorf("AssistantErased = %d; want 12", res.AssistantErased)
	}
	// Prior ACCESS exports persisted in dsar_request.outcome are scrubbed too
	// so ACCESS-then-ERASURE leaves no residual PII.
	if res.AccessOutcomesScrubbed != 1 {
		t.Errorf("AccessOutcomesScrubbed = %d; want 1", res.AccessOutcomesScrubbed)
	}
	// Account-record deletion: the subject's owned keys, SSO link rows,
	// refresh + SCIM tokens, and the NexusUser itself are GONE; the audit chain
	// is untouched (no AdminAuditLog DELETE was queued — ExpectationsWereMet
	// below would fail if the code had issued one).
	if res.VKOwnedDeleted != 2 {
		t.Errorf("VKOwnedDeleted = %d; want 2", res.VKOwnedDeleted)
	}
	if res.AdminApiKeysDeleted != 1 {
		t.Errorf("AdminApiKeysDeleted = %d; want 1", res.AdminApiKeysDeleted)
	}
	if res.FederatedIdentitiesDeleted != 3 {
		t.Errorf("FederatedIdentitiesDeleted = %d; want 3", res.FederatedIdentitiesDeleted)
	}
	if res.RefreshTokensDeleted != 4 {
		t.Errorf("RefreshTokensDeleted = %d; want 4", res.RefreshTokensDeleted)
	}
	if res.ScimTokensDeleted != 1 {
		t.Errorf("ScimTokensDeleted = %d; want 1", res.ScimTokensDeleted)
	}
	if res.IamGroupMembershipsDeleted != 2 {
		t.Errorf("IamGroupMembershipsDeleted = %d; want 2", res.IamGroupMembershipsDeleted)
	}
	if res.IamPolicyAttachmentsDeleted != 1 {
		t.Errorf("IamPolicyAttachmentsDeleted = %d; want 1", res.IamPolicyAttachmentsDeleted)
	}
	if !res.AccountDeleted {
		t.Errorf("AccountDeleted = false; want true (NexusUser row removed)")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("erasure statements mismatch (incl. audit-chain-preserved invariant): %v", err)
	}

	// error arms — every statement's failure must roll back and surface.
	for _, tc := range []struct {
		name  string
		setup func(m pgxmock.PgxPoolIface)
	}{
		{"begin", func(m pgxmock.PgxPoolIface) { m.ExpectBegin().WillReturnError(errors.New("x")) }},
		{"count query", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"payload scrub", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"assistant delete", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event_normalized`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event\s+SET entity_id = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event t\s+SET source_ip = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"outcome scrub", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event_normalized`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event\s+SET entity_id = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event t\s+SET source_ip = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantSession"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantFile"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantPendingConfirm"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`UPDATE dsar_request\s+SET outcome = NULL`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"owned vk delete", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event_normalized`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event\s+SET entity_id = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event t\s+SET source_ip = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantSession"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantFile"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantPendingConfirm"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`UPDATE dsar_request\s+SET outcome = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "VirtualKey"`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"account delete", func(m pgxmock.PgxPoolIface) {
			m.ExpectBegin()
			m.ExpectQuery(`SELECT COUNT`).WithArgs("subj1").WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectExec(`UPDATE traffic_event_payload`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event_normalized`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event\s+SET entity_id = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`UPDATE traffic_event t\s+SET source_ip = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantSession"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantFile"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AssistantPendingConfirm"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`UPDATE dsar_request\s+SET outcome = NULL`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			m.ExpectExec(`DELETE FROM "VirtualKey"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "AdminApiKey"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "UserFederatedIdentity"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "RefreshToken"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "ScimToken"`).WithArgs("subj1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
			m.ExpectExec(`DELETE FROM "NexusUser"`).WithArgs("subj1").WillReturnError(errors.New("x"))
			m.ExpectRollback()
		}},
		{"commit", func(m pgxmock.PgxPoolIface) {
			expectErasureHappy(m)
			m.ExpectCommit().WillReturnError(errors.New("x"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := newMock(t)
			tc.setup(m)
			if _, err := s.FulfillDSARErasure(context.Background(), "subj1"); err == nil {
				t.Fatalf("%s error should surface", tc.name)
			}
		})
	}
}

func TestDSARStatusCountsAndPeriod(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM dsar_request`).
		WillReturnRows(pgxmock.NewRows([]string{"pending", "inProgress", "completed", "rejected"}).AddRow(2, 1, 5, 1))
	c, err := s.GetDSARStatusCounts(context.Background())
	if err != nil || c.Pending != 2 || c.Completed != 5 {
		t.Fatalf("GetDSARStatusCounts: %+v %v", c, err)
	}
	m.ExpectQuery(`FROM dsar_request`).WillReturnError(errors.New("boom"))
	if _, err := s.GetDSARStatusCounts(context.Background()); err == nil {
		t.Fatal("status counts error should surface")
	}
	m.ExpectQuery(`status = 'COMPLETED' AND completed_at`).WithArgs(tNow, tNow).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(7))
	if n, err := s.GetDSARCompletedInPeriod(context.Background(), tNow, tNow); err != nil || n != 7 {
		t.Fatalf("GetDSARCompletedInPeriod: %d %v", n, err)
	}
	m.ExpectQuery(`completed_at`).WithArgs(tNow, tNow).WillReturnError(errors.New("boom"))
	if _, err := s.GetDSARCompletedInPeriod(context.Background(), tNow, tNow); err == nil {
		t.Fatal("period count error should surface")
	}
}
