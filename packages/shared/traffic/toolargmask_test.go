package traffic

import (
	"errors"
	"testing"
)

// maskingAdapter embeds stubAdapter and additionally implements ToolArgMasker
// (reporting true), modeling the OpenAI canonical adapter that masks tool-call
// arguments onto its wire.
type maskingAdapter struct{ stubAdapter }

func (a *maskingAdapter) MasksToolCallArgs() bool { return true }

// disabledMaskingAdapter implements ToolArgMasker but returns false — modeling
// an adapter that declared the capability but cannot currently deliver it.
type disabledMaskingAdapter struct{ stubAdapter }

func (a *disabledMaskingAdapter) MasksToolCallArgs() bool { return false }

func TestToolArgMaskingSupported(t *testing.T) {
	cases := []struct {
		name    string
		adapter Adapter
		want    bool
	}{
		{"plain adapter without capability", &stubAdapter{id: "anthropic"}, false},
		{"adapter implementing ToolArgMasker=true", &maskingAdapter{stubAdapter{id: "openai"}}, true},
		{"adapter implementing ToolArgMasker=false", &disabledMaskingAdapter{stubAdapter{id: "x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolArgMaskingSupported(tc.adapter); got != tc.want {
				t.Fatalf("ToolArgMaskingSupported = %v want %v", got, tc.want)
			}
		})
	}
}

func TestGuardToolArgMasking(t *testing.T) {
	masked := NormalizedContent{ToolCallArgs: []string{`{"q":"[REDACTED]"}`}}
	none := NormalizedContent{} // ToolCallArgs nil
	empty := NormalizedContent{ToolCallArgs: []string{}}

	cases := []struct {
		name    string
		adapter Adapter
		content NormalizedContent
		wantErr bool
	}{
		{
			// The core leak: a non-OpenAI adapter handed masked tool args must
			// fail closed rather than forward them unmasked.
			name:    "non-masking adapter with masked tool args fails closed",
			adapter: &stubAdapter{id: "anthropic"}, content: masked, wantErr: true,
		},
		{
			name:    "masking adapter with masked tool args is allowed",
			adapter: &maskingAdapter{stubAdapter{id: "openai"}}, content: masked, wantErr: false,
		},
		{
			name:    "non-masking adapter with no tool args is a no-op",
			adapter: &stubAdapter{id: "anthropic"}, content: none, wantErr: false,
		},
		{
			name:    "non-masking adapter with empty tool args slice is a no-op",
			adapter: &stubAdapter{id: "anthropic"}, content: empty, wantErr: false,
		},
		{
			name:    "ToolArgMasker returning false with masked args fails closed",
			adapter: &disabledMaskingAdapter{stubAdapter{id: "x"}}, content: masked, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardToolArgMasking(tc.adapter, tc.content)
			switch {
			case tc.wantErr && !errors.Is(err, ErrRewriteUnsupported):
				t.Fatalf("want ErrRewriteUnsupported, got %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

// ensure the test adapters satisfy the interfaces (compile-time guards).
var (
	_ Adapter       = (*maskingAdapter)(nil)
	_ ToolArgMasker = (*maskingAdapter)(nil)
)
