package helpers

import (
	"context"
	"strings"
	"testing"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// A login failure must be latched, not retried.
//
// This is the orchestration defect a full L5 run exposed: 57 scenarios each
// call CPLogin, the success cache only fills on success, so ONE failure sends
// every later scenario down the slow path to issue its own request. The CP
// allows ten attempts per (ip, email) inside a five-minute SLIDING window, so
// attempt eleven onward is 429 — and because the window slides, the retries
// keep feeding it and the lockout outlives the run. One root cause produced 62
// red tests and a suite that could not be re-run for five minutes.
//
// Asserted by the message rather than by counting requests: the second call
// can only produce "not retrying" by having returned without issuing one.
func TestCPLogin_LatchesTheFirstFailureInsteadOfRetrying(t *testing.T) {
	ResetTokenCache()
	t.Cleanup(ResetTokenCache)

	// A port nothing listens on: the first call fails at dial, deterministically
	// and without touching any real rate limiter.
	env := &intg.Env{
		CPURL:         "http://127.0.0.1:1",
		AdminEmail:    "admin@nexus.ai",
		AdminPassword: "irrelevant",
		OAuthClientID: "cp-ui",
		OAuthRedirect: "http://localhost:3000/auth/callback",
	}

	if _, err := CPLogin(context.Background(), env); err == nil {
		t.Fatal("expected the first login against a dead port to fail")
	}

	_, err := CPLogin(context.Background(), env)
	if err == nil {
		t.Fatal("expected the second login to fail too")
	}
	if !strings.Contains(err.Error(), "not retrying") {
		t.Errorf("the second call issued another request instead of latching; got %q", err)
	}
}

// ResetTokenCache is what a scenario calls when it deliberately invalidates its
// token, so it must clear the latch too — otherwise one early failure would
// make every later deliberate re-login impossible for the rest of the process.
func TestResetTokenCache_ClearsTheFailureLatch(t *testing.T) {
	ResetTokenCache()
	t.Cleanup(ResetTokenCache)

	env := &intg.Env{CPURL: "http://127.0.0.1:1", AdminEmail: "a@b.c", AdminPassword: "x",
		OAuthClientID: "cp-ui", OAuthRedirect: "http://localhost:3000/auth/callback"}
	_, _ = CPLogin(context.Background(), env)

	ResetTokenCache()

	_, err := CPLogin(context.Background(), env)
	if err != nil && strings.Contains(err.Error(), "not retrying") {
		t.Error("the latch survived ResetTokenCache; a scenario that revokes its token could never log in again")
	}
}
