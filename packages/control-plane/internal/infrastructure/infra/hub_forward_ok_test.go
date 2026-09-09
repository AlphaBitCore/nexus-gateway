package infra

import (
	"net/http"
	"testing"
	"time"
)

// hubForward's bool is the whole point of the signature change: the callers
// that write an audit row used to read c.Response().Status back off the
// response writer, because the helper knew whether Hub had accepted the
// request and would not say. Every refusal it reports travels as a written
// response with a nil error, so `if err != nil` could never have carried it.
//
// The table drives the arms an operator's audit trail actually depends on: a
// 2xx must be reported true so the row is written, and each refusal shape must
// be reported false so it is not.
func TestHubForward_OkReportsTheUpstreamVerdict(t *testing.T) {
	cases := []struct {
		name      string
		hubStatus int
		wantOK    bool
		wantRelay int
	}{
		{"hub accepted", http.StatusOK, true, http.StatusOK},
		{"hub accepted, no content", http.StatusNoContent, true, http.StatusNoContent},
		{"hub rejected the request", http.StatusBadRequest, false, http.StatusBadRequest},
		{"hub refused authorisation", http.StatusForbidden, false, http.StatusForbidden},
		{"hub failed", http.StatusInternalServerError, false, http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.hubStatus)
			}))
			h := newHandler(t, nil, hub, nil)
			c, rec := echoCtx(http.MethodPost, "/", "", true)

			ok, err := h.hubForward(c, http.MethodPost, "/api/hub/things", nil)
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v — an audit row is written on ok, so getting this "+
					"wrong either loses the record or fabricates one for a request Hub refused",
					ok, tc.wantOK)
			}
			if rec.Code != tc.wantRelay {
				t.Errorf("relayed status = %d, want %d", rec.Code, tc.wantRelay)
			}
		})
	}
}

// The refusals hubForward generates itself, rather than relaying. These are
// the arms where the old `if err != nil { return err }` looked like it was
// doing something: err is nil here too, because c.JSON succeeded.
func TestHubForward_SelfGeneratedRefusalsReportNotOk(t *testing.T) {
	t.Run("hub not configured", func(t *testing.T) {
		h := newHandler(t, nil, nil, nil)
		c, rec := echoCtx(http.MethodGet, "/", "", true)
		ok, err := h.hubForward(c, http.MethodGet, "/api/hub/things", nil)
		if err != nil {
			t.Fatalf("err = %v, want nil: the refusal travels as a written response", err)
		}
		if ok {
			t.Error("ok = true with no Hub configured")
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", rec.Code)
		}
	})

	t.Run("hub unreachable", func(t *testing.T) {
		hub := &fakeHub{baseURL: "http://127.0.0.1:1", token: "tok"}
		h := newHandler(t, nil, hub, nil)
		h.hubProxyClientRef = &http.Client{Timeout: 100 * time.Millisecond}
		c, rec := echoCtx(http.MethodGet, "/", "", true)
		ok, err := h.hubForward(c, http.MethodGet, "/api/hub/things", nil)
		if err != nil {
			t.Fatalf("err = %v, want nil: the refusal travels as a written response", err)
		}
		if ok {
			t.Error("ok = true with Hub unreachable")
		}
		if rec.Code != http.StatusBadGateway {
			t.Errorf("code = %d, want 502", rec.Code)
		}
	})
}
