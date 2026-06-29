// Coverage-completion tests for the residual reachable branches in the fleet
// Manager that the existing focused suites left uncovered: the inter-Hub signal
// HMAC sign/verify helpers (config.go), the heartbeat egress-IP refresh
// goroutine, the RegisterThing name-default + enroll-error + service-row +
// cache-marshal-skip paths, the cacheShadow marshal skip, the RePushConfig
// WS-send-false → MQ fall-through, the emitBreakGlassDenied audit-write tails,
// and the insertAdminAuditLog marshal/chain error returns.
//
// Every test asserts a real outcome (the verified/forged signal, the emitted
// row, the specific wrapped error, the cached/not-cached key) — never bare
// execution.

package manager

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// TestHubSignal_SignVerify_HMAC locks the inter-Hub authenticity
// guarantee end to end: a frame signed with a secret round-trips through
// VerifyAndDecodeHubSignal under the SAME secret, and every forgery /
// corruption mode (wrong secret, stripped sig, mangled envelope, empty payload,
// corrupt inner payload) is dropped (ok=false). The unsigned path (empty
// secret) accepts any well-formed frame.
func TestHubSignal_SignVerify_HMAC(t *testing.T) {
	secret := []byte("shared-hub-secret")
	sig := HubSignal{
		Action:    "config_changed",
		SourceHub: "hub-east",
		ThingType: "agent",
		ConfigKey: "routing",
		State:     map[string]any{"enabled": true},
		Version:   42,
	}

	t.Run("signed frame verifies under same secret", func(t *testing.T) {
		data, err := SignHubSignal(sig, secret)
		if err != nil {
			t.Fatalf("SignHubSignal: %v", err)
		}
		// The signed envelope must actually carry a non-empty HMAC.
		var env struct {
			Sig     string          `json:"sig"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("envelope unmarshal: %v", err)
		}
		if env.Sig == "" {
			t.Fatal("signed frame must carry a non-empty sig")
		}
		got, ok := VerifyAndDecodeHubSignal(data, secret)
		if !ok {
			t.Fatal("valid HMAC must verify ok")
		}
		if got.ConfigKey != "routing" || got.Version != 42 || got.SourceHub != "hub-east" {
			t.Errorf("decoded signal mismatch: %+v", got)
		}
	})

	t.Run("wrong secret is dropped", func(t *testing.T) {
		data, _ := SignHubSignal(sig, secret)
		if _, ok := VerifyAndDecodeHubSignal(data, []byte("attacker-secret")); ok {
			t.Fatal("a frame signed with a different secret must be dropped")
		}
	})

	t.Run("missing sig under required secret is dropped", func(t *testing.T) {
		// Sign with NO secret → envelope has empty sig; verify WITH a secret.
		data, _ := SignHubSignal(sig, nil)
		if _, ok := VerifyAndDecodeHubSignal(data, secret); ok {
			t.Fatal("an unsigned frame must be dropped when a secret is configured")
		}
	})

	t.Run("malformed envelope is dropped", func(t *testing.T) {
		if _, ok := VerifyAndDecodeHubSignal([]byte("not json"), secret); ok {
			t.Fatal("non-JSON envelope must be dropped")
		}
	})

	t.Run("empty payload is dropped", func(t *testing.T) {
		if _, ok := VerifyAndDecodeHubSignal([]byte(`{"sig":"x"}`), secret); ok {
			t.Fatal("envelope with empty payload must be dropped")
		}
	})

	t.Run("corrupt inner payload is dropped even with valid envelope MAC", func(t *testing.T) {
		// Hand-build an envelope whose payload is valid-bytes-but-not-a-HubSignal,
		// then sign THAT payload so the MAC check passes and the inner
		// json.Unmarshal is the thing that fails.
		badPayload := json.RawMessage(`123`) // a number, not a HubSignal object
		data, _ := json.Marshal(struct {
			Sig     string          `json:"sig"`
			Payload json.RawMessage `json:"payload"`
		}{
			Sig:     hubSignalMAC(badPayload, secret),
			Payload: badPayload,
		})
		if _, ok := VerifyAndDecodeHubSignal(data, secret); ok {
			t.Fatal("a payload that is not a HubSignal object must be dropped")
		}
	})

	t.Run("unsigned mode accepts any well-formed frame", func(t *testing.T) {
		data, _ := SignHubSignal(sig, nil) // no secret → no sig stamped
		got, ok := VerifyAndDecodeHubSignal(data, nil)
		if !ok {
			t.Fatal("unsigned mode must accept a well-formed frame")
		}
		if got.ConfigKey != "routing" {
			t.Errorf("decoded ConfigKey = %q, want routing", got.ConfigKey)
		}
	})
}

// TestManager_SetSignalSecret asserts that the installed secret is the one used
// to sign subsequently-published frames: after SetSignalSecret, a frame this
// Manager publishes verifies under that same secret (and not under a different
// one).
func TestManager_SetSignalSecret(t *testing.T) {
	mq := &mockMQProducer{}
	m := &Manager{mq: mq, hubID: "hub-1", logger: silentLogger()}
	secret := []byte("installed-secret")
	m.SetSignalSecret(secret)

	m.publishHubSignal(context.Background(), "agent", "routing", map[string]any{"e": true}, 7)

	mq.mu.Lock()
	published := mq.lastData
	count := mq.publishCount
	mq.mu.Unlock()
	if count != 1 {
		t.Fatalf("publishCount = %d, want 1", count)
	}
	if _, ok := VerifyAndDecodeHubSignal(published, secret); !ok {
		t.Fatal("frame published after SetSignalSecret must verify under the installed secret")
	}
	if _, ok := VerifyAndDecodeHubSignal(published, []byte("other")); ok {
		t.Fatal("frame must NOT verify under a different secret")
	}
}

// TestManager_UpdateConfig_RowsButZeroShadowVer drives the UpdateConfig default
// branch: rows were updated (affected>0) but the returned thing_desired_ver is
// 0 (a "bad shadow ver" condition). The response must report ThingsOnline but
// NOT set ThingDesiredVer, and must NOT broadcast (notified stays 0).
func TestManager_UpdateConfig_RowsButZeroShadowVer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	// UpsertConfigTemplate → new version 5.
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hooks", pgxmock.AnyArg(), "admin").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(5)))
	// UpdateDesiredForType returns affected=2 but shadowDesiredVer=0.
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "hooks", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
			AddRow("t-1", int64(0)).
			AddRow("t-2", int64(0)))
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("agent", "hooks", "update", "admin", "", pgxmock.AnyArg(), int64(5), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectCommit()

	resp, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "hooks", State: map[string]any{"e": true},
		Action: "update", ActorID: "admin",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.ThingsOnline != 2 {
		t.Errorf("ThingsOnline = %d, want 2", resp.ThingsOnline)
	}
	// thingShadowVer == 0 → the broadcast switch hit the default warn branch,
	// so nothing was notified and ThingDesiredVer must stay unset.
	if resp.ThingsNotified != 0 {
		t.Errorf("ThingsNotified = %d, want 0 (no broadcast on zero shadow ver)", resp.ThingsNotified)
	}
	if resp.ThingDesiredVer != 0 {
		t.Errorf("ThingDesiredVer = %d, want 0 (unset on zero shadow ver)", resp.ThingDesiredVer)
	}
	ws.mu.Lock()
	broadcasts := ws.lastBroadcastMsg
	ws.mu.Unlock()
	if broadcasts != nil {
		t.Error("no config_changed broadcast must fire when shadow ver is zero")
	}
}

// TestManager_HandleHeartbeat_RefreshesEgressIP locks the F-identity-enricher
// behavior: a heartbeat carrying a non-empty IPAddress spawns the
// refreshDeviceAssignmentIP goroutine, which UPDATEs DeviceAssignment.ip_address.
// The test pins that UPDATE on the mock and waits for the goroutine to drain.
func TestManager_HandleHeartbeat_RefreshesEgressIP(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := New(st, nil, nil, nil, "hub-test", silentLogger())

	// The heartbeat spawns two concurrent fire-and-forget goroutines (trust
	// recompute + egress-IP refresh); their DB calls race, so match unordered.
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`UPDATE thing\s+SET status\s*= CASE`).
		WithArgs("agent-1", "online", pgxmock.AnyArg(), nil, nil, nil, nil).
		WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
			AddRow(int64(3), []byte(`{}`)))
	// trust_level goroutine: no thing_agent → quick exit.
	mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
		WithArgs("agent-1").
		WillReturnError(pgx.ErrNoRows)
	// The egress-IP goroutine fires ONLY because IPAddress is non-empty.
	mock.ExpectExec(`UPDATE "DeviceAssignment"\s+SET ip_address`).
		WithArgs("agent-1", "203.0.113.9").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if _, err := mgr.HandleHeartbeat(context.Background(), HeartbeatRequest{
		ID: "agent-1", Status: "online", ReportedVer: 1, IPAddress: "203.0.113.9",
	}); err != nil {
		t.Fatalf("HandleHeartbeat: %v", err)
	}
	// Drain the fire-and-forget goroutines so their mock expectations settle.
	time.Sleep(80 * time.Millisecond)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("egress-IP refresh UPDATE not issued by heartbeat goroutine: %v", err)
	}
}

// TestManager_RegisterThing_NameDefaultsToID drives the enrollment branch where
// the client sent no Name: the row must be written with name == ID (the empty
// name is never persisted as the admin-display label).
func TestManager_RegisterThing_NameDefaultsToID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", silentLogger())

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":true}`), int64(2), now, "alice"))
	// Touch returns 0 rows → ErrNotFound → enrollment path.
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("a-noname", "1.0", "addr", "").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	// Enrollment INSERT: name arg (slot 3) must be the ID fallback "a-noname".
	mock.ExpectExec(`INSERT INTO thing\s*\(`).
		WithArgs(
			"a-noname", "agent", "a-noname", "1.0", "addr",
			"", "bearer", "http", "online",
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(2), nil,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("a-noname").
		WillReturnRows(minimalGetThingRow("a-noname", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 2))

	resp, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "a-noname", Type: "agent", Version: "1.0", Address: "addr",
	})
	if err != nil {
		t.Fatalf("RegisterThing: %v", err)
	}
	if resp.DesiredVer != 2 {
		t.Errorf("DesiredVer = %d, want 2", resp.DesiredVer)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("enrollment must INSERT with name defaulted to ID: %v", err)
	}
}

// TestManager_RegisterThing_EnrollErr surfaces the wrapped error from the
// enrollment UPSERT (the ErrNotFound → enroll path that then fails).
func TestManager_RegisterThing_EnrollErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", silentLogger())

	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("a-1", "1.0", "addr", "host").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	mock.ExpectExec(`INSERT INTO thing\s*\(`).
		WillReturnError(errors.New("constraint violation"))

	_, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "a-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
	})
	if err == nil || !strings.Contains(err.Error(), "enroll thing") {
		t.Errorf("err = %v, want enroll-thing wrap", err)
	}
}

// TestManager_RegisterThing_ServiceRowUpsertErrNonFatal drives the non-agent
// thing_service UPSERT branch and asserts a failure there is logged but does
// NOT fail registration (the register still returns the desired payload).
func TestManager_RegisterThing_ServiceRowUpsertErrNonFatal(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", silentLogger())
	// thing_service UPSERT is best-effort: its error is logged, registration
	// continues to GetThing. Match unordered so the swallowed-Exec and the
	// follow-on GetThing Query are not order-coupled.
	mock.MatchExpectationsInOrder(false)

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("ai-gateway").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("ai-gateway", "routing", []byte(`{"r":1}`), int64(4), now, "alice"))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("gw-1", "1.0", "addr", "gw").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// thing_service UPSERT errors — must be swallowed (warn-only).
	mock.ExpectExec(`INSERT INTO thing_service`).
		WillReturnError(errors.New("planner err"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("gw-1").
		WillReturnRows(minimalGetThingRow("gw-1", "ai-gateway", map[string]any{"routing": map[string]any{"r": 1}}, 4))

	resp, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "gw-1", Type: "ai-gateway", Name: "gw", Version: "1.0", Address: "addr",
		MetricsURL: "http://gw-1/metrics", Role: "primary",
	})
	if err != nil {
		t.Fatalf("thing_service upsert failure must be non-fatal, got: %v", err)
	}
	if resp.DesiredVer != 4 {
		t.Errorf("DesiredVer = %d, want 4", resp.DesiredVer)
	}
}

