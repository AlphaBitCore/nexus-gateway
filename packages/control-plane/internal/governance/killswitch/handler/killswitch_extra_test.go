package killswitch

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

// errorProducer is a mq.Producer whose Enqueue always returns an error.
// Injecting it into audit.NewWriter makes LogCritical return that error,
// exercising the fail-closed audit failure branch inside Post.
type errorProducer struct{}

func (p *errorProducer) Publish(_ context.Context, _ string, _ []byte) error {
	return errors.New("audit enqueue failed (test)")
}

func (p *errorProducer) Enqueue(_ context.Context, _ string, _ []byte) error {
	return errors.New("audit enqueue failed (test)")
}

func (p *errorProducer) Close() error { return nil }

// newHandlerWithAuditError builds a Handler whose audit Writer always fails
// on LogCritical — used by the audit-failure branch tests.
func newHandlerWithAuditError(t *testing.T, fh *fakeHub) *Handler {
	t.Helper()
	if fh != nil && fh.resp == nil && fh.err == nil {
		fh.resp = &hub.ConfigChangeResponse{Version: 1, ThingsNotified: 1, ThingsOnline: 1}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var h HubConfigChanger
	if fh != nil {
		h = fh
	}
	return New(Deps{
		Hub:    h,
		Audit:  audit.NewWriter(&errorProducer{}, "audit", logger),
		Logger: logger,
	})
}

// TestPost_AuditFailureReturns500 covers the fail-closed audit path: when
// the admin audit MQ enqueue fails AFTER a successful Hub fan-out, Post must
// return 500 with AUDIT_FAILURE code. This is the fail-closed
// discipline — a kill-switch toggle without an audit row is unacceptable, so
// the CP rejects the request rather than silently succeeding.
func TestPost_AuditFailureReturns500(t *testing.T) {
	fh := &fakeHub{}
	h := newHandlerWithAuditError(t, fh)

	body, _ := json.Marshal(map[string]any{"engaged": true})
	c, rec := newAdminContext(t, http.MethodPost, "/api/admin/compliance/killswitch", body)

	_ = h.Post(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on audit failure, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "AUDIT_FAILURE" {
		t.Errorf("error code = %q; want AUDIT_FAILURE", env.Error.Code)
	}
}
