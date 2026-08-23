package virtualkey

import (
	"net/http"
	"testing"

	"strings"

	"github.com/goccy/go-json"
	"github.com/pashagolub/pgxmock/v4"
)

// TestValidateAllowedModels tables the write-boundary shape check. The gateway
// unmarshals VirtualKey.allowedModels into []store.AllowedModelRef ({providerId,
// modelId}); any other shape is persisted intact by CP and then 401s every
// request the VK makes with an opaque goccy parse error. The validator must
// accept exactly what the gateway can parse.
func TestValidateAllowedModels(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"nil", "", false},
		{"empty-array", `[]`, false},
		{"valid-single", `[{"providerId":"p1","modelId":"m1"}]`, false},
		{"valid-multi", `[{"providerId":"p1","modelId":"m1"},{"providerId":"p2","modelId":"m2"}]`, false},
		{"valid-extra-keys", `[{"providerId":"p1","modelId":"m1","note":"x"}]`, false},
		{"bare-strings", `["gpt-4.1-nano"]`, true},
		{"object-not-array", `{"providerId":"p","modelId":"m"}`, true},
		{"missing-modelId", `[{"providerId":"p1"}]`, true},
		{"empty-providerId", `[{"providerId":"","modelId":"m1"}]`, true},
		{"empty-modelId", `[{"providerId":"p1","modelId":""}]`, true},
		{"numbers", `[1,2]`, true},
		{"garbage", `not json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			got := validateAllowedModels(raw)
			if (got != "") != tc.wantErr {
				t.Fatalf("validateAllowedModels(%s) = %q; wantErr=%v", tc.raw, got, tc.wantErr)
			}
		})
	}
}

// TestCreateVirtualKey_RejectsBareStringAllowedModels is the regression test for
// the deferred-401 bug: a raw API caller sending allowedModels as an array of
// bare model-code strings must be rejected at create with a 400, not accepted
// and then 401'd at the gateway on every request.
func TestCreateVirtualKey_RejectsBareStringAllowedModels(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	// No INSERT expectation: validation must reject before touching the store.
	body := `{"name":"n","vkType":"personal","allowedModels":["gpt-4.1-nano"]}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/virtual-keys", body)
	if err := h.CreateVirtualKey(c); err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestCreateUserVirtualKey_RejectsBareStringAllowedModels covers the personal-VK
// create wiring (a distinct handler from the admin path).
func TestCreateUserVirtualKey_RejectsBareStringAllowedModels(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"mine","allowedModels":["gpt-4.1-nano"]}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/user/virtual-keys", body)
	if err := h.CreateUserVirtualKey(c); err != nil {
		t.Fatalf("CreateUserVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateVirtualKey_RejectsBareStringAllowedModels covers the update wiring,
// which re-marshals the body's `any` allowedModels back to JSON before the
// check — a different path than create. Rejection happens after the existing-VK
// lookup and BEFORE any UPDATE, so no UPDATE is expected on the mock.
func TestUpdateVirtualKey_RejectsBareStringAllowedModels(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "VirtualKey" WHERE id = \$1`).
		WithArgs("vk-1").
		WillReturnRows(pgxmock.NewRows(vkCols).AddRow(makeVKRow("vk-1", "old", strPtr("admin-1"))...))
	mock.ExpectQuery(`SELECT g.name`).
		WithArgs("nexus_user", "admin-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	body := `{"allowedModels":["gpt-4.1-nano"]}`
	c, rec := makeJSONReq(t, http.MethodPut, "/x", body)
	c.SetParamNames("id")
	c.SetParamValues("vk-1")
	if err := h.UpdateVirtualKey(c); err != nil {
		t.Fatalf("UpdateVirtualKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// A '*' in an allowed-model ref is refused at the write boundary.
//
// It is not a wildcard and never was in a way an operator could rely on: the
// gateway compares these refs exactly, and the model-access picker decides each
// checkbox by exact equality, so a pattern renders as "0 model(s) selected"
// while the operator believes they granted access. Accepting it would leave a
// key whose real permissions no admin screen can show — the dangerous direction
// for an authorisation boundary, since the UI understates rather than overstates.
func TestValidateAllowedModels_RefusesAWildcard(t *testing.T) {
	for _, body := range []string{
		`[{"providerId":"openai","modelId":"gpt-*"}]`,
		`[{"providerId":"*","modelId":"gpt-4"}]`,
		`[{"providerId":"openai","modelId":"gpt-4"},{"providerId":"anthropic","modelId":"claude-*"}]`,
	} {
		msg := validateAllowedModels(json.RawMessage(body))
		if msg == "" {
			t.Errorf("%s was accepted; a pattern the picker cannot render must not reach the column", body)
			continue
		}
		if !strings.Contains(msg, "concrete") {
			t.Errorf("the refusal should say what to do instead; got %q", msg)
		}
	}
}

// Concrete refs — the only thing the picker writes — still pass.
func TestValidateAllowedModels_AcceptsConcreteRefs(t *testing.T) {
	ok := `[{"providerId":"11111111-1111-1111-1111-111111111111","modelId":"22222222-2222-2222-2222-222222222222"}]`
	if msg := validateAllowedModels(json.RawMessage(ok)); msg != "" {
		t.Errorf("a concrete ref must be accepted, got %q", msg)
	}
}
