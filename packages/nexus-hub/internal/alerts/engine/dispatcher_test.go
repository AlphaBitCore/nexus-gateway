package alerting_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// testRegistry is a minimal alerting.SenderRegistry for dispatcher tests.
// Using this instead of senders.Registry avoids pulling the senders subpackage
// (and its HTTP clients) into the test binary — the dispatcher only needs the
// Get(channelType) lookup semantics.
type testRegistry struct{ m map[string]alerting.Sender }

func newTestRegistry() *testRegistry { return &testRegistry{m: map[string]alerting.Sender{}} }

func (r *testRegistry) Register(channelType string, s alerting.Sender) { r.m[channelType] = s }

func (r *testRegistry) Get(channelType string) (alerting.Sender, error) {
	s, ok := r.m[channelType]
	if !ok {
		return nil, fmt.Errorf("no sender for %q", channelType)
	}
	return s, nil
}

// fakeSender captures every Send call and returns a configurable result.
// Dispatcher invocations are synchronous per-channel, but we guard access
// anyway so `-race` stays clean if the implementation ever parallelises.
type fakeSender struct {
	mu      sync.Mutex
	calls   []alerting.Alert
	retCode int
	retErr  error
}

func (f *fakeSender) Send(_ context.Context, _ alerting.Channel, a alerting.Alert) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
	return f.retCode, f.retErr
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// seedDispatchRule inserts a minimal enabled AlertRule for Dispatcher tests.
func seedDispatchRule(t *testing.T, pool *pgxpool.Pool, id, sourceType string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, 'Test Dispatch Rule', $2, 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`,
		id, sourceType,
	)
	if err != nil {
		t.Fatalf("seed rule %q: %v", id, err)
	}
}

// seedAlertForDispatch inserts a test Alert for the given (rule, target, severity).
// Returns the generated alert ID.
func seedAlertForDispatch(t *testing.T, store *alerting.Store, ruleID, sourceType, targetKey string, sev alerting.Severity) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := alerting.Alert{
		RuleID:      ruleID,
		SourceType:  sourceType,
		TargetKey:   targetKey,
		TargetLabel: "Test Target",
		Severity:    sev,
		State:       alerting.StateFiring,
		Message:     "test alert",
		Details:     map[string]any{},
		FiredAt:     now,
		LastSeenAt:  now,
	}
	id, err := store.InsertAlert(context.Background(), a)
	if err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	return id
}

// silentLogger returns a slog.Logger that discards output — keeps test logs clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDispatcher_RoutesBySeverity(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	seedDispatchRule(t, pool, "test.dispatch.sev", "test")

	// Channel that only accepts "critical" severity.
	chID, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-sev-critical-only",
		Type:        "fake-ok",
		Enabled:     true,
		Severities:  []alerting.Severity{alerting.SeverityCritical},
		SourceTypes: []string{}, // empty = match any source
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	fake := &fakeSender{retCode: 200}
	reg := newTestRegistry()
	reg.Register("fake-ok", fake)

	d := alerting.NewDispatcher(store, reg, silentLogger())

	// Case 1: critical alert → sender called, success row written.
	critID := seedAlertForDispatch(t, store, "test.dispatch.sev", "test", "t:critical", alerting.SeverityCritical)
	critAlert := alerting.Alert{
		ID: critID, RuleID: "test.dispatch.sev", SourceType: "test",
		TargetKey: "t:critical", Severity: alerting.SeverityCritical,
		State: alerting.StateFiring, Message: "crit",
	}
	d.Dispatch(ctx, critAlert)

	if got := fake.callCount(); got != 1 {
		t.Fatalf("critical: want 1 send, got %d", got)
	}
	critRows, err := store.ListDispatchesByAlert(ctx, critID)
	if err != nil {
		t.Fatalf("list critical dispatches: %v", err)
	}
	if len(critRows) != 1 {
		t.Fatalf("critical dispatch rows: want 1, got %d", len(critRows))
	}
	if !critRows[0].Success {
		t.Errorf("critical dispatch: want success=true, got false")
	}

	// Case 2: medium alert → no send, no row for *this* alert+channel.
	medID := seedAlertForDispatch(t, store, "test.dispatch.sev", "test", "t:medium", alerting.SeverityMedium)
	medAlert := alerting.Alert{
		ID: medID, RuleID: "test.dispatch.sev", SourceType: "test",
		TargetKey: "t:medium", Severity: alerting.SeverityMedium,
		State: alerting.StateFiring, Message: "med",
	}
	d.Dispatch(ctx, medAlert)

	if got := fake.callCount(); got != 1 {
		t.Errorf("medium should not have triggered a send; total calls=%d", got)
	}
	medRows, err := store.ListDispatchesByAlert(ctx, medID)
	if err != nil {
		t.Fatalf("list medium dispatches: %v", err)
	}
	if len(medRows) != 0 {
		t.Errorf("medium alert: want 0 dispatch rows, got %d", len(medRows))
	}
	_ = chID
}

func TestDispatcher_RoutesBySourceType(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	seedDispatchRule(t, pool, "test.dispatch.quota", "quota")
	seedDispatchRule(t, pool, "test.dispatch.proxy", "proxy")

	// Channel that only accepts sourceType "quota".
	_, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-src-quota-only",
		Type:        "fake-ok",
		Enabled:     true,
		Severities:  []alerting.Severity{}, // empty = match any severity
		SourceTypes: []string{"quota"},
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	fake := &fakeSender{retCode: 200}
	reg := newTestRegistry()
	reg.Register("fake-ok", fake)

	d := alerting.NewDispatcher(store, reg, silentLogger())

	// Case 1: quota alert → sender called.
	qID := seedAlertForDispatch(t, store, "test.dispatch.quota", "quota", "t:quota", alerting.SeverityHigh)
	d.Dispatch(ctx, alerting.Alert{
		ID: qID, RuleID: "test.dispatch.quota", SourceType: "quota",
		TargetKey: "t:quota", Severity: alerting.SeverityHigh,
		State: alerting.StateFiring, Message: "q",
	})
	if got := fake.callCount(); got != 1 {
		t.Fatalf("quota: want 1 send, got %d", got)
	}

	// Case 2: proxy alert → sender NOT called.
	pID := seedAlertForDispatch(t, store, "test.dispatch.proxy", "proxy", "t:proxy", alerting.SeverityHigh)
	d.Dispatch(ctx, alerting.Alert{
		ID: pID, RuleID: "test.dispatch.proxy", SourceType: "proxy",
		TargetKey: "t:proxy", Severity: alerting.SeverityHigh,
		State: alerting.StateFiring, Message: "p",
	})
	if got := fake.callCount(); got != 1 {
		t.Errorf("proxy should not have triggered a send; total calls=%d", got)
	}
	proxyRows, err := store.ListDispatchesByAlert(ctx, pID)
	if err != nil {
		t.Fatalf("list proxy dispatches: %v", err)
	}
	if len(proxyRows) != 0 {
		t.Errorf("proxy alert: want 0 dispatch rows, got %d", len(proxyRows))
	}
}

func TestDispatcher_EmptyArraysMatchAll(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	seedDispatchRule(t, pool, "test.dispatch.any", "anything")

	// Channel with both arrays empty → matches any alert.
	_, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-match-all",
		Type:        "fake-ok",
		Enabled:     true,
		Severities:  []alerting.Severity{},
		SourceTypes: []string{},
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	fake := &fakeSender{retCode: 200}
	reg := newTestRegistry()
	reg.Register("fake-ok", fake)

	d := alerting.NewDispatcher(store, reg, silentLogger())

	alertID := seedAlertForDispatch(t, store, "test.dispatch.any", "anything", "t:any", alerting.SeverityInfo)
	d.Dispatch(ctx, alerting.Alert{
		ID: alertID, RuleID: "test.dispatch.any", SourceType: "anything",
		TargetKey: "t:any", Severity: alerting.SeverityInfo,
		State: alerting.StateFiring, Message: "any",
	})

	if got := fake.callCount(); got != 1 {
		t.Errorf("empty arrays should match all; want 1 send, got %d", got)
	}
	rows, err := store.ListDispatchesByAlert(ctx, alertID)
	if err != nil {
		t.Fatalf("list dispatches: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("want 1 dispatch row, got %d", len(rows))
	}
}

func TestDispatcher_DisabledChannelSkipped(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	seedDispatchRule(t, pool, "test.dispatch.disabled", "test")

	// Disabled channel — would otherwise match.
	_, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-disabled-chan",
		Type:        "fake-ok",
		Enabled:     false,
		Severities:  []alerting.Severity{},
		SourceTypes: []string{},
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	fake := &fakeSender{retCode: 200}
	reg := newTestRegistry()
	reg.Register("fake-ok", fake)

	d := alerting.NewDispatcher(store, reg, silentLogger())

	alertID := seedAlertForDispatch(t, store, "test.dispatch.disabled", "test", "t:disabled", alerting.SeverityHigh)
	d.Dispatch(ctx, alerting.Alert{
		ID: alertID, RuleID: "test.dispatch.disabled", SourceType: "test",
		TargetKey: "t:disabled", Severity: alerting.SeverityHigh,
		State: alerting.StateFiring, Message: "d",
	})

	if got := fake.callCount(); got != 0 {
		t.Errorf("disabled channel should not be invoked; got %d calls", got)
	}
	rows, err := store.ListDispatchesByAlert(ctx, alertID)
	if err != nil {
		t.Fatalf("list dispatches: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("disabled channel: want 0 dispatch rows, got %d", len(rows))
	}
}

func TestDispatcher_SuccessAndFailureBothWriteDispatchRow(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	seedDispatchRule(t, pool, "test.dispatch.mixed", "test")

	okID, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-mixed-ok",
		Type:        "fake-ok",
		Enabled:     true,
		Severities:  []alerting.Severity{},
		SourceTypes: []string{},
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert ok channel: %v", err)
	}
	failID, err := store.InsertChannel(ctx, alerting.Channel{
		Name:        "test-mixed-fail",
		Type:        "fake-fail",
		Enabled:     true,
		Severities:  []alerting.Severity{},
		SourceTypes: []string{},
		Config:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert fail channel: %v", err)
	}

	okFake := &fakeSender{retCode: 200}
	failFake := &fakeSender{retCode: 500, retErr: errors.New("boom")}
	reg := newTestRegistry()
	reg.Register("fake-ok", okFake)
	reg.Register("fake-fail", failFake)

	d := alerting.NewDispatcher(store, reg, silentLogger())

	alertID := seedAlertForDispatch(t, store, "test.dispatch.mixed", "test", "t:mixed", alerting.SeverityHigh)
	d.Dispatch(ctx, alerting.Alert{
		ID: alertID, RuleID: "test.dispatch.mixed", SourceType: "test",
		TargetKey: "t:mixed", Severity: alerting.SeverityHigh,
		State: alerting.StateFiring, Message: "mix",
	})

	if got := okFake.callCount(); got != 1 {
		t.Errorf("ok-fake: want 1 send, got %d", got)
	}
	if got := failFake.callCount(); got != 1 {
		t.Errorf("fail-fake: want 1 send, got %d", got)
	}

	rows, err := store.ListDispatchesByAlert(ctx, alertID)
	if err != nil {
		t.Fatalf("list dispatches: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 dispatch rows, got %d", len(rows))
	}

	byChannel := map[string]alerting.Dispatch{}
	for _, r := range rows {
		byChannel[r.ChannelID] = r
	}

	okRow, ok := byChannel[okID]
	if !ok {
		t.Fatalf("missing ok dispatch row for channel %s", okID)
	}
	if !okRow.Success {
		t.Errorf("ok row: want success=true, got false")
	}
	if okRow.StatusCode == nil || *okRow.StatusCode != 200 {
		t.Errorf("ok row: want statusCode=200, got %v", okRow.StatusCode)
	}
	if okRow.ErrorMsg != nil {
		t.Errorf("ok row: want nil errorMsg, got %q", *okRow.ErrorMsg)
	}

	failRow, ok := byChannel[failID]
	if !ok {
		t.Fatalf("missing fail dispatch row for channel %s", failID)
	}
	if failRow.Success {
		t.Errorf("fail row: want success=false, got true")
	}
	if failRow.StatusCode == nil || *failRow.StatusCode != 500 {
		t.Errorf("fail row: want statusCode=500, got %v", failRow.StatusCode)
	}
	if failRow.ErrorMsg == nil || *failRow.ErrorMsg != "boom" {
		t.Errorf("fail row: want errorMsg=boom, got %v", failRow.ErrorMsg)
	}
}
