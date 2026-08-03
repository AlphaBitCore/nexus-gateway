package interception

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestValidateAdapterConfig tables the write-boundary shape check. The traffic
// snapshot unmarshals adapterConfig into map[string]any; a non-object value
// fails that parse and the whole domain is silently skipped (its traffic is no
// longer intercepted), so the write boundary must reject non-object shapes.
func TestValidateAdapterConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"nil", "", false},
		{"null", `null`, false},
		{"empty-object", `{}`, false},
		{"valid-object", `{"stripPaths":["/internal"]}`, false},
		{"array", `["x"]`, true},
		{"scalar", `42`, true},
		{"string", `"x"`, true},
		{"garbage", `not json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			got := validateAdapterConfig(raw)
			if (got != "") != tc.wantErr {
				t.Fatalf("validateAdapterConfig(%s) = %q; wantErr=%v", tc.raw, got, tc.wantErr)
			}
		})
	}
}

// TestCreateInterceptionDomain_BadAdapterConfig_Returns400 is the regression
// test for the silent-domain-skip bug: a non-object adapterConfig must be a
// loud 400 at create, not a domain the snapshot later drops with a warn log.
func TestCreateInterceptionDomain_BadAdapterConfig_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	h := newTestHandler(db, nil)
	body := mustJSONI(map[string]any{
		"name":          "example.com",
		"hostPattern":   "example.com",
		"adapterId":     "adapt-1",
		"adapterConfig": []any{"not-an-object"},
	})
	c, rec := echoCtxWith(http.MethodPost, "/interception-domains", body)
	if err := h.CreateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "adapterConfig must be a JSON object") {
		t.Errorf("missing shape message; body: %s", rec.Body.String())
	}
}

// TestUpdateInterceptionDomain_BadAdapterConfig_Returns400 covers the update
// wiring: a PATCH carrying a non-object adapterConfig must 400 and leave the
// existing domain untouched.
func TestUpdateInterceptionDomain_BadAdapterConfig_Returns400(t *testing.T) {
	db := newFakeInterceptionDB()
	seedDomain(db)
	h := newTestHandler(db, nil)
	c, rec := echoCtxWith(http.MethodPut, "/interception-domains/dom-1", `{"adapterConfig":[1,2]}`)
	c.SetParamNames("id")
	c.SetParamValues("dom-1")
	if err := h.UpdateInterceptionDomain(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "adapterConfig must be a JSON object") {
		t.Errorf("missing shape message; body: %s", rec.Body.String())
	}
}
