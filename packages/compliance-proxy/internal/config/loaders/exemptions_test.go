package loaders

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// exemptionGrantCols mirrors the SELECT column list. Stays in sync with
// activeExemptionGrantSelect in exemptions.go.
var exemptionGrantCols = []string{
	"id", "source_ip", "target_host", "reason", "approved_by", "inactive",
	"effective_from", "expires_at",
}

// TestLoadActiveExemptions_NilDBReturnsEmpty pins the nil-DB short-circuit:
// the receiver path in configdispatch may call this loader during boot or
// in tests where ConfigDB is not yet wired; that must yield an empty slice
// with no error so Rebuild safely clears the in-memory store.
func TestLoadActiveExemptions_NilDBReturnsEmpty(t *testing.T) {
	got, err := LoadActiveExemptions(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil DB must NOT error; got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

// TestLoadActiveExemptions_QueryErrorWrapped — a transient DB error must
// propagate with an attribution prefix so service logs surface the
// failure source. Returning nil slice is acceptable because the caller
// (configdispatch handler) reports the err back to Hub and skips the
// Rebuild, preserving the prior in-memory snapshot.
func TestLoadActiveExemptions_QueryErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	want := errors.New("planner err")
	mock.ExpectQuery(activeExemptionGrantSelect).WillReturnError(want)
	_, err := LoadActiveExemptions(context.Background(), db)
	if err == nil {
		t.Fatal("err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("err must wrap original via %%w; got: %v", err)
	}
	if !strings.Contains(err.Error(), "exemptions: query compliance_exemption_grant") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

// TestLoadActiveExemptions_ScanErrorWrapped — column count mismatch must
// surface a scan error with attribution prefix.
func TestLoadActiveExemptions_ScanErrorWrapped(t *testing.T) {
	db, mock := newSQLMock(t)
	// Only one column where the scan expects 8 — forces scan err.
	mock.ExpectQuery(activeExemptionGrantSelect).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("x"))
	_, err := LoadActiveExemptions(context.Background(), db)
	if err == nil {
		t.Fatal("scan err must surface")
	}
	if !strings.Contains(err.Error(), "exemptions: scan compliance_exemption_grant") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

// TestLoadActiveExemptions_RowsErrPropagates — rows.Err() after iteration
// must surface so transient mid-stream errors aren't silently swallowed.
func TestLoadActiveExemptions_RowsErrPropagates(t *testing.T) {
	db, mock := newSQLMock(t)
	now := time.Now().UTC()
	mock.ExpectQuery(activeExemptionGrantSelect).
		WillReturnRows(sqlmock.NewRows(exemptionGrantCols).
			AddRow("g1", "10.0.0.1", "api.openai.com", "ticket-42", "alice", false,
				now.Add(-time.Hour), now.Add(time.Hour)).
			RowError(0, errors.New("iter broke")))
	_, err := LoadActiveExemptions(context.Background(), db)
	if err == nil {
		t.Fatal("rows.Err must propagate")
	}
	if !strings.Contains(err.Error(), "exemptions: iterate compliance_exemption_grant") {
		t.Errorf("attribution prefix missing: %v", err)
	}
}

// TestLoadActiveExemptions_EmptyResultReturnsEmptySlice — a fresh deploy
// with zero active grants must return a non-nil empty slice (so the
// receiver's Rebuild call wipes any stale in-memory entries instead of
// no-opping on nil input).
func TestLoadActiveExemptions_EmptyResultReturnsEmptySlice(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectQuery(activeExemptionGrantSelect).
		WillReturnRows(sqlmock.NewRows(exemptionGrantCols))
	got, err := LoadActiveExemptions(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries, got %d", len(got))
	}
}

// TestLoadActiveExemptions_HappyPath — verifies the canonical projection
// from DB columns to the identity.ActiveExemption wire shape: timestamps
// formatted RFC3339 in UTC, ApprovedBy threaded through, Disabled mapped
// from `inactive`.
func TestLoadActiveExemptions_HappyPath(t *testing.T) {
	db, mock := newSQLMock(t)
	effective := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery(activeExemptionGrantSelect).
		WillReturnRows(sqlmock.NewRows(exemptionGrantCols).
			AddRow("g1", "10.0.0.1", "api.openai.com", "ticket-42", "alice", false,
				effective, expires).
			AddRow("g2", "10.0.0.2", "*.anthropic.com", "vendor-outage", "bob", true,
				effective, expires))
	got, err := LoadActiveExemptions(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].ID != "g1" || got[0].SourceIP != "10.0.0.1" || got[0].TargetHost != "api.openai.com" {
		t.Errorf("row 0 fields wrong: %+v", got[0])
	}
	if got[0].Reason != "ticket-42" || got[0].ApprovedBy != "alice" {
		t.Errorf("row 0 reason/approvedBy wrong: %+v", got[0])
	}
	if got[0].Disabled {
		t.Errorf("row 0 must map inactive=false → Disabled=false; got %+v", got[0])
	}
	if got[0].ExpiresAt != "2026-05-19T14:00:00Z" {
		t.Errorf("ExpiresAt formatting drifted: %q", got[0].ExpiresAt)
	}
	if got[0].EffectiveFrom != "2026-05-19T12:00:00Z" {
		t.Errorf("EffectiveFrom formatting drifted: %q", got[0].EffectiveFrom)
	}
	if !got[1].Disabled {
		t.Errorf("row 1 must map inactive=true → Disabled=true; got %+v", got[1])
	}
}
