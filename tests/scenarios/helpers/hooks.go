package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// HookAction is what a scenario should do about a hook it needs switched on.
//
// Deciding is split from acting for the same reason SeedDecision is: t.Skipf and
// t.Fatalf both call runtime.Goexit, so a decision made inside them cannot be
// tested.
type HookAction int

const (
	// HookReady — already enabled. Nothing to change, nothing to restore.
	HookReady HookAction = iota
	// HookEnableIt — enable it, and restore the original value on cleanup.
	HookEnableIt
	// HookMissing — the seed ships this hook and it is not there. A regression.
	HookMissing
)

// HookEnableDecision decides what to do about a hook a scenario needs, given
// what the admin list said.
//
// An absent hook is a regression, not a missing precondition: the seed ships
// twelve HookConfig rows and every one of them is enabled=false, which is why
// these scenarios have to switch one on rather than find one on.
func HookEnableDecision(found, enabled bool, name string) (HookAction, string) {
	if !found {
		return HookMissing, fmt.Sprintf(
			"hook %q is not in the admin list. The seed ships it "+
				"(tools/db-migrate/seed/fixtures/HookConfig.json), so its absence is a "+
				"regression rather than a missing precondition.", name)
	}
	if enabled {
		return HookReady, ""
	}
	return HookEnableIt, ""
}

// hookListEntry is the slice of the admin hook object this file reads.
type hookListEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// FindHookByName locates one hook in a GET /api/admin/hooks body.
//
// It returns an error rather than "not found" when the page it was handed does
// not hold every hook the server counted. GET /api/admin/hooks paginates
// (default limit 50), so a short page and an absent hook look identical in the
// response body — and reading the short page as absence is how a scenario
// reports "the seed is broken" about a hook sitting on page two.
func FindHookByName(body []byte, name string) (entry hookListEntry, found bool, err error) {
	var page struct {
		Data  []hookListEntry `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return entry, false, fmt.Errorf("decode hook list: %w", err)
	}
	if len(page.Data) < page.Total {
		// No advice to raise the limit: the caller already asks for 1000 and the
		// handler caps it there, so if this ever fires the page size is not the
		// thing to change.
		return entry, false, fmt.Errorf(
			"hook list returned %d of %d hooks — this page cannot answer whether %q "+
				"exists, and reading a short page as absence would report a seed "+
				"regression about a hook that is simply on another page",
			len(page.Data), page.Total, name)
	}
	for _, h := range page.Data {
		if h.Name == name {
			return h, true, nil
		}
	}
	return entry, false, nil
}

// EnsureHookEnabled makes a named hook enabled for the duration of one scenario,
// and registers the restore.
//
// Restore returns the hook to the value it was FOUND at, not to false: a hook
// that was already on in this environment must still be on when the scenario
// leaves, and a scenario that switched one on must not leave it on.
//
// A HookConfig row has no owner and no organization — the hook chain is a
// single global policy surface that every request through the gateway resolves.
// Against prod that means this call changes what live customer traffic is
// scanned for, for as long as the scenario runs. That is deliberate and
// authorized; what makes it safe is that the restore always runs. Cleanup fires
// on t.Fatal, but a SIGINT does not run it, so the enable also prints the exact
// restore call: an interrupted run leaves prod changed, and the operator needs
// the undo in the output rather than having to reconstruct it. (The shape is the
// one that bit the smoke run which disabled every routing rule up front and
// restored at the end — interrupted, it left prod half-configured with no alarm
// and nineteen green results above it.)
func EnsureHookEnabled(t *testing.T, ctx context.Context, env *intg.Env, token string, cleanup *Cleanup, name string) {
	t.Helper()

	status, body, err := CPDoJSON(ctx, env, token, "GET", "/api/admin/hooks?limit=1000", nil)
	if err != nil || status != 200 {
		t.Fatalf("GET /api/admin/hooks: status %d err=%v", status, err)
	}
	hook, found, err := FindHookByName(body, name)
	if err != nil {
		t.Fatalf("%v", err)
	}

	action, msg := HookEnableDecision(found, hook.Enabled, name)
	switch action {
	case HookReady:
		return
	case HookMissing:
		t.Fatal(msg)
	}

	// Registered BEFORE the PUT, and the undo printed before it too. The PUT can
	// be delivered and applied and still hand us an error: DoJSON reports a
	// response-BODY read failure with the status already in hand, so a timeout,
	// a reset, or an idle load balancer after the server committed all look like
	// failure here while the row is already switched on. A Fatalf between the
	// write and the registration is exactly how a global compliance hook gets
	// left enabled with nothing queued to switch it back. Restoring a hook that
	// was never enabled is a no-op, so registering early costs nothing.
	undo := fmt.Sprintf("PUT %s/api/admin/hooks/%s  {\"enabled\":false}", env.CPURL, hook.ID)
	t.Logf("about to enable hook %q (%s) for this scenario. If this run is interrupted "+
		"before cleanup, undo it with:\n  %s", name, hook.ID, undo)
	cleanup.Register(fmt.Sprintf("restore hook %q enabled=false", name), func() error {
		err := setHookEnabled(ctx, env, token, hook.ID, false)
		if err != nil {
			// A restore that fails leaves a GLOBAL compliance control switched
			// on, so it must not be a green run with a swallowed line. Cleanup's
			// own t.Logf is not enough: the suite runs `go test` without -v,
			// which discards logs from PASSING tests — precisely the run where
			// this is the only warning there is. Errorf turns the run red;
			// stderr survives whatever the harness does with test logs.
			fmt.Fprintf(os.Stderr,
				"\nPROD RESIDUE: hook %q (%s) is still ENABLED — restore failed: %v\n  undo with: %s\n",
				name, hook.ID, err, undo)
			t.Errorf("restore of hook %q failed, leaving it enabled: %v", name, err)
		}
		return err
	})

	if err := setHookEnabled(ctx, env, token, hook.ID, true); err != nil {
		t.Fatalf("enable hook %q: %v", name, err)
	}
}

// setHookEnabled flips one hook's enabled flag. Every other field is omitted, and
// UpdateHookConfig binds them as pointers, so an omitted field keeps its stored
// value — changing one flag does not have to round-trip the hook's whole config.
func setHookEnabled(ctx context.Context, env *intg.Env, token, id string, enabled bool) error {
	payload, err := json.Marshal(map[string]any{"enabled": enabled})
	if err != nil {
		return err
	}
	status, body, err := CPDoJSON(ctx, env, token, "PUT", "/api/admin/hooks/"+id, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("PUT /api/admin/hooks/%s: status %d body %s", id, status, truncateBody(body))
	}
	return nil
}

func truncateBody(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}
