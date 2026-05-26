package passthrough

import (
	"reflect"
	"testing"
	"time"
)

func TestConfig_NilReceiver_AllSafe(t *testing.T) {
	var c *Config
	if c.AnyBypassActive() {
		t.Errorf("nil.AnyBypassActive() = true, want false")
	}
	if got := c.Flags(); got != nil {
		t.Errorf("nil.Flags() = %v, want nil", got)
	}
}

func TestConfig_EmptyConfig_NoBypass(t *testing.T) {
	c := &Config{}
	if c.AnyBypassActive() {
		t.Errorf("empty.AnyBypassActive() = true, want false")
	}
	if got := c.Flags(); got != nil {
		t.Errorf("empty.Flags() = %v, want nil", got)
	}
}

func TestConfig_DisabledButFlagsSet_NoBypass(t *testing.T) {
	c := &Config{Enabled: false, BypassHooks: true, BypassCache: true, BypassNormalize: true}
	if c.AnyBypassActive() {
		t.Errorf("disabled with all flags set: AnyBypassActive() = true, want false (Enabled is master switch)")
	}
	if got := c.Flags(); got != nil {
		t.Errorf("disabled with all flags set: Flags() = %v, want nil", got)
	}
}

func TestConfig_OneBypassActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"hooks only", Config{Enabled: true, BypassHooks: true}, []string{"bypassHooks"}},
		{"cache only", Config{Enabled: true, BypassCache: true}, []string{"bypassCache"}},
		{"normalize only", Config{Enabled: true, BypassNormalize: true}, []string{"bypassNormalize"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &tc.cfg
			if !c.AnyBypassActive() {
				t.Errorf("AnyBypassActive() = false, want true")
			}
			if got := c.Flags(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Flags() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_AllBypassActive_CanonicalOrder(t *testing.T) {
	c := &Config{
		Enabled:         true,
		BypassHooks:     true,
		BypassCache:     true,
		BypassNormalize: true,
		ExpiresAt:       time.Now().Add(1 * time.Hour),
		EnabledBy:       "user-uuid",
		Reason:          "incident #INC-42, anthropic codec panic",
	}
	if !c.AnyBypassActive() {
		t.Fatalf("AnyBypassActive() = false, want true")
	}
	want := []string{"bypassHooks", "bypassCache", "bypassNormalize"}
	if got := c.Flags(); !reflect.DeepEqual(got, want) {
		t.Errorf("Flags() = %v, want canonical order %v", got, want)
	}
}

func TestConfig_FlagsCanonicalOrder_HooksAndNormalizeOnly(t *testing.T) {
	// Out-of-order spec input: still emits canonical order in Flags().
	c := &Config{Enabled: true, BypassNormalize: true, BypassHooks: true}
	want := []string{"bypassHooks", "bypassNormalize"}
	if got := c.Flags(); !reflect.DeepEqual(got, want) {
		t.Errorf("Flags() = %v, want canonical order %v", got, want)
	}
}
