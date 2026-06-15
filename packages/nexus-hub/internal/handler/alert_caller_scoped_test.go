package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	alertclient "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// minRaiser is a minimal raiserAPI-compatible struct for testing alertCallerScoped.
// It satisfies the unexported interface structurally via Raise + Resolve methods.
type minRaiser struct {
	raised int
}

func (r *minRaiser) Raise(_ context.Context, _ alerting.RaiseInput) error {
	r.raised++
	return nil
}

func (r *minRaiser) Resolve(_ context.Context, _, _, _ string) error {
	return nil
}

// alertEnvelopeBody returns a POST body with a valid AlertEnvelope targeting targetKey.
func alertEnvelopeBody(t *testing.T, targetKey string) *bytes.Reader {
	t.Helper()
	env := alertclient.AlertEnvelope{
		RuleID:    "test.rule",
		TargetKey: targetKey,
		Severity:  "info",
		Message:   "test alert",
	}
	b, _ := json.Marshal(env)
	return bytes.NewReader(b)
}

// TestAlertCallerScoped_ServiceToken proves that when no Thing is set in the
// Echo context (service-token auth path), alertCallerScoped injects
// Caller{IsService: true} — meaning HandleRaise will not apply the per-Thing
// scope guard and will call the raiser unconditionally (F-0071).
func TestAlertCallerScoped_ServiceToken(t *testing.T) {
	raiser := &minRaiser{}
	// Use HandleRaise as the inner handler: if IsService=true the scope check is
	// skipped and raiser.Raise is called; if IsService=false with a mismatched
	// target we'd get 403 instead of 200.
	inner := alerting.HandleRaise(raiser)
	wrapper := alertCallerScoped(inner)

	e := echo.New()
	// No Thing set in context → service-token path.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise",
		alertEnvelopeBody(t, "org:other-thing"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// Deliberately do NOT set a Thing in the context.

	if err := wrapper(c); err != nil {
		t.Fatalf("alertCallerScoped returned error: %v", err)
	}
	// A service-token caller bypasses per-Thing scoping → raiser must be called.
	if raiser.raised != 1 {
		t.Errorf("raiser.raised = %d; want 1 (service caller is unrestricted)", raiser.raised)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 for service-token path", rec.Code)
	}
}

// TestAlertCallerScoped_DeviceToken proves that when a Thing IS set in the
// Echo context (device-token auth path), alertCallerScoped injects
// Caller{IsService: false, ThingID: thing.ID} — meaning HandleRaise will
// enforce per-Thing scoping and return 403 for a mismatched target (F-0071).
func TestAlertCallerScoped_DeviceToken(t *testing.T) {
	const thingID = "thing-n42"

	raiser := &minRaiser{}
	inner := alerting.HandleRaise(raiser)
	wrapper := alertCallerScoped(inner)

	e := echo.New()
	// Target is a DIFFERENT thing — with a device token this must be 403.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise",
		alertEnvelopeBody(t, "thing:other-node"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// Inject a Thing into the context to simulate device-token auth.
	c.Set("thing", &store.Thing{ID: thingID, Type: "agent"})

	if err := wrapper(c); err != nil {
		t.Fatalf("alertCallerScoped returned error: %v", err)
	}
	// Device caller targeting another thing → 403 (per-Thing scope guard).
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 for device-token caller targeting another thing", rec.Code)
	}
	// Raiser must NOT have been called.
	if raiser.raised != 0 {
		t.Errorf("raiser.raised = %d; want 0 (blocked by scope guard)", raiser.raised)
	}
}

// TestAlertCallerScoped_DeviceToken_OwnTarget proves that a device-token caller
// CAN raise an alert whose TargetKey references its own ThingID (F-0071 happy
// path): the scope guard must pass and the raiser must be invoked.
func TestAlertCallerScoped_DeviceToken_OwnTarget(t *testing.T) {
	const thingID = "thing-n42"

	raiser := &minRaiser{}
	inner := alerting.HandleRaise(raiser)
	wrapper := alertCallerScoped(inner)

	e := echo.New()
	// Target matches the authenticated Thing's ID — must succeed.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/raise",
		alertEnvelopeBody(t, "thing:"+thingID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	c.Set("thing", &store.Thing{ID: thingID, Type: "agent"})

	if err := wrapper(c); err != nil {
		t.Fatalf("alertCallerScoped returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 for device-token caller targeting own thing", rec.Code)
	}
	if raiser.raised != 1 {
		t.Errorf("raiser.raised = %d; want 1 (own-target allowed)", raiser.raised)
	}
}
