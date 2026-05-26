package core

import (
	"net/http"
	"testing"
)

func TestSafeHeaders_FiltersBlockedHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer secret")
	in.Set("Cookie", "session=abc")
	in.Set("X-API-Key", "live_xxx")
	in.Set("X-Customer-Tier", "enterprise")
	in.Set("User-Agent", "curl/8.6.0")

	s := NewSafeHeaders(in)

	if got := s.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be filtered, got %q", got)
	}
	if got := s.Get("Cookie"); got != "" {
		t.Errorf("Cookie should be filtered, got %q", got)
	}
	if got := s.Get("X-API-Key"); got != "" {
		t.Errorf("X-API-Key should be filtered, got %q", got)
	}
	if got := s.Get("X-Customer-Tier"); got != "enterprise" {
		t.Errorf("X-Customer-Tier should survive, got %q", got)
	}
	if got := s.Get("User-Agent"); got != "curl/8.6.0" {
		t.Errorf("User-Agent should survive, got %q", got)
	}
}

func TestSafeHeaders_DenyListIsCaseInsensitive(t *testing.T) {
	cases := []string{"Authorization", "AUTHORIZATION", "authorization", "AuThOrIzAtIoN"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			in := http.Header{}
			in.Set(name, "Bearer secret")
			s := NewSafeHeaders(in)
			if got := s.Get("Authorization"); got != "" {
				t.Errorf("variant %q should be filtered, got %q", name, got)
			}
		})
	}
}

func TestSafeHeaders_GetIsCaseInsensitive(t *testing.T) {
	in := http.Header{}
	in.Set("X-Customer-Tier", "enterprise")

	s := NewSafeHeaders(in)

	for _, lookup := range []string{"x-customer-tier", "X-Customer-Tier", "X-CUSTOMER-TIER"} {
		if got := s.Get(lookup); got != "enterprise" {
			t.Errorf("Get(%q) = %q, want %q", lookup, got, "enterprise")
		}
	}
}

func TestSafeHeaders_GetOnAbsentHeader_ReturnsEmpty(t *testing.T) {
	in := http.Header{}
	in.Set("X-Customer-Tier", "enterprise")
	s := NewSafeHeaders(in)
	if got := s.Get("X-Missing"); got != "" {
		t.Errorf("Get on absent header = %q, want %q", got, "")
	}
}

func TestSafeHeaders_ZeroValue_IsUsable(t *testing.T) {
	var s SafeHeaders
	if got := s.Get("anything"); got != "" {
		t.Errorf("zero-value Get = %q, want %q", got, "")
	}
}

func TestNewSafeHeaders_EmptyInput_ReturnsZeroValue(t *testing.T) {
	cases := []struct {
		name string
		in   http.Header
	}{
		{"nil", nil},
		{"empty", http.Header{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSafeHeaders(tc.in)
			if s.h != nil {
				t.Errorf("expected zero-value internal map, got %v", s.h)
			}
			if got := s.Get("any"); got != "" {
				t.Errorf("Get on zero-value = %q, want %q", got, "")
			}
		})
	}
}

func TestSafeHeaders_MultiValueHeader_ReturnsFirst(t *testing.T) {
	in := http.Header{}
	in.Add("X-Forwarded-For", "203.0.113.10")
	in.Add("X-Forwarded-For", "198.51.100.4")

	s := NewSafeHeaders(in)
	if got := s.Get("X-Forwarded-For"); got != "203.0.113.10" {
		t.Errorf("Get on multi-value = %q, want first %q", got, "203.0.113.10")
	}
}

func TestNewSafeHeaders_DoesNotMutateInput(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer secret")
	in.Set("X-Customer-Tier", "enterprise")

	_ = NewSafeHeaders(in)

	// Original map must still contain Authorization — the filter is
	// applied to the internal copy, not the caller's map.
	if got := in.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("input map was mutated: Authorization = %q", got)
	}
}
