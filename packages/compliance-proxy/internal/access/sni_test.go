package access

import (
	"errors"
	"testing"
)

func TestCheckSNI_Match(t *testing.T) {
	if err := CheckSNI("api.openai.com", "api.openai.com"); err != nil {
		t.Errorf("expected match: %v", err)
	}
}

func TestCheckSNI_CaseInsensitive(t *testing.T) {
	if err := CheckSNI("API.OpenAI.COM", "api.openai.com"); err != nil {
		t.Errorf("expected case-insensitive match: %v", err)
	}
}

func TestCheckSNI_TrailingDot(t *testing.T) {
	if err := CheckSNI("api.openai.com.", "api.openai.com"); err != nil {
		t.Errorf("expected match with trailing dot: %v", err)
	}
	if err := CheckSNI("api.openai.com", "api.openai.com."); err != nil {
		t.Errorf("expected match with trailing dot on connect host: %v", err)
	}
	if err := CheckSNI("API.OpenAI.COM.", "api.openai.com."); err != nil {
		t.Errorf("expected match with both trailing dots and case diff: %v", err)
	}
}

func TestCheckSNI_Mismatch(t *testing.T) {
	err := CheckSNI("evil.com", "api.openai.com")
	if err == nil {
		t.Error("expected mismatch error")
	}
	if !errors.Is(err, ErrSNIMismatch) {
		t.Errorf("expected ErrSNIMismatch, got: %v", err)
	}
}
