// Focused tests for the four decodeJSONB error branches that live
// at the bottom of every multi-column Thing read (GetThing,
// ValidateDeviceToken). Each branch is a separate row planting bad
// JSON in exactly one jsonb column.

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// validateDeviceTokenCols mirrors the column order in ValidateDeviceToken's
// SELECT.
var validateDeviceTokenCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
}

// validateDeviceTokenRow returns one valid row whose `column` jsonb
// field is replaced with `bad` so the corresponding decodeJSONB call
// surfaces an error.
func validateDeviceTokenRow(now time.Time, column string, bad []byte) []any {
	defaults := []any{
		"thing-1", "agent", "host", "1.0", "127.0.0.1:0",
		"sso", "bearer", "http",
		"online",
		[]byte(`{}`), // desired
		[]byte(`{}`), // reported
		int64(0), int64(0),
		[]byte(`{"deviceTokenHash":"h"}`), // metadata
		&now, now,
		[]byte(`{}`), // reported_outcomes
		(*time.Time)(nil),
	}
	idx := map[string]int{
		"desired": 9, "reported": 10, "metadata": 13, "reported_outcomes": 16,
	}[column]
	defaults[idx] = bad
	return defaults
}

// TestValidateDeviceToken_DecodeBranches covers each of the four
// decodeJSONB error returns inside ValidateDeviceToken — desired,
// reported, metadata, reported_outcomes.
func TestValidateDeviceToken_DecodeBranches(t *testing.T) {
	cases := []struct {
		col  string
		want string
	}{
		{"desired", "decode desired"},
		{"reported", "decode reported"},
		{"metadata", "decode metadata"},
		{"reported_outcomes", "decode reported_outcomes"},
	}
	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			mock, _ := pgxmock.NewPool()
			defer mock.Close()
			now := time.Now().UTC()
			mock.ExpectQuery(`FROM thing\s+WHERE id =`).
				WithArgs("thing-1", "h").
				WillReturnRows(pgxmock.NewRows(validateDeviceTokenCols).AddRow(
					validateDeviceTokenRow(now, tc.col, []byte("not json"))...,
				))

			store := New(mock)
			_, err := store.ValidateDeviceToken(context.Background(), "thing-1", "h")
			if err == nil {
				t.Fatalf("expected decode err for column %q", tc.col)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("missing prefix %q: %v", tc.want, err)
			}
		})
	}
}

// TestGetThing_DecodeBranches covers each remaining decodeJSONB
// error inside GetThing — reported, metadata, reported_outcomes
// (desired is already covered in thing_registry_get_heartbeat_test.go).
func TestGetThing_DecodeBranches(t *testing.T) {
	cases := []struct {
		col  string
		want string
	}{
		{"reported", "decode reported"},
		{"metadata", "decode metadata"},
		{"reported_outcomes", "decode reported_outcomes"},
	}
	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			mock, _ := pgxmock.NewPool()
			defer mock.Close()
			now := time.Now().UTC()
			row := []any{
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				[]byte(`{}`), // desired
				[]byte(`{}`), // reported
				int64(1), int64(1),
				[]byte(`{}`), // metadata
				&now, now,
				[]byte(`{}`), // reported_outcomes
				(*time.Time)(nil),
				"", "", "", "", "",
				"", "", "", "",
			}
			idx := map[string]int{
				"reported": 10, "metadata": 13, "reported_outcomes": 16,
			}[tc.col]
			row[idx] = []byte("not json")
			mock.ExpectQuery(`FROM thing t`).
				WithArgs("thing-1").
				WillReturnRows(pgxmock.NewRows(getThingCols).AddRow(row...))

			store := New(mock)
			_, err := store.GetThing(context.Background(), "thing-1")
			if err == nil {
				t.Fatalf("expected decode err for column %q", tc.col)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("missing prefix %q: %v", tc.want, err)
			}
		})
	}
}
