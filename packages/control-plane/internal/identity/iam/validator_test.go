package iam

import (
	"strings"
	"testing"
)

func TestValidatePolicyDocument(t *testing.T) {
	// Build a 51-statement doc to drive the maxStatements branch (>50).
	tooMany := make([]Statement, 51)
	for i := range tooMany {
		tooMany[i] = Statement{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}}
	}

	tests := []struct {
		name    string
		doc     *PolicyDocument
		wantErr int // expected number of errors
	}{
		{"nil doc", nil, 1},
		{"valid super admin", &NexusSuperAdmin, 0},
		{"valid viewer", &NexusViewer, 0},
		{"empty version", &PolicyDocument{Statement: []Statement{{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}}}}, 1},
		{"empty statement", &PolicyDocument{Version: "v1", Statement: []Statement{}}, 1},
		{"too many statements (>50)", &PolicyDocument{Version: "v1", Statement: tooMany}, 1},
		{"invalid effect", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Maybe", Action: []string{"*"}, Resource: []string{"*"}}}}, 1},
		{"consecutive wildcards", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{"admin:**"}, Resource: []string{"*"}}}}, 1},
		{"empty action array", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{}, Resource: []string{"*"}}}}, 1},
		{"empty action entry", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{""}, Resource: []string{"*"}}}}, 1},
		{"empty resource array", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{"*"}, Resource: []string{}}}}, 1},
		{"empty resource entry", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{"*"}, Resource: []string{""}}}}, 1},
		{"resource with consecutive wildcards", &PolicyDocument{Version: "v1", Statement: []Statement{{Effect: "Allow", Action: []string{"*"}, Resource: []string{"nrn:**:foo"}}}}, 1},
		{"unknown condition operator", &PolicyDocument{Version: "v1", Statement: []Statement{{
			Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"},
			Condition: ConditionBlock{"FooOp": {"x": "y"}},
		}}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidatePolicyDocument(tt.doc)
			if len(errs) != tt.wantErr {
				t.Errorf("ValidatePolicyDocument() errors = %v (count=%d), want count=%d", errs, len(errs), tt.wantErr)
			}
		})
	}
}

// TestValidatePolicyDocument_MessageProvenance pins the prefix
// `Statement[i].` so the operator-fix loop in the admin UI can index
// errors by statement number. Without this the index could regress to
// a generic `Statement.` and the UI's per-row error highlighting
// would silently break.
func TestValidatePolicyDocument_MessageProvenance(t *testing.T) {
	doc := &PolicyDocument{Version: "v1", Statement: []Statement{
		{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}},
		{Effect: "Maybe", Action: []string{"*"}, Resource: []string{"*"}}, // bad effect at index 1
	}}
	errs := ValidatePolicyDocument(doc)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "Statement[1]") {
		t.Errorf("error did not prefix Statement[1]: %q", errs[0])
	}
}
