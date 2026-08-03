package guardrail

import "testing"

func TestNormalizeStage(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", StageInput, false},
		{"input", StageInput, false},
		{"INPUT", StageInput, false},
		{" output ", StageOutput, false},
		{"sideways", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeStage(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeStage(%q) err=nil, want error", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeStage(%q) = (%q,%v), want (%q,nil)", c.in, got, err, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := (&Request{Content: "hi"}).Validate(); err != nil {
		t.Errorf("content-only should validate, got %v", err)
	}
	if err := (&Request{Messages: []Message{{Content: "hi"}}}).Validate(); err != nil {
		t.Errorf("messages-only should validate, got %v", err)
	}
	if err := (&Request{Content: "a", Messages: []Message{{Content: "b"}}}).Validate(); err == nil {
		t.Error("both content and messages should be rejected")
	}
	if err := (&Request{}).Validate(); err == nil {
		t.Error("neither content nor messages should be rejected")
	}
	if err := (&Request{Content: "   "}).Validate(); err == nil {
		t.Error("whitespace-only content should be rejected")
	}
	if err := (&Request{Messages: []Message{{Role: "user", Content: "  "}}}).Validate(); err == nil {
		t.Error("whitespace-only messages should be rejected")
	}
}
