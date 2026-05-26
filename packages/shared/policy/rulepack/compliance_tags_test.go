// packages/shared/policy/rulepack/compliance_tags_test.go
package rulepack

import "testing"

func TestTagConstants_Defined(t *testing.T) {
	want := []string{
		TagDetectorPromptInjection,
		TagDetectorJailbreak,
		TagDetectorSecretLeak,
		TagDetectorToolCallSafety,
		TagDetectorContentSafety,
	}
	for _, v := range want {
		if v == "" {
			t.Errorf("tag constant is empty")
		}
	}
}
