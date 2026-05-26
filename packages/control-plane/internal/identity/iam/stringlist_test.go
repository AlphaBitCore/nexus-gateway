package iam

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestStringList_Unmarshal_SingleString(t *testing.T) {
	var s StringList
	if err := json.Unmarshal([]byte(`"s3:PutObject"`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual([]string(s), []string{"s3:PutObject"}) {
		t.Errorf("got %v; want [s3:PutObject]", s)
	}
}

func TestStringList_Unmarshal_Array(t *testing.T) {
	var s StringList
	if err := json.Unmarshal([]byte(`["a","b","c"]`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual([]string(s), []string{"a", "b", "c"}) {
		t.Errorf("got %v; want [a b c]", s)
	}
}

func TestStringList_Unmarshal_Empty(t *testing.T) {
	var s StringList
	if err := json.Unmarshal([]byte(`[]`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("empty array → %v; want empty", s)
	}
}

func TestStringList_Unmarshal_Null(t *testing.T) {
	var s StringList
	if err := json.Unmarshal([]byte(`null`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("null → %v; want empty", s)
	}
}

func TestStringList_Unmarshal_Rejects_NonStringForms(t *testing.T) {
	cases := []string{
		`42`,
		`true`,
		`{}`,
		`[1, 2]`,     // array of numbers
		`{"k": "v"}`, // object
		`["a", 5]`,   // mixed array
	}
	for _, c := range cases {
		var s StringList
		if err := json.Unmarshal([]byte(c), &s); err == nil {
			t.Errorf("unmarshal(%q) should have errored; got %v", c, s)
		}
	}
}

func TestStringList_Marshal_SingleStringFormCanonical(t *testing.T) {
	s := StringList{"s3:PutObject"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `"s3:PutObject"` {
		t.Errorf("len==1 should emit bare string, got %s", data)
	}
}

func TestStringList_Marshal_ArrayFormForMultiple(t *testing.T) {
	s := StringList{"a", "b"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `["a","b"]` {
		t.Errorf("len>1 should emit array, got %s", data)
	}
}

// TestStatement_AWSPolicyShapes_RoundTrip covers the three AWS-policy
// shapes the user pasted from the AWS console as round-trip test
// fixtures. Previously, unmarshaling these into our Statement type
// would fail on the single-string Action or Resource forms. Post-fix
// the engine accepts them verbatim, evaluates them correctly, and
// re-serializes back to canonical AWS shape.
func TestStatement_AWSPolicyShapes_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			name: "Vercel marketplace mixed-service",
			json: `{
				"Version": "2012-10-17",
				"Statement": [
					{
						"Effect": "Allow",
						"Action": ["account:CloseAccount","ce:GetCostAndUsage","iam:ListSAMLProviders"],
						"Resource": "*"
					},
					{
						"Sid": "ManageServiceRole",
						"Effect": "Allow",
						"Action": ["iam:GetRole","iam:CreateRole"],
						"Resource": "arn:aws:iam::*:role/Vercel/Service_2026_04_16"
					}
				]
			}`,
		},
		{
			name: "Full wildcard with string forms",
			json: `{
				"Version": "2012-10-17",
				"Statement": [
					{"Effect": "Allow", "Action": "*", "Resource": "*"}
				]
			}`,
		},
		{
			name: "AIOps + SSO mixed verb wildcards",
			json: `{
				"Version": "2012-10-17",
				"Statement": [
					{
						"Sid": "AIOpsReadOnlyAccess",
						"Effect": "Allow",
						"Action": ["aiops:Get*","aiops:List*","aiops:ValidateInvestigationGroup"],
						"Resource": "*"
					},
					{
						"Sid": "SSOManagementAccess",
						"Effect": "Allow",
						"Action": ["identitystore:DescribeUser","sso:DescribeInstance"],
						"Resource": "*"
					}
				]
			}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var doc PolicyDocument
			if err := json.Unmarshal([]byte(c.json), &doc); err != nil {
				t.Fatalf("first unmarshal: %v", err)
			}

			// Round-trip: re-serialize and re-parse should yield an
			// identical PolicyDocument value (StringList canonicalises
			// length==1 to bare string).
			out, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var doc2 PolicyDocument
			if err := json.Unmarshal(out, &doc2); err != nil {
				t.Fatalf("re-unmarshal: %v\noutput was: %s", err, out)
			}
			if !reflect.DeepEqual(doc, doc2) {
				t.Errorf("round-trip drift:\n  before: %#v\n  after:  %#v", doc, doc2)
			}
		})
	}
}

// TestStatement_Canonical_LengthOneEmitsString verifies the AWS
// formatting rule: a Statement whose Action has exactly one element
// serializes back as `"Action": "..."` (bare string), not as a one-
// element array. Same for Resource. Previously the engine had no
// custom marshaler and would always emit arrays; this test pins the
// canonicalising behavior.
func TestStatement_Canonical_LengthOneEmitsString(t *testing.T) {
	stmt := Statement{
		Effect:   "Allow",
		Action:   StringList{"admin:provider.read"},
		Resource: StringList{"nrn:nexus:*:*:*/*"},
	}
	out, err := json.Marshal(stmt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"Action":"admin:provider.read"`) {
		t.Errorf("Action should serialize as bare string when len==1; got %s", s)
	}
	if !strings.Contains(s, `"Resource":"nrn:nexus:*:*:*/*"`) {
		t.Errorf("Resource should serialize as bare string when len==1; got %s", s)
	}
}

func TestStatement_Canonical_MultiElementEmitsArray(t *testing.T) {
	stmt := Statement{
		Effect: "Allow",
		Action: StringList{
			"admin:provider.read", "admin:provider.create",
		},
		Resource: StringList{"nrn:nexus:*:*:provider/openai", "nrn:nexus:*:*:provider/anthropic"},
	}
	out, err := json.Marshal(stmt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"Action":["admin:provider.read","admin:provider.create"]`) {
		t.Errorf("Action should serialize as array when len>1; got %s", s)
	}
	if !strings.Contains(s, `"Resource":["nrn:nexus:*:*:provider/openai","nrn:nexus:*:*:provider/anthropic"]`) {
		t.Errorf("Resource should serialize as array when len>1; got %s", s)
	}
}
