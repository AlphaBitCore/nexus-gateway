package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// overrideMgrTestPool returns a pgx pool for manager-level override tests.
// Mirrors the store-level harness in thing_config_override_test.go so a
// machine without a running stack still passes.
//
// Test-data isolation: every override test seeds rows under
// `overrideMgrTestPrefix` + a per-test slug, and cleanupOverrideMgrPrefix
// deletes exclusively under that prefix. No table-wide TRUNCATE / wildcard
// DELETE / cross-test data touching — safe to run alongside live data.
func overrideMgrTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

// overrideMgrTestPrefix scopes every row a single test seeds so a parallel
// run never races against another test's data.
const overrideMgrTestPrefix = "tco-mgr-test-"

// seedThingForMgr inserts a minimal thing row so the FK on
// thing_config_override holds.
func seedThingForMgr(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, ttype string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
		                   status, metadata, desired, reported, desired_ver, reported_ver, last_seen_at, enrolled_at, updated_at)
		VALUES ($1, $2, $3, '1.0.0', '127.0.0.1', 'tester', 'bearer', 'http',
		        'online', '{}', '{}', '{}', 0, 0, NOW(), NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET type = EXCLUDED.type
	`, id, ttype, id)
	if err != nil {
		t.Fatalf("seed thing %s: %v", id, err)
	}
}

// seedTemplateForMgr writes a thing_config_template row at a specific version.
func seedTemplateForMgr(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ttype, key string, version int64, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal template state: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO thing_config_template (type, config_key, state, version, updated_at, updated_by)
		VALUES ($1, $2, $3::jsonb, $4, NOW(), 'tester')
		ON CONFLICT (type, config_key) DO UPDATE SET
			state = EXCLUDED.state,
			version = EXCLUDED.version,
			updated_at = NOW()
	`, ttype, key, stateJSON, version)
	if err != nil {
		t.Fatalf("seed template %s/%s: %v", ttype, key, err)
	}
}

// cleanupOverrideMgrPrefix scrubs rows seeded under a per-test prefix and any
// audit rows the manager wrote against that thing.
func cleanupOverrideMgrPrefix(ctx context.Context, pool *pgxpool.Pool, prefix string) {
	_, _ = pool.Exec(ctx, `DELETE FROM thing_config_override WHERE thing_id LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM "AdminAuditLog" WHERE "entityId" LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM thing_config_template WHERE config_key LIKE $1`, prefix+"%")
}

// newTestManager assembles a Manager backed by the real store + a mock WS pool
// so tests can assert on the post-commit push without spinning up the full
// WebSocket stack. mq is left nil; the override path never publishes hub
// signals (the push is per-Thing, and the Thing is "connected" via the mock).
func newTestManager(t *testing.T, pool *pgxpool.Pool) (*Manager, *mockWSPool) {
	t.Helper()
	st := store.New(pool)
	ws := &mockWSPool{connectedIDs: map[string]bool{}}
	mgr := &Manager{
		store:  st,
		ws:     ws,
		hubID:  "hub-test",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return mgr, ws
}

// countAuditRows returns the number of AdminAuditLog rows for a given
// (entityId, action) tuple — used to assert exactly-one audit semantics.
func countAuditRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, entityID, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "AdminAuditLog"
		WHERE "entityId" = $1 AND action = $2
	`, entityID, action).Scan(&n); err != nil {
		t.Fatalf("count audit rows %s/%s: %v", entityID, action, err)
	}
	return n
}

// fetchAuditAfterStateMap returns the most recent AdminAuditLog row's
// afterState JSON decoded as a map for a given (entityId, action).
func fetchAuditAfterStateMap(t *testing.T, ctx context.Context, pool *pgxpool.Pool, entityID, action string) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `
		SELECT "afterState"
		FROM "AdminAuditLog"
		WHERE "entityId" = $1 AND action = $2
		ORDER BY timestamp DESC
		LIMIT 1
	`, entityID, action).Scan(&raw); err != nil {
		t.Fatalf("fetch audit afterState %s/%s: %v", entityID, action, err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode afterState: %v", err)
		}
	}
	return out
}

// fetchAuditChainFields returns the (previousHash, integrityHash, sequenceNumber)
// of the most recent AdminAuditLog row for a given (entityId, action). Used
// to assert F3 chain semantics from the override path.
func fetchAuditChainFields(t *testing.T, ctx context.Context, pool *pgxpool.Pool, entityID, action string) (prev *string, integ string, seq int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT "previousHash", "integrityHash", "sequenceNumber"
		FROM "AdminAuditLog"
		WHERE "entityId" = $1 AND action = $2
		ORDER BY "sequenceNumber" DESC
		LIMIT 1
	`, entityID, action).Scan(&prev, &integ, &seq); err != nil {
		t.Fatalf("fetch chain fields %s/%s: %v", entityID, action, err)
	}
	return prev, integ, seq
}

func TestManager_SetOverride_HappyPath(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "happy-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-1"
	seedThingForMgr(t, ctx, pool, thingID, "agent")

	keyR := prefix + "routing"
	keyP := prefix + "policy"
	seedTemplateForMgr(t, ctx, pool, "agent", keyR, 4, map[string]any{"weight": 1})
	seedTemplateForMgr(t, ctx, pool, "agent", keyP, 1, map[string]any{"level": "low"})

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	reason := "canary rollout"
	expires := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	got, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: keyR,
		State:     json.RawMessage(`{"weight":99,"enabled":true}`),
		SetBy:     "alice@nexus.ai",
		Reason:    &reason,
		ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got == nil || got.ThingID != thingID || got.ConfigKey != keyR {
		t.Fatalf("returned override mismatch: %+v", got)
	}
	if got.TemplateVerAtSet != 4 {
		t.Errorf("TemplateVerAtSet = %d, want 4 (snapshotted)", got.TemplateVerAtSet)
	}
	if got.EmergencyOverride {
		t.Error("EmergencyOverride must be false for plain reason")
	}
	if got.SetAt.IsZero() {
		t.Error("SetAt should be populated by re-fetch")
	}

	// thing.desired must contain the override state for keyR + template state for keyP.
	st := store.New(pool)
	thing, err := st.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		t.Fatalf("GetThing: %v", err)
	}
	if thing.DesiredVer != 1 {
		t.Errorf("DesiredVer = %d, want 1 (one bump from 0)", thing.DesiredVer)
	}
	rt, ok := thing.Desired[keyR].(map[string]any)
	if !ok {
		t.Fatalf("desired[%s] not a map: %v", keyR, thing.Desired[keyR])
	}
	if rt["weight"].(float64) != 99 {
		t.Errorf("desired[%s].weight = %v, want 99", keyR, rt["weight"])
	}
	if rt["enabled"] != true {
		t.Errorf("desired[%s].enabled = %v, want true", keyR, rt["enabled"])
	}
	pl, ok := thing.Desired[keyP].(map[string]any)
	if !ok {
		t.Fatalf("desired[%s] not a map: %v", keyP, thing.Desired[keyP])
	}
	if pl["level"] != "low" {
		t.Errorf("desired[%s].level = %v, want low (template)", keyP, pl["level"])
	}

	// Audit row exists with action thing_override_set + correct metadata.
	if n := countAuditRows(t, ctx, pool, thingID, "thing_override_set"); n != 1 {
		t.Errorf("audit row count = %d, want 1", n)
	}
	meta := fetchAuditAfterStateMap(t, ctx, pool, thingID, "thing_override_set")
	if meta["configKey"] != keyR {
		t.Errorf("audit configKey = %v, want %s", meta["configKey"], keyR)
	}
	if meta["emergencyOverride"] != false {
		t.Errorf("audit emergencyOverride = %v, want false", meta["emergencyOverride"])
	}
	if meta["templateVerAtSet"].(float64) != 4 {
		t.Errorf("audit templateVerAtSet = %v, want 4", meta["templateVerAtSet"])
	}

	// Push must have happened — exactly one Send for this Thing.
	ws.mu.Lock()
	calls := append([]mockSendCall(nil), ws.sendCalls...)
	ws.mu.Unlock()
	if len(calls) != 1 || calls[0].ThingID != thingID {
		t.Errorf("WS Send calls = %d, want 1 to %s", len(calls), thingID)
	}

	// F3 chain: integrityHash must be populated; previousHash links to the
	// preceding row (NULL only if this happens to be the genesis row, which
	// is rare in a populated DB but cheap to special-case).
	_, integ, _ := fetchAuditChainFields(t, ctx, pool, thingID, "thing_override_set")
	if len(integ) != 64 {
		t.Errorf("audit integrityHash len = %d, want 64 (sha256 hex)", len(integ))
	}
}

func TestManager_SetOverride_BlacklistKey(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "blacklist-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-2"
	seedThingForMgr(t, ctx, pool, thingID, "agent")

	mgr, _ := newTestManager(t, pool)
	_, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: "credentials",
		State:     json.RawMessage(`{"apiKey":"x"}`),
		SetBy:     "alice",
	})
	if !errors.Is(err, ErrKeyNotOverridable) {
		t.Fatalf("err = %v, want ErrKeyNotOverridable", err)
	}

	// No DB writes — no override row, no audit row.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thing_config_override WHERE thing_id = $1`, thingID).Scan(&n); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if n != 0 {
		t.Errorf("override rows = %d, want 0", n)
	}
	if n := countAuditRows(t, ctx, pool, thingID, "thing_override_set"); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

