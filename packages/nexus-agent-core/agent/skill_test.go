package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillSetCatalogAndGet(t *testing.T) {
	s := NewSkillSet(
		Skill{Name: "incident-triage", Description: "Walk a firing alert to root cause", Body: "1. open alerts\n2. ...", AllowedTools: []string{"observe_alerts", "observe_nodes"}},
		Skill{Name: "cost-investigation", Description: "Find the cost driver", Body: "1. open cost\n2. ..."},
	)
	cat := s.Catalog()
	if !strings.Contains(cat, "incident-triage") || !strings.Contains(cat, "Walk a firing alert") {
		t.Fatalf("catalog must list name + description, got:\n%s", cat)
	}
	if strings.Contains(cat, "open alerts") {
		t.Fatal("catalog must NOT contain skill bodies (progressive disclosure)")
	}
	if names := s.Names(); len(names) != 2 || names[0] != "cost-investigation" {
		t.Fatalf("Names should be sorted, got %v", names)
	}
	sk, ok := s.Get("incident-triage")
	if !ok || sk.Body == "" {
		t.Fatal("Get returns the full skill incl body")
	}

	// Empty set renders a clear placeholder.
	if NewSkillSet().Catalog() != "(no skills available)" {
		t.Fatal("empty catalog must render a placeholder")
	}
}

func TestUseSkillToolInjectsBodyAndReportsAllowed(t *testing.T) {
	s := NewSkillSet(Skill{Name: "cost-investigation", Description: "d", Body: "PLAYBOOK BODY", AllowedTools: []string{"observe_cost"}})
	var activated string
	var narrowed []string
	tool := newUseSkillTool(s, func(name string, allow []string) { activated = name; narrowed = allow })

	// Accessors.
	if tool.Name() != "use_skill" || tool.Description() == "" || tool.Tier() != TierAuto {
		t.Fatalf("use_skill accessors wrong: name=%q tier=%v", tool.Name(), tool.Tier())
	}
	if !json.Valid(tool.Schema()) {
		t.Fatal("use_skill schema must be valid JSON")
	}

	res, err := tool.Run(context.Background(), json.RawMessage(`{"name":"cost-investigation"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "PLAYBOOK BODY") {
		t.Fatalf("use_skill result must inject the body, got %q", res.Content)
	}
	if activated != "cost-investigation" {
		t.Fatalf("use_skill must report activation, got %q", activated)
	}
	if len(narrowed) != 1 || narrowed[0] != "observe_cost" {
		t.Fatalf("use_skill must report the allow-list, got %v", narrowed)
	}

	// Unknown skill → recoverable error result the model can adapt to.
	bad, err := tool.Run(context.Background(), json.RawMessage(`{"name":"ghost"}`))
	if err != nil {
		t.Fatal("unknown skill returns an error Result, not a Go error")
	}
	if !bad.IsError || !strings.Contains(bad.Content, "ghost") {
		t.Fatalf("unknown skill should be a recoverable error result, got %+v", bad)
	}

	// Invalid JSON → recoverable error result.
	inv, err := tool.Run(context.Background(), json.RawMessage(`{not json`))
	if err != nil || !inv.IsError {
		t.Fatalf("invalid input should be a recoverable error result, got %+v err %v", inv, err)
	}
}
