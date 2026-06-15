package passthrough

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// TestValidate exercises the cross-toggle + reason + expiry invariants
// that every PUT endpoint relies on. These run server-side as the
// authoritative gate; the UI mirrors them client-side for instant
// feedback.
func TestValidate(t *testing.T) {
	now := time.Now()
	in1h := now.Add(1 * time.Hour)
	in7h := now.Add(7 * time.Hour)
	in9h := now.Add(9 * time.Hour)
	past := now.Add(-5 * time.Minute)
	const validReason = "incident-2026-05-13 anthropic api outage"

	cases := []struct {
		name     string
		in       payload
		wantOK   bool
		wantCode string
	}{
		{
			name:   "disabled never validates further",
			in:     payload{Enabled: false},
			wantOK: true,
		},
		{
			name:     "enabled with no flags rejected",
			in:       payload{Enabled: true, ExpiresAt: &in1h, Reason: validReason},
			wantOK:   false,
			wantCode: "passthrough_no_bypass_selected",
		},
		{
			name:     "bypassNormalize without bypassCache rejected",
			in:       payload{Enabled: true, BypassNormalize: true, ExpiresAt: &in1h, Reason: validReason},
			wantOK:   false,
			wantCode: "passthrough_normalize_requires_cache_bypass",
		},
		{
			name:   "bypassNormalize with bypassCache accepted",
			in:     payload{Enabled: true, BypassCache: true, BypassNormalize: true, ExpiresAt: &in1h, Reason: validReason},
			wantOK: true,
		},
		{
			name:     "missing expiresAt rejected",
			in:       payload{Enabled: true, BypassHooks: true, Reason: validReason},
			wantOK:   false,
			wantCode: "passthrough_invalid_expiry",
		},
		{
			name:     "expiresAt > NOW+8h rejected",
			in:       payload{Enabled: true, BypassHooks: true, ExpiresAt: &in9h, Reason: validReason},
			wantOK:   false,
			wantCode: "passthrough_invalid_expiry",
		},
		{
			name:     "expiresAt in the past rejected",
			in:       payload{Enabled: true, BypassHooks: true, ExpiresAt: &past, Reason: validReason},
			wantOK:   false,
			wantCode: "passthrough_invalid_expiry",
		},
		{
			name:     "reason < 20 chars rejected",
			in:       payload{Enabled: true, BypassHooks: true, ExpiresAt: &in1h, Reason: "too short"},
			wantOK:   false,
			wantCode: "passthrough_invalid_reason",
		},
		{
			name:   "valid bypass-hooks, max expiry, full reason accepted",
			in:     payload{Enabled: true, BypassHooks: true, ExpiresAt: &in7h, Reason: validReason},
			wantOK: true,
		},
		{
			name:   "valid all-three flags accepted",
			in:     payload{Enabled: true, BypassHooks: true, BypassCache: true, BypassNormalize: true, ExpiresAt: &in1h, Reason: validReason},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, code, ok := validate(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok mismatch: got=%v want=%v (msg=%q code=%q)", ok, tc.wantOK, msg, code)
			}
			if !ok && code != tc.wantCode {
				t.Fatalf("code mismatch: got=%q want=%q (msg=%q)", code, tc.wantCode, msg)
			}
		})
	}
}

// TestPassthroughActionCatalogAlignment pins the three IAM action
// strings the handler gates on to the canonical catalog form. Without
// this check, a rename of the verb constants in shared/iam silently
// breaks the passthrough route gates with no compile-time failure.
func TestPassthroughActionCatalogAlignment(t *testing.T) {
	cases := []struct {
		verb iam.Verb
		want string
	}{
		{iam.VerbRead, "admin:passthrough.read"},
		{iam.VerbWrite, "admin:passthrough.write"},
		{iam.VerbEmergencyEnable, "admin:passthrough.emergency-enable"},
	}
	for _, tc := range cases {
		got := iam.ResourcePassthrough.Action(tc.verb)
		if got != tc.want {
			t.Errorf("ResourcePassthrough.Action(%v) = %q, want %q", tc.verb, got, tc.want)
		}
	}
}

// TestConstants pins the handler invariants that are also encoded in
// the spec + SDD doc. If any of these change without updating the
// documentation, this test is the signal to keep doc + code aligned.
func TestConstants(t *testing.T) {
	if maxExpiry != 8*time.Hour {
		t.Errorf("maxExpiry drifted from documented 8h: %v", maxExpiry)
	}
	if minReasonLen != 20 {
		t.Errorf("minReasonLen drifted from documented 20: %d", minReasonLen)
	}
	if shadowKey != "gateway_passthrough" {
		t.Errorf("shadowKey drifted: %q", shadowKey)
	}
}

func TestPayload_ConfigJSON(t *testing.T) {
	p := payload{BypassHooks: true, BypassCache: false, BypassNormalize: true}
	raw, err := p.configJSON()
	if err != nil {
		t.Fatalf("configJSON err: %v", err)
	}
	want := `{"bypassCache":false,"bypassHooks":true,"bypassNormalize":true}`
	if string(raw) != want {
		t.Fatalf("configJSON = %q, want %q", string(raw), want)
	}
}

func TestPayload_FillFromJSONB(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want payload
	}{
		{"empty", "", payload{}},
		{"all true", `{"bypassHooks":true,"bypassCache":true,"bypassNormalize":true}`,
			payload{BypassHooks: true, BypassCache: true, BypassNormalize: true}},
		{"mixed", `{"bypassHooks":false,"bypassCache":true}`,
			payload{BypassCache: true}},
		{"corrupt JSON silently ignored", `{not json`, payload{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payload{}
			p.fillFromJSONB([]byte(tc.raw))
			if p != tc.want {
				t.Fatalf("got %+v, want %+v", p, tc.want)
			}
		})
	}
}

func TestNullableString(t *testing.T) {
	if v := nullableString(""); v != nil {
		t.Errorf("empty → %v, want nil", v)
	}
	if v := nullableString("x"); v != "x" {
		t.Errorf("non-empty → %v, want \"x\"", v)
	}
}

func TestApplyTierBypass(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want tierBlob
	}{
		{"empty leaves zero", "", tierBlob{}},
		{"all true", `{"bypassHooks":true,"bypassCache":true,"bypassNormalize":true}`,
			tierBlob{BypassHooks: true, BypassCache: true, BypassNormalize: true}},
		{"corrupt silently ignored", `{not json`, tierBlob{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := tierBlob{}
			applyTierBypass([]byte(tc.raw), &tb)
			if tb != tc.want {
				t.Fatalf("got %+v, want %+v", tb, tc.want)
			}
		})
	}
}

func TestErrJSON_EnvelopeShape(t *testing.T) {
	env := errJSON("msg", "type", "CODE")
	inner, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope missing 'error' map: %+v", env)
	}
	if inner["message"] != "msg" || inner["type"] != "type" || inner["code"] != "CODE" {
		t.Errorf("envelope inner = %+v", inner)
	}
}