// TestManager_RegisterThing_GetThingAfterRegisterErr surfaces the wrapped error
// from the post-write GetThing readback: registration commits the row but the
// readback fails, which must propagate as "get thing after register".
func TestManager_RegisterThing_GetThingAfterRegisterErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", silentLogger())

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":true}`), int64(1), now, "alice"))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("a-1", "1.0", "addr", "host").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1")) // touch OK
	// GetThing readback fails.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("a-1").
		WillReturnError(errors.New("conn lost"))

	_, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "a-1", Type: "agent", Name: "host", Version: "1.0", Address: "addr",
	})
	if err == nil || !strings.Contains(err.Error(), "get thing after register") {
		t.Errorf("err = %v, want get-thing-after-register wrap", err)
	}
}

// TestManager_RePushConfig_MQPublishErr drives the rePushConfigForThing MQ
// branch where the Thing is not connected locally (WS path skipped) and the
// hub-signal publish fails: the error must propagate as "publish hub signal"
// (drift repair must surface the delivery failure, not swallow it).
func TestManager_RePushConfig_MQPublishErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	// No WS connection → MQ branch; publish fails.
	mq := &mockMQProducer{publishErr: errors.New("nats down")}
	mgr := NewWithPool(st, mock, nil, mq, &mockWSPool{}, "hub-test", silentLogger())

	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))

	err := mgr.RePushConfig(context.Background(), "t-1", "agent")
	if err == nil || !strings.Contains(err.Error(), "publish hub signal") {
		t.Errorf("err = %v, want publish-hub-signal wrap", err)
	}
}

