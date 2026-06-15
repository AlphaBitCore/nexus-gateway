package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// fakeNotifier records the request it received and returns canned values so the
// PushTypeA tests can assert request construction + the no-op / error mapping.
type fakeNotifier struct {
	resp    *ConfigChangeResponse
	err     error
	calls   int
	lastReq ConfigChangeRequest
}

func (f *fakeNotifier) NotifyConfigChange(_ context.Context, req ConfigChangeRequest) (*ConfigChangeResponse, error) {
	f.calls++
	f.lastReq = req
	return f.resp, f.err
}

func TestPushTypeA_nilNotifierIsNoOp(t *testing.T) {
	resp, err := PushTypeA(context.Background(), nil, "ai-gateway", "cache", map[string]any{"x": 1}, Actor{ID: "u", Name: "n"})
	if err != nil || resp != nil {
		t.Fatalf("nil notifier: got resp=%v err=%v, want nil/nil", resp, err)
	}
}

func TestPushTypeA_errNotConfiguredIsNoOp(t *testing.T) {
	n := &fakeNotifier{err: ErrNotConfigured}
	resp, err := PushTypeA(context.Background(), n, "ai-gateway", "cache", nil, Actor{})
	if err != nil || resp != nil {
		t.Fatalf("ErrNotConfigured: got resp=%v err=%v, want nil/nil", resp, err)
	}
	if n.calls != 1 {
		t.Fatalf("expected the push to be attempted once, got %d", n.calls)
	}
}

func TestPushTypeA_successBuildsRequestAndReturnsResponse(t *testing.T) {
	n := &fakeNotifier{resp: &ConfigChangeResponse{OK: true, Version: 7}}
	state := map[string]any{"global": true}
	resp, err := PushTypeA(context.Background(), n, "ai-gateway", "gateway_passthrough", state, Actor{ID: "admin-1", Name: "Ada"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Version != 7 {
		t.Fatalf("response = %+v, want Version=7", resp)
	}
	got := n.lastReq
	if got.ThingType != "ai-gateway" || got.ConfigKey != "gateway_passthrough" {
		t.Fatalf("request target = %s/%s", got.ThingType, got.ConfigKey)
	}
	if got.ActorID != "admin-1" || got.ActorName != "Ada" {
		t.Fatalf("actor = %s/%s, want admin-1/Ada", got.ActorID, got.ActorName)
	}
	if got.State == nil {
		t.Fatalf("state not forwarded")
	}
}

func TestPushTypeA_propagatesRealError(t *testing.T) {
	boom := errors.New("hub down")
	n := &fakeNotifier{err: boom}
	_, err := PushTypeA(context.Background(), n, "ai-gateway", "cache", nil, Actor{})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want hub down", err)
	}
}

func TestPropagationErrorJSON_shape(t *testing.T) {
	out := PropagationErrorJSON(errors.New("down"))
	inner, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", out)
	}
	if inner["type"] != "propagation_error" {
		t.Errorf("type = %v, want propagation_error", inner["type"])
	}
	if inner["code"] != "HUB_PROPAGATION_FAILED" {
		t.Errorf("code = %v, want HUB_PROPAGATION_FAILED", inner["code"])
	}
	if inner["detail"] != "down" {
		t.Errorf("detail = %v, want down", inner["detail"])
	}
	if inner["message"] == "" {
		t.Errorf("message must be non-empty")
	}
}

func TestPropagationErrorJSON_nilDetail(t *testing.T) {
	out := PropagationErrorJSON(nil)
	inner := out["error"].(map[string]any)
	if inner["detail"] != "" {
		t.Errorf("nil detail = %v, want empty string", inner["detail"])
	}
}

func TestRespondPropagationFailure_writes502Envelope(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPut, "/", nil), rec)

	if err := RespondPropagationFailure(c, errors.New("boom")); err != nil {
		t.Fatalf("RespondPropagationFailure returned err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body struct {
		Error struct {
			Type   string `json:"type"`
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if body.Error.Type != "propagation_error" || body.Error.Code != "HUB_PROPAGATION_FAILED" || body.Error.Detail != "boom" {
		t.Fatalf("envelope = %+v", body.Error)
	}
}
