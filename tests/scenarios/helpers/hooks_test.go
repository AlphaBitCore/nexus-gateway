package helpers

import (
	"strings"
	"testing"
)

// The assertion this replaced was two independent substring scans of the whole
// list body: `contains("keyword-blocker") && contains("enabled":true)`. That is
// green whenever the named hook merely EXISTS and some OTHER hook is on — which
// is the exact state a freshly seeded database plus one unrelated enable
// produces. This pins that FindHookByName reads the named hook's own flag.
func TestFindHookByName_ReadsTheNamedHooksOwnFlag(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"h1","name":"keyword-blocker","enabled":false},
		{"id":"h2","name":"global-rate-limit","enabled":true}
	],"total":2}`)

	if strings.Contains(string(body), `"keyword-blocker"`) &&
		!strings.Contains(string(body), `"enabled":true`) {
		t.Fatal("this fixture is meant to satisfy the OLD two-substring check; " +
			"if it no longer does, the anti-vacuity claim below proves nothing")
	}

	hook, found, err := FindHookByName(body, "keyword-blocker")
	if err != nil || !found {
		t.Fatalf("FindHookByName: found=%v err=%v", found, err)
	}
	if hook.Enabled {
		t.Fatal("read enabled=true for a hook whose own flag is false — the whole " +
			"point of parsing instead of substring-scanning")
	}
	if hook.ID != "h1" {
		t.Fatalf("id = %q, want h1", hook.ID)
	}
}

// A short page and an absent hook produce the same body shape. Reading the short
// page as absence reports "the seed is broken" about a hook on page two.
func TestFindHookByName_RefusesToAnswerFromAPartialPage(t *testing.T) {
	body := []byte(`{"data":[{"id":"h1","name":"global-rate-limit","enabled":true}],"total":12}`)

	_, found, err := FindHookByName(body, "keyword-blocker")
	if err == nil {
		t.Fatalf("returned found=%v with no error — a page holding 1 of 12 hooks "+
			"cannot establish that the other 11 do not include this one", found)
	}
	if !strings.Contains(err.Error(), "1 of 12") {
		t.Fatalf("error %q does not say how much of the list it saw, so the reader "+
			"cannot tell this from a decode failure", err)
	}
}

func TestFindHookByName_AbsentFromACompletePage(t *testing.T) {
	body := []byte(`{"data":[{"id":"h1","name":"global-rate-limit","enabled":true}],"total":1}`)

	_, found, err := FindHookByName(body, "keyword-blocker")
	if err != nil {
		t.Fatalf("unexpected error on a complete page: %v", err)
	}
	if found {
		t.Fatal("found a hook that is not in a page proven complete")
	}
}

func TestHookEnableDecision(t *testing.T) {
	tests := []struct {
		name       string
		found      bool
		enabled    bool
		want       HookAction
		msgMustSay string
	}{
		{"already on — nothing to change or restore", true, true, HookReady, ""},
		{"off — enable and restore", true, false, HookEnableIt, ""},
		// The seed ships all twelve rows, so absence is a regression. Skipping
		// here would take the one end-to-end hook path quiet exactly when it
		// should go red.
		{"absent — regression, not a precondition", false, false, HookMissing, "regression"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := HookEnableDecision(tc.found, tc.enabled, "keyword-blocker")
			if got != tc.want {
				t.Fatalf("action = %v, want %v", got, tc.want)
			}
			if tc.msgMustSay != "" && !strings.Contains(msg, tc.msgMustSay) {
				t.Fatalf("message %q does not say %q, so the reader cannot tell "+
					"what to do about it", msg, tc.msgMustSay)
			}
			if tc.want == HookReady && msg != "" {
				t.Fatalf("carried a message %q for a hook needing no action", msg)
			}
		})
	}
}
