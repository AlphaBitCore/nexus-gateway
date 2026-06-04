package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestClient_Do_EmptyBodyOn2xxSucceeds guards the do() empty-2xx-body fix: a success
// with no body (an empty 200 or a 204) must decode as a zero-value result, never as a
// bogus ErrTransport from unmarshalling "". SetKillSwitch passes a non-nil out, so it
// exercises the decode arm.
func TestClient_Do_EmptyBodyOn2xxSucceeds(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent} {
		c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code) // intentionally no body
		}))
		res, err := c.SetKillSwitch(context.Background(), true)
		done()
		if err != nil {
			t.Fatalf("empty %d body must succeed, got err=%v", code, err)
		}
		if res == nil {
			t.Fatalf("empty %d body should yield a zero-value result, not nil", code)
		}
	}
}

// TestClient_AdminRequest_StatusPassthrough covers AdminRequest — the single execution
// path behind every generic resource write (the OpenAPI engine, the cli resource
// command, the TUI cascade). The contract is that it surfaces the raw body + HTTP
// status so a 400/403 teaches the model/operator to self-correct rather than aborting.
func TestClient_AdminRequest_StatusPassthrough(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/ok":
			_, _ = w.Write([]byte(`{"data":[{"id":"x"}]}`))
		case "/api/admin/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"forbidden","action":"provider.update"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	// 200: raw body + status returned verbatim, no error.
	raw, status, err := c.AdminRequest(context.Background(), http.MethodGet, "/api/admin/ok", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("AdminRequest 200: status=%d err=%v", status, err)
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &got) != nil || len(got.Data) != 1 || got.Data[0].ID != "x" {
		t.Fatalf("AdminRequest 200 raw body not relayed: %s", raw)
	}

	// 403: the status is surfaced AND classified as ErrForbidden so callers map it to
	// the documented exit code / a recoverable tool result.
	_, status, err = c.AdminRequest(context.Background(), http.MethodPut, "/api/admin/denied", nil, map[string]any{"enabled": false})
	if status != http.StatusForbidden {
		t.Fatalf("AdminRequest 403: status=%d", status)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("AdminRequest 403: err must classify as ErrForbidden, got %v", err)
	}
}
