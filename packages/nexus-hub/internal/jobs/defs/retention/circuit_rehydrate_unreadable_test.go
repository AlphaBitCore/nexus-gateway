package retention

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// A row the rehydrate pass cannot scan is an open circuit the gateway goes on
// treating as CLOSED. Skipping it is right — failing the whole pass over one
// bad row would leave every open circuit un-rehydrated rather than one — but
// skipping it invisibly is not.
//
// This replaces a sentinel test that asserted nothing and recorded the branch
// as "not exercisable via pgxmock because pgxmock Scan always succeeds if
// types match". That is true only while every destination is a string. Two of
// the five here are *time.Time, and a non-time value in one of those columns
// makes Scan fail on demand.
func TestRehydrate_UnreadableRowIsCountedNotSwallowed(t *testing.T) {
	_, rdb := newMiniredisRdb(t)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	cols := []string{"id", "circuitState", "reason", "circuitOpenedAt", "circuitNextProbeAt"}
	// "not-a-time" into the *time.Time destination → Scan fails for this row.
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("cred-unreadable", "open", "auth_fail", "not-a-time", nil))

	var logs strings.Builder
	j := &CredentialCircuitFlushJob{
		pool:   mock,
		rdb:    rdb,
		hubID:  "hub-unreadable",
		logger: captureLogger(&logs),
	}

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run must not fail the whole pass over one bad row: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "scan persisted row") {
		t.Errorf("the cause was not logged; an operator gets a count with no reason. log=%q", out)
	}
	// The summary is the part that was missing: its guard counted only the four
	// success-ish outcomes, so a pass where EVERY row was unreadable said
	// nothing at Info level at all.
	if !strings.Contains(out, "circuit state rehydrated from DB") {
		t.Errorf("a pass whose rows were all unreadable produced no summary line at all — "+
			"the guard counts outcomes that did not happen. log=%q", out)
	}
	if !strings.Contains(out, "unreadable=1") {
		t.Errorf("the summary does not carry the unreadable count. log=%q", out)
	}
}