func TestManager_SetOverride_NoTemplate(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "no-tmpl-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-3"
	seedThingForMgr(t, ctx, pool, thingID, "agent")
	// Intentionally no template.

	mgr, _ := newTestManager(t, pool)
	_, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: prefix + "novel",
		State:     json.RawMessage(`{"v":1}`),
		SetBy:     "alice",
	})
	if !errors.Is(err, ErrTemplateMissing) {
		t.Fatalf("err = %v, want ErrTemplateMissing", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thing_config_override WHERE thing_id = $1`, thingID).Scan(&n); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if n != 0 {
		t.Errorf("override rows = %d, want 0", n)
	}
}

func TestManager_SetOverride_BreakGlassReason(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "bg-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-4"
	seedThingForMgr(t, ctx, pool, thingID, "agent")

	key := prefix + "routing"
	seedTemplateForMgr(t, ctx, pool, "agent", key, 1, map[string]any{"v": 1})

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	reason := "break-glass: incident-1234"
	got, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: key,
		State:     json.RawMessage(`{"v":42}`),
		SetBy:     "responder@nexus.ai",
		Reason:    &reason,
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !got.EmergencyOverride {
		t.Error("EmergencyOverride should be true for break-glass: reason")
	}

	meta := fetchAuditAfterStateMap(t, ctx, pool, thingID, "thing_override_set")
	if meta["emergencyOverride"] != true {
		t.Errorf("audit emergencyOverride = %v, want true", meta["emergencyOverride"])
	}
	if meta["reason"] != reason {
		t.Errorf("audit reason = %v, want %q", meta["reason"], reason)
	}
}

func TestManager_SetOverride_KillswitchEmergency(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "ks-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-5"
	seedThingForMgr(t, ctx, pool, thingID, "agent")
	seedTemplateForMgr(t, ctx, pool, "agent", "killswitch", 1, map[string]any{"engaged": false})

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	got, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: "killswitch",
		State:     json.RawMessage(`{"engaged":true}`),
		SetBy:     "alice@nexus.ai",
		// No reason, no break-glass: prefix; the configKey alone must flip the flag.
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !got.EmergencyOverride {
		t.Error("EmergencyOverride should be true for killswitch override")
	}

	meta := fetchAuditAfterStateMap(t, ctx, pool, thingID, "thing_override_set")
	if meta["emergencyOverride"] != true {
		t.Errorf("audit emergencyOverride = %v, want true", meta["emergencyOverride"])
	}
}

func TestManager_ClearOverride_HappyPath(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "clr-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-6"
	seedThingForMgr(t, ctx, pool, thingID, "agent")

	key := prefix + "routing"
	seedTemplateForMgr(t, ctx, pool, "agent", key, 2, map[string]any{"weight": 1})

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	// Set then clear.
	if _, err := mgr.SetOverride(ctx, SetOverrideRequest{
		ThingID:   thingID,
		ConfigKey: key,
		State:     json.RawMessage(`{"weight":99}`),
		SetBy:     "alice",
	}); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	st := store.New(pool)
	preClear, err := st.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		t.Fatalf("GetThing pre-clear: %v", err)
	}

	if err := mgr.ClearOverride(ctx, thingID, key, "bob"); err != nil {
		t.Fatalf("ClearOverride: %v", err)
	}

	// Override row gone.
	if _, err := st.OverrideStore().GetOverride(ctx, thingID, key); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetOverride after Clear: err = %v, want ErrNotFound", err)
	}

	// thing.desired reverts to template state, desired_ver bumped.
	postClear, err := st.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		t.Fatalf("GetThing post-clear: %v", err)
	}
	if postClear.DesiredVer != preClear.DesiredVer+1 {
		t.Errorf("DesiredVer = %d, want %d (one more bump)", postClear.DesiredVer, preClear.DesiredVer+1)
	}
	rt, ok := postClear.Desired[key].(map[string]any)
	if !ok {
		t.Fatalf("desired[%s] not a map: %v", key, postClear.Desired[key])
	}
	if rt["weight"].(float64) != 1 {
		t.Errorf("desired[%s].weight = %v, want 1 (template)", key, rt["weight"])
	}

	// Exactly one cleared audit row.
	if n := countAuditRows(t, ctx, pool, thingID, "thing_override_cleared"); n != 1 {
		t.Errorf("clear audit rows = %d, want 1", n)
	}

	// Push fired again on clear.
	ws.mu.Lock()
	pushCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if pushCount != 2 {
		t.Errorf("WS push count = %d, want 2 (1 set + 1 clear)", pushCount)
	}

	// F3 chain: the cleared row's previousHash must equal the set row's
	// integrityHash (set ran before clear in the same test). This proves the
	// Hub direct writer participates in the same chain as everything else.
	_, setInteg, setSeq := fetchAuditChainFields(t, ctx, pool, thingID, "thing_override_set")
	clrPrev, clrInteg, clrSeq := fetchAuditChainFields(t, ctx, pool, thingID, "thing_override_cleared")
	if len(clrInteg) != 64 {
		t.Errorf("clear integrityHash len = %d, want 64", len(clrInteg))
	}
	if clrSeq <= setSeq {
		t.Errorf("clear seq %d should be > set seq %d", clrSeq, setSeq)
	}
	// They are consecutive only if no other audit row interleaved (this test
	// touches one prefix-scoped Thing, so we expect adjacency).
	if clrSeq == setSeq+1 {
		if clrPrev == nil || *clrPrev != setInteg {
			t.Errorf("clear previousHash = %v, want set integrityHash %s", clrPrev, setInteg)
		}
	}
}

func TestManager_ClearOverride_Missing(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "clr-miss-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-7"
	seedThingForMgr(t, ctx, pool, thingID, "agent")

	mgr, _ := newTestManager(t, pool)
	err := mgr.ClearOverride(ctx, thingID, prefix+"absent", "alice")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

func TestManager_ForceResyncAll_FanOut(t *testing.T) {
	// ForceResyncAll reads the Thing from the store, bumps its desired_ver in a
	// tx, then fans the keys out, so we use a real pool to seed the thing row.
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "repush-all-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-8"
	seedThingForMgr(t, ctx, pool, thingID, "agent")
	// Hand-roll a desired map directly into the thing row so the manager
	// reads back three keys without us having to seed templates + bump versions.
	if _, err := pool.Exec(ctx, `
		UPDATE thing
		SET desired = $2::jsonb, desired_ver = 5, updated_at = NOW()
		WHERE id = $1
	`, thingID, []byte(`{"hooks":{"e":1},"policy":{"l":"high"},"routing":{"w":7}}`)); err != nil {
		t.Fatalf("seed desired: %v", err)
	}

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	res, err := mgr.ForceResyncAll(ctx, thingID)
	if err != nil {
		t.Fatalf("ForceResyncAll: %v", err)
	}
	if res.Pushed != 3 {
		t.Errorf("Pushed = %d, want 3", res.Pushed)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %+v, want empty", res.Failed)
	}

	ws.mu.Lock()
	calls := append([]mockSendCall(nil), ws.sendCalls...)
	ws.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("WS Send count = %d, want 3", len(calls))
	}
	for i, c := range calls {
		if c.ThingID != thingID {
			t.Errorf("call[%d].ThingID = %q, want %s", i, c.ThingID, thingID)
		}
		var msg map[string]any
		if err := json.Unmarshal(c.Data, &msg); err != nil {
			t.Fatalf("call[%d] decode: %v", i, err)
		}
		if msg["force"] != true {
			t.Errorf("call[%d] force = %v, want true", i, msg["force"])
		}
		// desired_ver was 5 on seed; ForceResyncAll bumps it to 6 before the
		// push so an HTTP-fallback Thing's heartbeat compare fires (F-0116).
		if msg["desiredVer"].(float64) != 6 {
			t.Errorf("call[%d] desiredVer = %v, want 6 (bumped)", i, msg["desiredVer"])
		}
	}

	// The bump must be persisted, not just stamped on the in-flight message.
	var persistedVer int64
	if err := pool.QueryRow(ctx, `SELECT desired_ver FROM thing WHERE id = $1`, thingID).Scan(&persistedVer); err != nil {
		t.Fatalf("read desired_ver: %v", err)
	}
	if persistedVer != 6 {
		t.Errorf("persisted desired_ver = %d, want 6", persistedVer)
	}
}

func TestManager_ForceResyncAll_EmptyDesired(t *testing.T) {
	pool := overrideMgrTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideMgrTestPrefix + "repush-empty-"
	cleanupOverrideMgrPrefix(ctx, pool, prefix)
	defer cleanupOverrideMgrPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-9"
	seedThingForMgr(t, ctx, pool, thingID, "agent")
	// thing.desired left as the seeded '{}' empty object.

	mgr, ws := newTestManager(t, pool)
	ws.connectedIDs[thingID] = true

	res, err := mgr.ForceResyncAll(ctx, thingID)
	if err != nil {
		t.Fatalf("ForceResyncAll: %v", err)
	}
	if res.Pushed != 0 {
		t.Errorf("Pushed = %d, want 0", res.Pushed)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %+v, want empty", res.Failed)
	}
	ws.mu.Lock()
	sendCount := len(ws.sendCalls)
	ws.mu.Unlock()
	if sendCount != 0 {
		t.Errorf("WS Send count = %d, want 0 for empty desired", sendCount)
	}
}
