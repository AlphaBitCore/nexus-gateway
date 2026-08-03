// Rule packs family (S-026 — install + effective rules merge from
// catalog §5.5 gap). The rule pack lifecycle is the most complex
// admin authoring flow: an admin imports a pack, binds an install to
// a hook, optionally overrides individual rules, then expects the
// effective-rules endpoint to surface the post-override merged set
// the data plane evaluator uses. PM-grade because the merge logic
// directly governs which user prompts get blocked vs allowed.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS026_RulePackInstallEffectiveMerge — PM-grade e2e.
//
// BRAINSTORM (pre): the effective-rules endpoint is the join layer
// between pack rules and per-install overrides. Two PM-grade
// invariants:
//
//  1. Override DISABLE: setting Override{Disabled: true} on a rule
//     MUST cause that rule to disappear from the effective-rules
//     response (or its `disabled` flag must surface). The data
//     plane skips disabled rules — if the merge layer silently
//     ignores the Disabled flag, an admin can't actually disable
//     a rule that's catching false positives.
//  2. Override SEVERITY: setting Override{SeverityOverride: "warn"}
//     MUST surface the override severity in effective-rules. The
//     data plane reads severity to decide block vs warn — silently
//     ignoring overrides breaks the operator's ability to demote a
//     rule from block to warn.
//
// The severity vocabulary is hard|soft|warn (rulepack.validateRules), so
// the demotion under test is hard→warn: literally "block to warn". This
// scenario used to send high/low, which are not severities at all — the
// pack create 400'd before any of the merge logic was reached.
//
// Cross-service: CP-only (rulepack is CP-side). Hook binding doesn't
// require a live hook engine for the merge test — the merge is a
// pure DB join.
//
// Assertions:
//  1. POST /rule-packs creates a synthetic 2-rule pack.
//  2. POST /hooks/:hookId/rule-packs creates an install bound to a
//     live builtin hook id (pii-outbound-scanner).
//  3. GET /rule-pack-installs/:installId/effective-rules returns
//     both rules with severity matching the pack defaults.
//  4. PATCH overrides: disable rule 1, demote rule 2's severity.
//  5. GET effective-rules again: rule 1 absent or disabled flag set;
//     rule 2's severity is the override value.
//  6. Cleanup: uninstall + delete pack.
func TestS026_RulePackInstallEffectiveMerge(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	nonce := time.Now().UnixNano()
	// A pack name must be "<namespace>/<short-name>" — rulepack.packNameRE
	// (`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`) rejects a bare name outright, so
	// the namespace segment is required, not cosmetic. Without the slash the
	// create returns 400 validation_failed and every arm below is unreachable.
	packName := fmt.Sprintf("s026/pack-%d", nonce)
	// v-prefixed semver — rulepack.semverRE is
	// `^v(\d+)\.(\d+)\.(\d+)(?:[-+][A-Za-z0-9._-]+)?$`, so a bare "1.0.0" is
	// a 400. Reused verbatim as the install's pinVersion, which must equal
	// the pack version for the install to resolve.
	packVersion := "v1.0.0"
	packBody, _ := json.Marshal(map[string]any{
		"name":       packName,
		"version":    packVersion,
		"maintainer": "scenario-test",
		"rules": []map[string]any{
			{"ruleId": "r1-disable-me", "category": "pii", "severity": "hard",
				"pattern": fmt.Sprintf("secret-disable-me-x%d", nonce)},
			{"ruleId": "r2-demote-me", "category": "injection", "severity": "hard",
				"pattern": fmt.Sprintf("secret-demote-me-x%d", nonce)},
		},
	})
	st, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/rule-packs", packBody)
	if err != nil {
		t.Fatalf("create pack: %v", err)
	}
	if st != http.StatusCreated && st != http.StatusOK {
		t.Fatalf("create pack: status=%d body=%q", st, truncate(body, 300))
	}
	var pack struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &pack)
	if pack.ID == "" {
		t.Fatalf("create pack missing id: body=%q", truncate(body, 300))
	}
	packID := pack.ID
	sc.Cleanup.Register("delete pack "+packID, func() error {
		_, _, _ = helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/rule-packs/"+packID, nil)
		return nil
	})

	// Pick a live builtin hook to bind to.
	var hookID string
	if err := sc.DB.QueryRow(ctx,
		`SELECT id FROM "HookConfig" WHERE name = 'pii-outbound-scanner' LIMIT 1`,
	).Scan(&hookID); err != nil || hookID == "" {
		t.Skipf("no pii-outbound-scanner hook in DB — skipping (err=%v)", err)
	}

	installReq, _ := json.Marshal(map[string]any{
		"packId":     packID,
		"pinVersion": packVersion,
		"enabled":    true,
	})
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/admin/hooks/"+hookID+"/rule-packs", installReq)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if st != http.StatusCreated && st != http.StatusOK {
		t.Fatalf("install: status=%d body=%q", st, truncate(body, 300))
	}
	var install struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &install)
	if install.ID == "" {
		t.Fatalf("install missing id: body=%q", truncate(body, 300))
	}
	installID := install.ID
	sc.Cleanup.Register("uninstall "+installID, func() error {
		_, _, _ = helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/admin/rule-pack-installs/"+installID, nil)
		return nil
	})

	rulesBefore := fetchEffectiveRules(t, ctx, sc.Env, token, installID)
	if len(rulesBefore) != 2 {
		t.Errorf("effective-rules before overrides: %d rules, want 2",
			len(rulesBefore))
	}
	for _, r := range rulesBefore {
		if sev, _ := r["severity"].(string); sev != "hard" {
			t.Errorf("rule %v severity=%v, want 'hard' (pack default)",
				r["ruleId"], sev)
		}
	}

	overridesBody, _ := json.Marshal(map[string]any{
		"overrides": []map[string]any{
			{"ruleLocalId": "r1-disable-me", "disabled": true},
			{"ruleLocalId": "r2-demote-me", "severityOverride": "warn"},
		},
	})
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPatch,
		"/api/admin/rule-pack-installs/"+installID+"/overrides",
		overridesBody)
	if err != nil {
		t.Fatalf("upsert overrides: %v", err)
	}
	if st < 200 || st >= 300 {
		t.Fatalf("upsert overrides: status=%d body=%q",
			st, truncate(body, 300))
	}

	rulesAfter := fetchEffectiveRules(t, ctx, sc.Env, token, installID)
	var r1Found bool
	var r1Disabled bool
	var r2Severity string
	for _, r := range rulesAfter {
		rid, _ := r["ruleId"].(string)
		switch rid {
		case "r1-disable-me":
			r1Found = true
			if dis, ok := r["disabled"].(bool); ok {
				r1Disabled = dis
			}
		case "r2-demote-me":
			r2Severity, _ = r["severity"].(string)
		}
	}
	if r1Found && !r1Disabled {
		t.Errorf("r1 still present with disabled=false (override not applied)")
	}
	if r2Severity != "warn" {
		t.Errorf("r2 severity after override = %q, want 'warn' (override not applied)",
			r2Severity)
	}

	// Invariant 3 (S-13): an override severity outside the closed hard|soft|warn
	// enum must be REFUSED, not stored. severityEnforces() enforces only
	// hard|soft and returns false for everything else, so persisting e.g. "high"
	// silently converts a blocking rule into a non-blocking one: it still
	// matches and still reports, but no longer blocks or redacts. The authoring
	// path has guarded this since it was written (rulepack.validSeverities,
	// whose own comment calls it "a correctness backstop, not cosmetics"); the
	// override path reaches the same runtime field and must guard it too.
	badBody, _ := json.Marshal(map[string]any{
		"overrides": []map[string]any{
			{"ruleLocalId": "r2-demote-me", "severityOverride": "high"},
		},
	})
	st, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPatch,
		"/api/admin/rule-pack-installs/"+installID+"/overrides",
		badBody)
	if err != nil {
		t.Fatalf("bogus-severity override: %v", err)
	}
	if st != http.StatusBadRequest {
		t.Errorf("bogus severityOverride \"high\": status=%d want 400 (body=%q)",
			st, truncate(body, 300))
	}
	// And it must not have taken effect: r2 keeps the last VALID override.
	for _, r := range fetchEffectiveRules(t, ctx, sc.Env, token, installID) {
		if rid, _ := r["ruleId"].(string); rid == "r2-demote-me" {
			if sev, _ := r["severity"].(string); sev != "warn" {
				t.Errorf("r2 severity after REJECTED override = %q, want 'warn' (the rejected write must not persist)", sev)
			}
		}
	}

	t.Logf("S-026 OK: pack=%s install=%s effective: %d→%d; r1 present=%v disabled=%v; r2 severity=%s",
		packID, installID, len(rulesBefore), len(rulesAfter),
		r1Found, r1Disabled, r2Severity)
}

// fetchEffectiveRules pulls the effective-rules envelope and returns
// the merged rule array. The endpoint shape is
// `{install: {...}, pack: {rules: [...]}}` per the EffectiveRuleSet
// struct.
func fetchEffectiveRules(t *testing.T, ctx context.Context, env *intg.Env, token, installID string) []map[string]any {
	t.Helper()
	st, body, err := helpers.CPDoJSON(ctx, env, token,
		http.MethodGet,
		"/api/admin/rule-pack-installs/"+installID+"/effective-rules", nil)
	if err != nil {
		t.Fatalf("effective-rules: %v", err)
	}
	if st != http.StatusOK {
		t.Fatalf("effective-rules: status=%d body=%q", st, truncate(body, 300))
	}
	var env2 struct {
		Pack struct {
			Rules []map[string]any `json:"rules"`
		} `json:"pack"`
	}
	if err := json.Unmarshal(body, &env2); err != nil {
		t.Fatalf("decode effective-rules: %v body=%q", err, truncate(body, 400))
	}
	return env2.Pack.Rules
}