// TestManager_CacheDesired_MarshalSkip locks the marshal-error continue branch
// in cacheDesired: an unmarshalable value for one key is skipped (no Redis
// write for it) while a sibling encodable key IS cached. Uses miniredis to
// observe the written/absent keys.
func TestManager_CacheDesired_MarshalSkip(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	mgr.cacheDesired(context.Background(), "agent", map[string]any{
		"good": map[string]any{"e": true},
		"bad":  make(chan int), // unmarshalable → marshal err → skipped
	})
	if _, err := mini.Get("nexus:desired:agent:good"); err != nil {
		t.Errorf("encodable key must be cached: %v", err)
	}
	if _, err := mini.Get("nexus:desired:agent:bad"); err == nil {
		t.Error("unmarshalable key must be skipped, not cached")
	}
}

// TestManager_CacheShadow_MarshalSkip is the cacheShadow twin: the unmarshalable
// reported key is skipped while the encodable sibling is cached.
func TestManager_CacheShadow_MarshalSkip(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	mgr.cacheShadow(context.Background(), "thing-9", map[string]any{
		"good": map[string]any{"applied": 3},
		"bad":  make(chan int),
	})
	if _, err := mini.Get("nexus:shadow:thing-9:good"); err != nil {
		t.Errorf("encodable reported key must be cached: %v", err)
	}
	if _, err := mini.Get("nexus:shadow:thing-9:bad"); err == nil {
		t.Error("unmarshalable reported key must be skipped, not cached")
	}
}

// TestManager_RePushConfig_WSSendFalseFallsBackToMQ drives the rePushConfigForThing
// branch where the Thing is reported connected (IsConnected=true) but ws.Send
// returns false (close-during-call race): the delivery must NOT be dropped — it
// falls through to the MQ hub-signal publish so drift repair converges.
func TestManager_RePushConfig_WSSendFalseFallsBackToMQ(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{
		connectedIDs: map[string]bool{"t-1": true},
		sendReturn:   map[string]bool{"t-1": false}, // Send races to false
	}
	mq := &mockMQProducer{}
	mgr := NewWithPool(st, mock, nil, mq, ws, "hub-test", silentLogger())

	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))

	if err := mgr.RePushConfig(context.Background(), "t-1", "agent"); err != nil {
		t.Fatalf("RePushConfig: %v", err)
	}
	ws.mu.Lock()
	sends := len(ws.sendCalls)
	ws.mu.Unlock()
	mq.mu.Lock()
	publishes := mq.publishCount
	topic := mq.lastTopic
	mq.mu.Unlock()
	if sends != 1 {
		t.Errorf("WS Send attempts = %d, want 1", sends)
	}
	if publishes != 1 {
		t.Errorf("MQ publishes = %d, want 1 (Send-false must fall through to MQ)", publishes)
	}
	if topic != "nexus.hub.signal" {
		t.Errorf("MQ topic = %q, want nexus.hub.signal", topic)
	}
}

