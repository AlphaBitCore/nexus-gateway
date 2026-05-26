package iam

import (
	"fmt"
	"strings"
)

const maxStatements = 50

var validEffects = map[string]bool{"Allow": true, "Deny": true}

var validConditionOperators = map[string]bool{
	"StringEquals":       true,
	"StringNotEquals":    true,
	"StringLike":         true,
	"IpAddress":          true,
	"NotIpAddress":       true,
	"NumericLessThan":    true,
	"NumericGreaterThan": true,
	"NumericEquals":      true,
	"DateLessThan":       true,
	"DateGreaterThan":    true,
}

// ValidatePolicyDocument validates an IAM policy document and returns any errors.
func ValidatePolicyDocument(doc *PolicyDocument) []string {
	if doc == nil {
		return []string{"Policy document must not be nil"}
	}

	var errs []string

	if doc.Version == "" {
		errs = append(errs, "Version is required")
	}

	if len(doc.Statement) == 0 {
		errs = append(errs, "Statement array must contain at least one statement")
	}
	if len(doc.Statement) > maxStatements {
		errs = append(errs, fmt.Sprintf("Statement array exceeds maximum of %d statements", maxStatements))
	}

	for i, stmt := range doc.Statement {
		prefix := fmt.Sprintf("Statement[%d]", i)

		if !validEffects[stmt.Effect] {
			errs = append(errs, fmt.Sprintf("%s.Effect must be \"Allow\" or \"Deny\"", prefix))
		}

		if len(stmt.Action) == 0 {
			errs = append(errs, fmt.Sprintf("%s.Action must be a non-empty array", prefix))
		}
		for _, a := range stmt.Action {
			if a == "" {
				errs = append(errs, fmt.Sprintf("%s.Action entries must be non-empty strings", prefix))
				break
			}
			if strings.Contains(a, "**") {
				errs = append(errs, fmt.Sprintf("%s.Action: consecutive wildcards not allowed in %q", prefix, a))
			}
		}

		if len(stmt.Resource) == 0 {
			errs = append(errs, fmt.Sprintf("%s.Resource must be a non-empty array", prefix))
		}
		for _, r := range stmt.Resource {
			if r == "" {
				errs = append(errs, fmt.Sprintf("%s.Resource entries must be non-empty strings", prefix))
				break
			}
			if strings.Contains(r, "**") {
				errs = append(errs, fmt.Sprintf("%s.Resource: consecutive wildcards not allowed in %q", prefix, r))
			}
		}

		for op := range stmt.Condition {
			if !validConditionOperators[op] {
				errs = append(errs, fmt.Sprintf("%s.Condition: unknown operator %q", prefix, op))
			}
		}
	}

	return errs
}
