package helpers

import (
	"strings"
	"testing"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
)

// localEnv returns an Env that should pass CheckLocalTarget. Tests mutate
// individual fields to exercise each fail-closed path independently.
func localEnv() *intg.Env {
	return &intg.Env{
		HubURL:   "http://localhost:3060",
		CPURL:    "http://localhost:3001",
		AIGwURL:  "http://localhost:3050",
		ProxyURL: "http://localhost:3040",
		UIURL:    "http://localhost:3000",
		PGHost:   "localhost",
		PGPort:   "55532",
		PGUser:   "postgres",
		PGDB:     "nexus_gateway",
	}
}

func TestCheckLocalTarget_AllLocalIsAccepted(t *testing.T) {
	v := CheckLocalTarget(localEnv())
	if len(v) != 0 {
		t.Fatalf("clean local env should produce zero violations, got: %v", v)
	}
}

func TestCheckLocalTarget_HTTPSAgainstLocalhostIsAllowed(t *testing.T) {
	env := localEnv()
	env.UIURL = "https://localhost:3000"
	if v := CheckLocalTarget(env); len(v) != 0 {
		t.Errorf("https://localhost should be allowed, got violations: %v", v)
	}
	env.UIURL = "https://127.0.0.1:3000"
	if v := CheckLocalTarget(env); len(v) != 0 {
		t.Errorf("https://127.0.0.1 should be allowed, got violations: %v", v)
	}
}

func TestCheckLocalTarget_DockerInternalIsAllowed(t *testing.T) {
	env := localEnv()
	env.HubURL = "http://host.docker.internal:3060"
	if v := CheckLocalTarget(env); len(v) != 0 {
		t.Errorf("host.docker.internal should be allowed (for cross-container CI), got: %v", v)
	}
}

// Per §2 rule 2 — any URL whose host is not in the localhost allowlist
// must produce a violation. Covers each of the 5 NEXUS_*_URL fields.
func TestCheckLocalTarget_NonLocalHostRejected(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*intg.Env)
		want string
	}{
		{"Hub points at staging", func(e *intg.Env) { e.HubURL = "http://staging.example.internal:3060" }, "NEXUS_HUB_URL"},
		{"CP points at staging", func(e *intg.Env) { e.CPURL = "http://staging.example.internal:3001" }, "NEXUS_CP_URL"},
		{"AI Gw points at staging", func(e *intg.Env) { e.AIGwURL = "http://staging.example.internal:3050" }, "NEXUS_AI_GW_URL"},
		{"Proxy points at staging", func(e *intg.Env) { e.ProxyURL = "http://staging.example.internal:3040" }, "NEXUS_PROXY_URL"},
		{"UI points at staging", func(e *intg.Env) { e.UIURL = "http://staging.example.internal:3000" }, "NEXUS_UI_URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := localEnv()
			c.mut(env)
			vs := CheckLocalTarget(env)
			if len(vs) == 0 {
				t.Fatalf("%s: expected violation, got none", c.name)
			}
			joined := strings.Join(vs, " | ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("%s: violation should mention %s, got: %v", c.name, c.want, vs)
			}
		})
	}
}

// Per §2 rule 2 — defence-in-depth: even if /etc/hosts maps
// taskforce10x.com to 127.0.0.1, the harness must still refuse the URL
// based on substring match.
func TestCheckLocalTarget_ForbiddenSubstringRejected(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*intg.Env)
	}{
		{"prod hostname in Hub", func(e *intg.Env) { e.HubURL = "http://nexus.taskforce10x.com:3060" }},
		{"prod hostname in CP", func(e *intg.Env) { e.CPURL = "https://nexus.taskforce10x.com" }},
		{"prod hostname in AI Gw", func(e *intg.Env) { e.AIGwURL = "https://nexus.taskforce10x.com/v1" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := localEnv()
			c.mut(env)
			vs := CheckLocalTarget(env)
			if len(vs) == 0 {
				t.Fatalf("%s: expected at least one violation, got none", c.name)
			}
			joined := strings.Join(vs, " | ")
			if !strings.Contains(joined, "taskforce10x.com") {
				t.Errorf("%s: violation should mention taskforce10x.com substring, got: %v", c.name, vs)
			}
		})
	}
}

// Per §2 rule 3 — HTTPS pointing at non-localhost is rejected.
func TestCheckLocalTarget_HTTPSAgainstRemoteRejected(t *testing.T) {
	env := localEnv()
	env.CPURL = "https://nexus.example.internal"
	vs := CheckLocalTarget(env)
	if len(vs) == 0 {
		t.Fatalf("https against remote should produce violations")
	}
	joined := strings.Join(vs, " | ")
	if !strings.Contains(joined, "https only allowed against localhost") {
		t.Errorf("expected violation to call out https→remote, got: %v", vs)
	}
}

// Per §2 rule 4 — Postgres DSN must be localhost; refuse prod-port
// (5432) on a non-localhost host outright.
func TestCheckLocalTarget_PGHostMustBeLocal(t *testing.T) {
	env := localEnv()
	env.PGHost = "10.0.5.42"
	vs := CheckLocalTarget(env)
	if len(vs) == 0 {
		t.Fatalf("non-local PG host should produce violation")
	}
	joined := strings.Join(vs, " | ")
	if !strings.Contains(joined, "NEXUS_PG_HOST") {
		t.Errorf("expected violation to call out NEXUS_PG_HOST, got: %v", vs)
	}
}

func TestCheckLocalTarget_ProdPGPortRejected(t *testing.T) {
	env := localEnv()
	env.PGHost = "db.internal"
	env.PGPort = "5432"
	vs := CheckLocalTarget(env)
	if len(vs) == 0 {
		t.Fatalf("prod-port DSN should produce violation")
	}
	joined := strings.Join(vs, " | ")
	if !strings.Contains(joined, "NEXUS_PG_PORT=5432") {
		t.Errorf("expected violation to call out prod PG port, got: %v", vs)
	}
}

// Per §2 rule 2 — an unparseable URL is still a violation (better safe
// than silently treating it as empty).
func TestCheckLocalTarget_UnparseableURLRejected(t *testing.T) {
	env := localEnv()
	env.AIGwURL = "://broken"
	vs := CheckLocalTarget(env)
	if len(vs) == 0 {
		t.Fatalf("unparseable URL should produce violation")
	}
	joined := strings.Join(vs, " | ")
	if !strings.Contains(joined, "unparseable URL") {
		t.Errorf("expected violation to mark URL as unparseable, got: %v", vs)
	}
}
