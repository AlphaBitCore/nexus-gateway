package assistant

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/skills"
)

// stubFileStore satisfies the fileStore seam so BuildWebAgent registers the
// write_file/read_file sandbox tools without a real spillstore/DB. Used by the
// skill/registry guard below to build the MAXIMAL web tool set.
type stubFileStore struct{}

func (stubFileStore) Write(_, _, _ string) (fileMeta, error) { return fileMeta{}, nil }
func (stubFileStore) ReadByName(_ string) (string, error)    { return "", nil }

// TestBuiltinSkillsToolsResolveOnWebProfile is the e90-s7 AC-2 guard: every
// built-in skill's `allowed-tools` must name a tool the web agent actually
// exposes. A skill that lists a tool absent from the web profile would, on
// activation, narrow the model to a tool it can never call (a silent dead end);
// a typo or a CLI-only tool name (e.g. run_command) must fail here, not in prod.
func TestBuiltinSkillsToolsResolveOnWebProfile(t *testing.T) {
	d := WebAgentDeps{
		CallerAuthorization: "Bearer t",
		CPBaseURL:           "http://cp.local",
		AIGatewayURL:        "http://gw.local",
		SystemVK:            "nvk_test",
		Model:               "m",
		Session:             agent.NewSession("web"),
		Files:               stubFileStore{}, // include the sandbox tools in the set
	}
	ag, err := BuildWebAgent(d)
	if err != nil {
		t.Fatalf("BuildWebAgent: %v", err)
	}
	have := toolNameSet(ag.ToolNames())

	set, err := skills.Load("")
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	names := set.Names()
	if len(names) == 0 {
		t.Fatal("expected built-in skills to load; got none")
	}
	for _, name := range names {
		sk, ok := set.Get(name)
		if !ok {
			t.Fatalf("skill %q listed by Names() but not retrievable via Get()", name)
		}
		for _, tool := range sk.AllowedTools {
			if !have[tool] {
				t.Errorf("skill %q references tool %q which the web agent does not expose "+
					"(allowed-tools drift: add the tool to the web profile or fix the skill)", name, tool)
			}
		}
	}
}

// TestBuildWebAgent_DisableBodyReads is the §8 operator-control assertion: when
// DisableBodyReads is set, the raw-body read tools are absent from the agent's
// tool set entirely (neither advertised nor callable), while the aggregate /
// analysis tools remain available.
func TestBuildWebAgent_DisableBodyReads(t *testing.T) {
	base := func() WebAgentDeps {
		return WebAgentDeps{
			CallerAuthorization: "Bearer t",
			CPBaseURL:           "http://cp.local",
			AIGatewayURL:        "http://gw.local",
			SystemVK:            "nvk_test",
			Model:               "m",
			Session:             agent.NewSession("web"),
		}
	}

	on, err := BuildWebAgent(base())
	if err != nil {
		t.Fatalf("BuildWebAgent: %v", err)
	}
	onNames := toolNameSet(on.ToolNames())
	for _, n := range bodyReadToolNames {
		if !onNames[n] {
			t.Errorf("expected body-read tool %q present by default", n)
		}
	}

	d := base()
	d.DisableBodyReads = true
	off, err := BuildWebAgent(d)
	if err != nil {
		t.Fatalf("BuildWebAgent (disabled): %v", err)
	}
	offNames := toolNameSet(off.ToolNames())
	for _, n := range bodyReadToolNames {
		if offNames[n] {
			t.Errorf("body-read tool %q must be removed when DisableBodyReads is set", n)
		}
	}
	// The governance switch must NOT strip the assistant's aggregate tools.
	if !offNames["observe_health"] {
		t.Error("aggregate tool observe_health must remain when only body reads are disabled")
	}
}

func toolNameSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
