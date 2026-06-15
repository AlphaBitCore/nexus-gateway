// Package rulepack: yaml.go — YAML loader + validator for pack authoring.
package rulepack

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// semverRE matches v-prefixed semver e.g. v1.0.0, v1.2.3-rc1.
var semverRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:[-+][A-Za-z0-9._-]+)?$`)

// packNameRE requires "<namespace>/<short-name>"; namespace lowercase alphanumeric.
var packNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`)

// LoadYAML parses and validates a pack YAML document. Returns the Pack,
// a list of non-fatal warnings, or a fatal error. Validation covers:
//   - top-level shape (name, version, maintainer, rules required)
//   - pack name format "<namespace>/<short-name>"
//   - semver version (with "v" prefix)
//   - rule.id uniqueness within pack
//   - rule pattern compiles
//   - rule severity in {hard, soft, warn}
//   - rule category non-empty
func LoadYAML(data []byte) (*Pack, []string, error) {
	var pack Pack
	if err := yaml.Unmarshal(data, &pack); err != nil {
		return nil, nil, fmt.Errorf("rulepack: yaml parse: %w", err)
	}
	warnings, err := ValidatePack(&pack)
	if err != nil {
		return nil, nil, err
	}
	return &pack, warnings, nil
}

// ValidatePack enforces the full pack contract on an already-parsed Pack,
// regardless of the authoring surface (YAML upload OR the JSON admin form).
// It returns non-fatal warnings and a fatal error on the first invariant
// breach. Both LoadYAML and the JSON Create handler MUST route through this so
// a typo'd regex / bad severity / missing category is rejected with a 400 at
// authoring time instead of being silently skipped by the evaluator at
// runtime (a "PII block" rule that never fires). Validation covers:
//   - pack name format "<namespace>/<short-name>"
//   - semver version (with "v" prefix), maintainer present
//   - rule.id uniqueness within pack
//   - rule pattern compiles (RE2)
//   - rule severity in {hard, soft, warn}
//   - rule category non-empty
func ValidatePack(pack *Pack) ([]string, error) {
	var warnings []string

	if pack.Name == "" {
		return nil, fmt.Errorf("rulepack: name is required")
	}
	if !packNameRE.MatchString(pack.Name) {
		return nil, fmt.Errorf("rulepack: name %q must match <namespace>/<short-name>", pack.Name)
	}
	if pack.Version == "" {
		return nil, fmt.Errorf("rulepack: version is required")
	}
	if !semverRE.MatchString(pack.Version) {
		return nil, fmt.Errorf("rulepack: version %q must be semver (e.g. v1.0.0)", pack.Version)
	}
	if pack.Maintainer == "" {
		return nil, fmt.Errorf("rulepack: maintainer is required")
	}

	if len(pack.Rules) == 0 {
		warnings = append(warnings, "pack has no rules; nothing will ever match")
	}

	seen := map[string]struct{}{}
	for i, r := range pack.Rules {
		if r.RuleID == "" {
			return nil, fmt.Errorf("rulepack: rules[%d] missing id", i)
		}
		if _, dup := seen[r.RuleID]; dup {
			return nil, fmt.Errorf("rulepack: duplicate rule id %q", r.RuleID)
		}
		seen[r.RuleID] = struct{}{}
		if strings.TrimSpace(r.Category) == "" {
			return nil, fmt.Errorf("rulepack: rules[%d] (%q) missing category", i, r.RuleID)
		}
		switch r.Severity {
		case "hard", "soft", "warn":
			// ok
		default:
			return nil, fmt.Errorf("rulepack: rules[%d] (%q) invalid severity %q (want hard|soft|warn)", i, r.RuleID, r.Severity)
		}
		if r.Pattern == "" {
			return nil, fmt.Errorf("rulepack: rules[%d] (%q) missing pattern", i, r.RuleID)
		}
		// Compile via Go's regexp (not the cache) to catch parse errors.
		// The cache-backed compile happens again at evaluator build time.
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return nil, fmt.Errorf("rulepack: rules[%d] (%q) invalid regex: %w", i, r.RuleID, err)
		}
	}

	return warnings, nil
}
