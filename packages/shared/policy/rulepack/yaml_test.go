// packages/shared/policy/rulepack/yaml_test.go
package rulepack

import (
	"strings"
	"testing"
)

func TestLoadYAML_HappyPath(t *testing.T) {
	src := `
name: nexus/prompt-injection
version: v1.0.0
maintainer: nexus
description: demo
rules:
  - id: pi-001
    category: prompt_injection
    severity: hard
    pattern: '(?i)ignore\s+(all\s+)?previous'
    labels: [detector:prompt-injection]
    description: instruction override
`
	p, warnings, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if p.Name != "nexus/prompt-injection" || p.Version != "v1.0.0" {
		t.Fatalf("bad parse: %+v", p)
	}
	if len(p.Rules) != 1 || p.Rules[0].RuleID != "pi-001" {
		t.Fatalf("bad rules: %+v", p.Rules)
	}
}

func TestLoadYAML_InvalidSemver(t *testing.T) {
	src := `
name: acme/x
version: 1.0  # missing v prefix + not semver
maintainer: customer
rules: [{id: r1, category: x, severity: hard, pattern: foo}]
`
	_, _, err := LoadYAML([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestLoadYAML_DuplicateRuleID(t *testing.T) {
	src := `
name: acme/x
version: v1.0.0
maintainer: customer
rules:
  - {id: dup, category: x, severity: hard, pattern: foo}
  - {id: dup, category: y, severity: soft, pattern: bar}
`
	_, _, err := LoadYAML([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestLoadYAML_BadRegex(t *testing.T) {
	src := `
name: acme/x
version: v1.0.0
maintainer: customer
rules:
  - {id: r1, category: x, severity: hard, pattern: '[unclosed'}
`
	_, _, err := LoadYAML([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "regex") {
		t.Fatalf("expected regex error, got %v", err)
	}
}

func TestLoadYAML_InvalidSeverity(t *testing.T) {
	src := `
name: acme/x
version: v1.0.0
maintainer: customer
rules:
  - {id: r1, category: x, severity: blocker, pattern: foo}
`
	_, _, err := LoadYAML([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "severity") {
		t.Fatalf("expected severity error, got %v", err)
	}
}

func TestLoadYAML_InvalidName_NoNamespace(t *testing.T) {
	src := `
name: promptinjection
version: v1.0.0
maintainer: nexus
rules: [{id: r, category: x, severity: hard, pattern: foo}]
`
	_, _, err := LoadYAML([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestLoadYAML_EmptyRules(t *testing.T) {
	src := `
name: acme/x
version: v1.0.0
maintainer: customer
rules: []
`
	_, warnings, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatalf("empty rules should parse OK, got %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "no rules") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'no rules' warning, got %v", warnings)
	}
}
