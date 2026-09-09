package manager

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// capturingLogger returns a logger writing into buf, so a test can assert on
// what an operator would actually see rather than on an internal flag.
func capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// registerOnce drives one full RegisterThing through the enrollment path with
// the given type, and returns everything the logger emitted.
func registerOnce(t *testing.T, thingType string) string {
	t.Helper()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	var buf bytes.Buffer
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", capturingLogger(&buf))

	// An unrecognised type legitimately has no templates — that is precisely
	// why it is silent today, so the fixture returns none for either case and
	// the difference has to come from the guard, not from the data.
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs(thingType).
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("t-1", "1.0", "addr", "").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	mock.ExpectExec(`INSERT INTO thing\s*\(`).
		WithArgs(
			"t-1", thingType, "t-1", "1.0", "addr",
			"", "bearer", "http", "online",
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(0), nil,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", thingType, map[string]any{}, 0))

	if _, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "t-1", Type: thingType, Version: "1.0", Address: "addr",
	}); err != nil {
		t.Fatalf("RegisterThing(%q): %v", thingType, err)
	}
	return buf.String()
}

// The defect, pinned in its own name. A service whose configured ThingType has
// a typo registers, reports ONLINE, and receives no configuration for the life
// of the process — GetConfigTemplates answers an unknown type with zero
// templates and a nil error, so nothing fails. Everything an operator can see
// says the node is healthy.
func TestRegisterThing_UnknownTypeIsLoggedAtError(t *testing.T) {
	out := registerOnce(t, "ai-gatewy") // one letter short of ai-gateway

	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("an unrecognised type must be reported at ERROR — from here the Thing "+
			"is unconfigured and has no other way to say so; got:\n%s", out)
	}
	if !strings.Contains(out, "ai-gatewy") {
		t.Errorf("the log must name the offending type, or an operator cannot find the typo; got:\n%s", out)
	}
	if !strings.Contains(out, "ai-gateway") {
		t.Errorf("the log must name the valid set, so it says what the value should have been "+
			"rather than only that it was wrong; got:\n%s", out)
	}
}

// The other half of the same decision. Every recognised type must stay quiet,
// or the signal is worthless: an ERROR on every normal registration is one an
// operator learns to ignore.
func TestRegisterThing_KnownTypesAreSilent(t *testing.T) {
	for _, ty := range []string{"nexus-hub", "control-plane", "ai-gateway", "compliance-proxy", "agent"} {
		out := registerOnce(t, ty)
		if strings.Contains(out, "unrecognised type") {
			t.Errorf("%s is a recognised Thing type and must not warn; got:\n%s", ty, out)
		}
	}
}

// Admitted, not refused. Rejecting an unknown type would break a rolling deploy
// that introduces a new Thing type before Hub is updated — the same hazard the
// ws authenticator already reasons about for unknown statuses. A fleet that
// cannot connect is worse than one that logs.
func TestRegisterThing_UnknownTypeStillRegisters(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	var buf bytes.Buffer
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", capturingLogger(&buf))

	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("brand-new-type").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("t-2", "1.0", "addr", "").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-2").
		WillReturnRows(minimalGetThingRow("t-2", "brand-new-type", map[string]any{}, 0))

	resp, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "t-2", Type: "brand-new-type", Version: "1.0", Address: "addr",
	})
	if err != nil {
		t.Fatalf("an unrecognised type must still register, not be refused: %v", err)
	}
	if resp == nil {
		t.Fatal("registration returned no response")
	}
	if !strings.Contains(buf.String(), "unrecognised type") {
		t.Error("...but it must not do so quietly")
	}
}
