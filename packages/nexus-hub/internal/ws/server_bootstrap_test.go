package ws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// dialSvc performs a service-token upgrade for the given id and returns the
// HTTP status of the handshake response.
func dialSvc(t *testing.T, fv *fakeValidator, id string) int {
	t.Helper()
	pool := NewPool(nil, nullLogger())
	srv := newServerWithDeps(pool, &fakeManager{}, fv, "hub-1", testServiceToken, nil, false, nullLogger())
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?id=" + id + "&type=ai-gateway"
	conn, resp, _ := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + testServiceToken}},
		Subprotocols: []string{"nexus.bearer"},
	})
	if conn != nil {
		defer conn.Close(websocket.StatusNormalClosure, "")
	}
	if resp == nil {
		t.Fatal("no handshake response")
	}
	return resp.StatusCode
}

// The defect, pinned in its own name. A service whose row does not exist yet
// must be able to enrol over the handshake. Refusing it made WS unable to
// bootstrap a Thing at all: the client's auth-rejected branch waits and retries
// WS forever without ever reaching the HTTP fallback that could create the row,
// so a service booting against a healthy Hub — or one whose row an operator
// deleted — stayed off the fleet permanently. It only looked like it worked
// because at first boot Hub is usually starting too, and THAT dial fails with
// connection-refused, which does count toward the fallback threshold.
func TestAuthenticate_ServiceToken_NeverEnrolledThingIsAdmittedSoItCanRegister(t *testing.T) {
	fv := &fakeValidator{statusErr: store.ErrNotFound}
	if code := dialSvc(t, fv, "brand-new-svc"); code == http.StatusUnauthorized {
		t.Error("a Thing with no row must be admitted so RegisterThing can enrol it; " +
			"the HTTP register path already grants this same credential exactly that")
	}
}

// The security property the missing-row case must not cost. A revoked Thing is
// deliberately barred and stays barred.
func TestAuthenticate_ServiceToken_RevokedThingStaysRejected(t *testing.T) {
	fv := &fakeValidator{thingStatuses: map[string]string{"revoked-svc": "revoked"}}
	if code := dialSvc(t, fv, "revoked-svc"); code != http.StatusUnauthorized {
		t.Errorf("a revoked Thing must not re-promote itself to online on reconnect; got %d", code)
	}
}

// The other security property: an unreadable registry is not a licence to
// admit. Only ErrNotFound is treated as "never enrolled"; every other error
// still fails closed.
func TestAuthenticate_ServiceToken_RegistryErrorStillFailsClosed(t *testing.T) {
	fv := &fakeValidator{statusErr: errors.New("connection reset by peer")}
	if code := dialSvc(t, fv, "some-svc"); code != http.StatusUnauthorized {
		t.Errorf("a registry read that failed for an unknown reason must fail closed, "+
			"not be mistaken for a first enrollment; got %d", code)
	}
}

// A Thing in any ordinary status keeps connecting. Pinned so a future
// exclusionary status is added to the reject arm deliberately rather than by
// accidentally narrowing this one.
func TestAuthenticate_ServiceToken_OrdinaryStatusesAreAdmitted(t *testing.T) {
	for _, st := range []string{"online", "offline", "enrolled", "drift"} {
		fv := &fakeValidator{thingStatuses: map[string]string{"svc": st}}
		if code := dialSvc(t, fv, "svc"); code == http.StatusUnauthorized {
			t.Errorf("status %q must be admitted; got 401", st)
		}
	}
}

// The boundary, pinned so the service-token carve-out is never widened to
// agents. A service holds the shared internal token and is trusted to enrol
// itself; an agent presents a device token that must already be stored against
// its row, so a missing row means the token cannot be validated and the
// handshake is refused. Auto-registration is for services only.
func TestAuthenticate_DeviceToken_UnknownAgentIsStillRejected(t *testing.T) {
	pool := NewPool(nil, nullLogger())
	// No stored thing: ValidateDeviceToken fails the way a missing row does.
	fv := &fakeValidator{err: store.ErrNotFound}
	srv := newServerWithDeps(pool, &fakeManager{}, fv, "hub-1", testServiceToken, nil, false, nullLogger())
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleUpgrade))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?id=ghost-agent"
	conn, resp, _ := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		// A device token, NOT the service token — this is the agent path.
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer some-device-token"}},
		Subprotocols: []string{"nexus.bearer"},
	})
	if conn != nil {
		defer conn.Close(websocket.StatusNormalClosure, "")
	}
	if resp == nil {
		t.Fatal("no handshake response")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an agent with no row must be refused — enrolment for agents goes through "+
			"the device-token issuance flow, not a self-service handshake; got %d", resp.StatusCode)
	}
}