// TestManager_EmitBreakGlassDenied_AuditTails covers emitBreakGlassDenied's
// remaining branches reached via a denied break-glass report (a key outside the
// {killswitch, exemptions} allowlist):
//   - GetThing SUCCESS → the audit row carries the resolved thing_type,
//   - the audit INSERT erroring → logged + swallowed (report stays non-fatal),
//   - the commit erroring → logged + swallowed.
func TestManager_EmitBreakGlassDenied_AuditTails(t *testing.T) {
	deniedReq := ShadowReportRequest{
		ID:          "thing-1",
		Reported:    map[string]any{"credentialKeys": map[string]any{"x": 1}},
		ReportedVer: 7,
		Reason:      "break_glass",
		KeyVersions: map[string]int64{"credentialKeys": 7}, // not in writable allowlist
	}

	t.Run("GetThing success stamps resolved thing_type", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		// 1. UpdateShadowReport succeeds.
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(7), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		// 2. emitBreakGlassDenied's GetThing SUCCEEDS → thing_type = "agent".
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("thing-1").
			WillReturnRows(minimalGetThingRow("thing-1", "agent", map[string]any{}, 7))
		// 3. The denied event is written with thing_type "agent" (slot 1).
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO config_change_event`).
			WithArgs("agent", "credentialKeys", "break_glass_denied", "break-glass:unknown", "break-glass",
				pgxmock.AnyArg(), int64(7), "", false).
			WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
		mock.ExpectCommit()

		if err := mgr.HandleShadowReport(context.Background(), deniedReq); err != nil {
			t.Fatalf("denied break-glass must be non-fatal: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("audit row must carry the resolved thing_type: %v", err)
		}
	})

	t.Run("audit insert err is swallowed", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(7), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("thing-1").
			WillReturnError(errors.New("lookup skipped")) // type falls back to ""
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO config_change_event`).
			WillReturnError(errors.New("planner err")) // insert fails
		mock.ExpectRollback()
		if err := mgr.HandleShadowReport(context.Background(), deniedReq); err != nil {
			t.Fatalf("audit-insert failure must be swallowed (non-fatal): %v", err)
		}
	})

	t.Run("commit err is swallowed", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET reported`).
			WithArgs("thing-1", pgxmock.AnyArg(), int64(7), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("thing-1").
			WillReturnError(errors.New("lookup skipped"))
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO config_change_event`).
			WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
		mock.ExpectCommit().WillReturnError(errors.New("commit failed")) // commit fails
		if err := mgr.HandleShadowReport(context.Background(), deniedReq); err != nil {
			t.Fatalf("audit-commit failure must be swallowed (non-fatal): %v", err)
		}
	})
}

// TestInsertAdminAuditLog_MarshalErrs drives the two marshal-error returns of
// insertAdminAuditLog: a non-encodable BeforeState and a non-encodable
// AfterState each surface the corresponding wrapped error before any DB write.
func TestInsertAdminAuditLog_MarshalErrs(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	tx, _ := mock.Begin(context.Background())

	t.Run("beforeState marshal err", func(t *testing.T) {
		err := insertAdminAuditLog(context.Background(), tx, adminAuditEntry{
			Action: "override.set", ActorID: "admin", EntityType: "thing", EntityID: "t-1",
			BeforeState: make(chan int), // unmarshalable
		})
		if err == nil || !strings.Contains(err.Error(), "marshal beforeState") {
			t.Errorf("err = %v, want marshal-beforeState wrap", err)
		}
	})
	t.Run("afterState marshal err", func(t *testing.T) {
		err := insertAdminAuditLog(context.Background(), tx, adminAuditEntry{
			Action: "override.set", ActorID: "admin", EntityType: "thing", EntityID: "t-1",
			AfterState: make(chan int), // unmarshalable
		})
		if err == nil || !strings.Contains(err.Error(), "marshal afterState") {
			t.Errorf("err = %v, want marshal-afterState wrap", err)
		}
	})
}
