package exemption

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// A compliance exemption is a bypass of the interception this product exists to
// perform. `PostGrant` has always bounded it at 7 days. The employee REQUEST
// path did not bound it at all, and approval computes `expires_at` from that
// stored number — so a routine approve click could mint a bypass measured in
// years for one source IP × target host.
//
// These arms pin the ceiling at BOTH ends: the submit that creates the number,
// and the approve that turns it into a live grant.

// overLongDurations are values the request endpoint accepted. 10081 is one
// minute past the ceiling — the arm that catches an off-by-one in the bound.
var overLongDurations = []float64{10081, 525600, 5256000, 1e9}

func TestCreateRequest_OverCeiling_RefusedAndNothingPersisted(t *testing.T) {
	for _, d := range overLongDurations {
		t.Run(fmt.Sprintf("%.0f", d), func(t *testing.T) {
			mock, h := newHandlerWithDB(t, nil)
			// No ExpectQuery: pgxmock fails the test if the handler issues a
			// query it was not told to expect, so this also proves the refusal
			// lands before the INSERT.
			body := fmt.Sprintf(
				`{"transactionId":"tx-1","sourceIp":"10.0.0.1","targetHost":"x.com","reason":"need access","durationMinutes":%.0f}`, d)
			c, rec := ctxWithAuth(http.MethodPost, "/", body)
			if err := h.CreateRequest(c); err != nil {
				t.Fatalf("CreateRequest: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code=%d want 400 for durationMinutes=%.0f; body=%s", rec.Code, d, rec.Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet/extra DB expectations: %v", err)
			}
		})
	}
}

func TestCreateRequest_NonPositiveOrFractional_Refused(t *testing.T) {
	// `int(durMin)` truncates: 0.5 becomes 0 (a grant expiring the instant
	// it is approved) and 10080.9 becomes a value the caller never sent.
	for _, raw := range []string{"0", "-60", "0.5", "10080.9"} {
		t.Run(raw, func(t *testing.T) {
			mock, h := newHandlerWithDB(t, nil)
			body := `{"transactionId":"tx-1","sourceIp":"10.0.0.1","targetHost":"x.com","reason":"need access","durationMinutes":` + raw + `}`
			c, rec := ctxWithAuth(http.MethodPost, "/", body)
			if err := h.CreateRequest(c); err != nil {
				t.Fatalf("CreateRequest: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code=%d want 400 for durationMinutes=%s; body=%s", rec.Code, raw, rec.Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet/extra DB expectations: %v", err)
			}
		})
	}
}

// The sibling: a value AT the ceiling still goes through, so "refuse
// everything" would not satisfy the arms above.
func TestCreateRequest_AtCeiling_Accepted(t *testing.T) {
	mock, h := newHandlerWithDB(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO exemption_request`).
		WithArgs("tx-1", "10.0.0.1", "x.com", "need access", maxExemptionMinutes, "employee").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "transaction_id", "source_ip", "target_host", "reason", "status",
			"duration_minutes", "reviewed_by", "review_note", "reviewed_at", "createdAt", "requested_by",
		}).AddRow(
			"er-1", "tx-1", "10.0.0.1", "x.com", "need access", "PENDING",
			maxExemptionMinutes, (*string)(nil), (*string)(nil), (*time.Time)(nil), now, "employee",
		))
	body := fmt.Sprintf(
		`{"transactionId":"tx-1","sourceIp":"10.0.0.1","targetHost":"x.com","reason":"need access","durationMinutes":%d}`,
		maxExemptionMinutes)
	c, rec := ctxWithAuth(http.MethodPost, "/", body)
	if err := h.CreateRequest(c); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201 at the ceiling; body=%s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestApproveRequest_LegacyOverCeilingRow_Refused covers the rows that already
// exist. The submit-side bound cannot reach them: they were written before it
// existed, and approval is the moment they become a live bypass.
func TestApproveRequest_LegacyOverCeilingRow_Refused(t *testing.T) {
	for _, d := range []int{10081, 5256000, 0, -1} {
		t.Run(fmt.Sprintf("%d", d), func(t *testing.T) {
			data := &fakeData{
				getExReq:     &store.ExemptionRequest{ID: "er-1", Status: "PENDING", DurationMinutes: d},
				approveGrant: &store.ComplianceExemptionGrant{ID: "g-1"},
			}
			h := newHandler(data, &fakeHub{})
			c, rec := ctxWithAuth(http.MethodPost, "/", "")
			if err := h.ApproveRequest(withParam(c, "id", "er-1")); err != nil {
				t.Fatalf("ApproveRequest: %v", err)
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("code=%d want 422 for stored duration %d; body=%s", rec.Code, d, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "DURATION_OUT_OF_RANGE") {
				t.Errorf("body=%s want DURATION_OUT_OF_RANGE", rec.Body)
			}
			if data.approveHits != 0 {
				t.Error("the grant transaction ran despite the refusal")
			}
		})
	}
}

// The sibling: an in-range stored row still approves, so the arm above is not
// passing because approve is broken for everything.
func TestApproveRequest_InRangeRow_StillApproves(t *testing.T) {
	data := &fakeData{
		getExReq:     &store.ExemptionRequest{ID: "er-1", Status: "PENDING", DurationMinutes: 240},
		approveGrant: &store.ComplianceExemptionGrant{ID: "g-1"},
	}
	h := newHandler(data, &fakeHub{})
	c, rec := ctxWithAuth(http.MethodPost, "/", "")
	if err := h.ApproveRequest(withParam(c, "id", "er-1")); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	if data.approveHits != 1 {
		t.Errorf("approveHits=%d, want 1 — an in-range request was not approved", data.approveHits)
	}
}
