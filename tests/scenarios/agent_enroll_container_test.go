// Daemon-bound — containerized Linux agent enrollment.
//
// A real Linux agent binary, running in a container, must enroll against the
// real Hub and register as an online agent Thing. This is the runtime arm of P3
// (daemon in CI): the admin-side enrollment is covered by the in-process Phase
// 2b tests; this drives the actual agent daemon end-to-end.
package scenarios_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

var deviceIDRe = regexp.MustCompile(`agent-[0-9a-f]{16}`)

// TestS084_ContainerizedAgentEnrollment — daemon-bound enrollment scenario.
//
// Preconditions (SKIP, never red — a missing Docker/image is an env gap):
//   - docker is on PATH,
//   - the agent image exists (NEXUS_AGENT_IMAGE, default nexus-agent:citest;
//     built via `docker build -f packages/agent/Dockerfile .`).
//
// Flow: mint an enrollment token (admin) -> `docker run … enroll --hub-url
// http://host.docker.internal:3060 --token <t>` -> assert a `thing` row of
// type='agent', status='online' appears -> unenroll cleanup.
func TestS084_ContainerizedAgentEnrollment(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH — containerized agent enrollment is a CI/daemon-bound scenario")
	}
	image := getenvDefault("NEXUS_AGENT_IMAGE", "nexus-agent:citest")
	if out, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Skipf("agent image %q not built (%v) — run `docker build -f packages/agent/Dockerfile -t %s .` first; "+
			"this scenario runs in the daemon CI lane", image, strings.TrimSpace(string(out)), image)
	}

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}
	// Mint a single-use enrollment token via the admin API.
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token, "POST",
		"/api/admin/agent-devices/enroll-token", []byte(`{"hostname":"s084-ci-agent"}`))
	if err != nil || status != 201 {
		t.Fatalf("mint enroll-token: status=%d err=%v body=%q", status, err, truncate(body, 200))
	}
	var tok struct {
		Token string `json:"token"`
	}
	if jsonErr := json.Unmarshal(body, &tok); jsonErr != nil || tok.Token == "" {
		t.Fatalf("parse enroll-token: %v body=%q", jsonErr, truncate(body, 200))
	}

	// Run the agent container's enroll against the Hub. host.docker.internal
	// resolves to the host (--add-host makes it explicit on Linux CI too).
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "docker", "run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		image, "enroll",
		"--hub-url", "http://host.docker.internal:3060",
		"--token", tok.Token,
		"--config", "/etc/nexus-agent/agent.yaml",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("agent container enroll failed: %v\n%s", err, truncate(out, 600))
	}
	deviceID := deviceIDRe.FindString(string(out))
	if deviceID == "" {
		t.Fatalf("could not parse enrolled device id from agent output:\n%s", truncate(out, 600))
	}
	// Cleanup: unenroll the device (best-effort; this scenario owns it).
	sc.Cleanup.Register("unenroll("+deviceID+")", func() error {
		_, _, _ = helpers.CPDoJSON(context.Background(), sc.Env, token, "POST",
			"/api/admin/agent-devices/"+deviceID+"/unenroll", []byte(`{}`))
		return nil
	})

	// Assert the Hub registered it as an online agent Thing.
	const query = `SELECT type, status FROM thing WHERE id = $1`
	const tries = 10
	const interval = 2 * time.Second
	var typ, st string
	found := false
	for i := 0; i < tries; i++ {
		if scanErr := sc.DB.QueryRow(ctx, query, deviceID).Scan(&typ, &st); scanErr == nil {
			found = true
			break
		}
		time.Sleep(interval)
	}
	if !found {
		t.Fatalf("no thing row for enrolled device %s — the Hub did not register the containerized agent", deviceID)
	}
	if typ != "agent" {
		t.Errorf("thing %s type=%q (want 'agent')", deviceID, typ)
	}
	if st != "online" {
		t.Errorf("thing %s status=%q (want 'online') right after enroll", deviceID, st)
	}
	t.Logf("S-084 OK: containerized agent enrolled against live Hub — device=%s type=%s status=%s", deviceID, typ, st)
}
