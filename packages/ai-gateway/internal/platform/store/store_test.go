package store

import (
	"encoding/json"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   *string
		wantVal float64
		wantOK  bool
	}{
		{name: "nil input", input: nil, wantVal: 0, wantOK: false},
		{name: "integer", input: strPtr("42"), wantVal: 42, wantOK: true},
		{name: "decimal", input: strPtr("3.14"), wantVal: 3.14, wantOK: true},
		{name: "zero", input: strPtr("0"), wantVal: 0, wantOK: true},
		{name: "negative", input: strPtr("-12.5"), wantVal: -12.5, wantOK: true},
		{name: "large number", input: strPtr("999999.999"), wantVal: 999999.999, wantOK: true},
		{name: "scientific notation", input: strPtr("1.5e2"), wantVal: 150, wantOK: true},
		{name: "empty string", input: strPtr(""), wantVal: 0, wantOK: false},
		{name: "not a number", input: strPtr("abc"), wantVal: 0, wantOK: false},
		{name: "leading whitespace", input: strPtr(" 10"), wantVal: 10, wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVal, gotOK := ParseDecimal(tc.input)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotVal != tc.wantVal {
				t.Errorf("val = %v, want %v", gotVal, tc.wantVal)
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		wantNl bool
	}{
		{name: "empty string returns nil", input: "", wantNl: true},
		{name: "non-empty returns pointer", input: "hello", wantNl: false},
		{name: "whitespace is non-empty", input: " ", wantNl: false},
		{name: "single char", input: "x", wantNl: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NilIfEmpty(tc.input)
			if tc.wantNl {
				if got != nil {
					t.Fatalf("expected nil, got %q", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil pointer")
			}
			if *got != tc.input {
				t.Errorf("pointed value = %q, want %q", *got, tc.input)
			}
		})
	}
}

func TestAllowedModelRef_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []AllowedModelRef
	}{
		{
			name:  "nil slice",
			input: nil,
		},
		{
			name:  "empty slice",
			input: []AllowedModelRef{},
		},
		{
			name: "single model",
			input: []AllowedModelRef{
				{ProviderID: "openai", ModelID: "gpt-4"},
			},
		},
		{
			name: "multiple models",
			input: []AllowedModelRef{
				{ProviderID: "openai", ModelID: "gpt-4"},
				{ProviderID: "anthropic", ModelID: "claude-3"},
			},
		},
		{
			name: "glob pattern",
			input: []AllowedModelRef{
				{ProviderID: "openai", ModelID: "gpt-*"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got []AllowedModelRef
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if tc.input == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}

			if len(got) != len(tc.input) {
				t.Fatalf("length = %d, want %d", len(got), len(tc.input))
			}
			for i := range tc.input {
				if got[i].ProviderID != tc.input[i].ProviderID {
					t.Errorf("[%d] providerId = %q, want %q", i, got[i].ProviderID, tc.input[i].ProviderID)
				}
				if got[i].ModelID != tc.input[i].ModelID {
					t.Errorf("[%d] modelId = %q, want %q", i, got[i].ModelID, tc.input[i].ModelID)
				}
			}
		})
	}
}

func TestAllowedModelRef_UnmarshalFromDB(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		jsonStr string
		want    []AllowedModelRef
		wantErr bool
	}{
		{
			name:    "valid JSON array",
			jsonStr: `[{"providerId":"openai","modelId":"gpt-4"}]`,
			want:    []AllowedModelRef{{ProviderID: "openai", ModelID: "gpt-4"}},
		},
		{
			name:    "empty array",
			jsonStr: `[]`,
			want:    []AllowedModelRef{},
		},
		{
			name:    "multiple entries with extra fields ignored",
			jsonStr: `[{"providerId":"a","modelId":"b","extra":"ignored"},{"providerId":"c","modelId":"d"}]`,
			want: []AllowedModelRef{
				{ProviderID: "a", ModelID: "b"},
				{ProviderID: "c", ModelID: "d"},
			},
		},
		{
			name:    "invalid JSON",
			jsonStr: `not-json`,
			wantErr: true,
		},
		{
			name:    "wrong type (object instead of array)",
			jsonStr: `{"providerId":"x","modelId":"y"}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []AllowedModelRef
			err := json.Unmarshal([]byte(tc.jsonStr), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("length = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }
