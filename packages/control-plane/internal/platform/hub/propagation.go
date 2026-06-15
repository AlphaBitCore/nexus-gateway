package hub

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
)

// This file is the single, truthful config-propagation surface shared by every
// security-sensitive admin handler. Before it existed, each handler
// re-implemented three things with subtle drift: the Category-A full-state push
// (NotifyConfigChange + nil-Hub no-op + ErrNotConfigured handling), the 502
// failure envelope, and the actor wiring. The accretion produced five distinct
// "DB committed but Hub push failed" policies (silent 200, 500, two 502 message
// variants). Routing every handler through PushTypeA
// + RespondPropagationFailure collapses that to one policy: the DB write (the
// source of truth) commits, a failed push surfaces to the admin as a 502, and
// the reconcile loop re-attempts propagation automatically.

// Actor is the authenticated admin identity stamped onto a config push so Hub
// can attribute the resulting config_change_event row.
type Actor struct {
	ID   string
	Name string
}

// ConfigNotifier is the narrow push surface PushTypeA needs. *Client satisfies
// it, and so do the per-handler HubConfigChanger interfaces (which declare the
// same single method), so a handler can pass its existing `h.hub` field
// directly.
type ConfigNotifier interface {
	NotifyConfigChange(ctx context.Context, req ConfigChangeRequest) (*ConfigChangeResponse, error)
}

// PushTypeA pushes a full-state Category-A config change (the whole assembled
// blob under one shadow key) to Hub and returns Hub's response.
//
// It is the single Category-A push path. Two "no Hub" conditions collapse to a
// silent success so dev/test writes do not fail spuriously:
//   - a nil notifier (Hub never wired into the handler) → (nil, nil);
//   - ErrNotConfigured (Hub base URL unset in local/dev) → (nil, nil).
//
// Any other error is returned verbatim so the caller can surface it via
// RespondPropagationFailure. The DB write that precedes this call is the source
// of truth and must already have committed; a push failure never rolls it back.
func PushTypeA(ctx context.Context, n ConfigNotifier, thingType, configKey string, state any, actor Actor) (*ConfigChangeResponse, error) {
	if isNilNotifier(n) {
		return nil, nil
	}
	resp, err := n.NotifyConfigChange(ctx, ConfigChangeRequest{
		ThingType: thingType,
		ConfigKey: configKey,
		State:     state,
		ActorID:   actor.ID,
		ActorName: actor.Name,
	})
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, nil
		}
		return resp, err
	}
	return resp, nil
}

// PropagationErrorJSON is the canonical 502 body returned whenever a config
// write committed to the CP database but propagation to the data plane failed.
// Every security-sensitive handler returns exactly this shape so the admin UI
// can render one consistent "saved but not yet propagated" state and the SIEM
// bridge keys off one stable code.
func PropagationErrorJSON(detail error) map[string]any {
	msg := "Config saved, but propagation to the data plane failed; verify Hub health and retry. The reconcile job also re-attempts propagation automatically."
	d := ""
	if detail != nil {
		d = detail.Error()
	}
	return map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "propagation_error",
			"code":    "HUB_PROPAGATION_FAILED",
			"detail":  d,
		},
	}
}

// RespondPropagationFailure writes the canonical 502 propagation envelope. It is
// the single responder every handler calls when a push fails, replacing the
// per-package hubPropagationError / hubPropagationErrorJSON copies.
func RespondPropagationFailure(c echo.Context, detail error) error {
	return c.JSON(http.StatusBadGateway, PropagationErrorJSON(detail))
}

// isNilNotifier reports whether the supplied notifier carries no usable value.
// A handler stores its Hub dependency in an interface field that is the untyped
// nil interface when unwired (production wiring always injects the concrete
// *Client, even when Hub itself is unconfigured — that case is handled by
// ErrNotConfigured inside PushTypeA), so a plain == nil check is sufficient.
func isNilNotifier(n ConfigNotifier) bool {
	return n == nil
}
