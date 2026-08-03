package config

import (
	"bytes"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit/lossmode"
)

// The shipped config files must name the audit-overflow mode with a literal
// lossmode.Resolve recognises EXACTLY.
//
// The silent failure this guards: Resolve deliberately rescues an unrecognised
// value to the no-loss default rather than rejecting it, because a typo must
// never be able to make the audit trail lossy. That rescue also means a
// misspelled literal in a shipped config produces the right behaviour today
// while being wrong — `lossMode: spillBlock` did exactly that, landing on
// spillblock only because it matched nothing. The file then documents a posture
// it does not actually express, and the day the default changes, its meaning
// changes with it.
//
// This asserts the value survives a round-trip through Resolve unchanged, which
// is true only for the four defined literals.
func TestShippedConfigs_LossModeLiteralIsExact(t *testing.T) {
	for _, path := range []string{"../../ai-gateway.config.yaml", "../../ai-gateway.dev.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var cfg Config
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		got := cfg.Audit.LossMode
		if got == "" {
			t.Fatalf("%s does not state audit.lossMode; the durability posture of a compliance "+
				"product must be readable from its config, not inferred from a Go default", path)
		}
		if resolved := string(lossmode.Resolve(got)); resolved != got {
			t.Fatalf("%s has audit.lossMode=%q, which no defined mode matches — it resolves to %q "+
				"only via the typo-safety rescue, so the file states a posture it does not express",
				path, got, resolved)
		}
	}
}

// The shipped posture itself: the gateway is a compliance product, so both files
// must ship a mode that does not discard records.
func TestShippedConfigs_LossModeIsNoLoss(t *testing.T) {
	for _, path := range []string{"../../ai-gateway.config.yaml", "../../ai-gateway.dev.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var cfg Config
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if mode := lossmode.Resolve(cfg.Audit.LossMode); !mode.NoLoss() {
			t.Fatalf("%s ships audit.lossMode=%q (%s), which can discard audit records", path, cfg.Audit.LossMode, mode)
		}
	}
}

// The spill posture must be STATED, not merely defaulted (finding S-4).
//
// Before this, none of the four server services' *.config.yaml carried a `spill:`
// block at all. The factory then returned (nil, nil) and every captured body was
// kept inline — correct behaviour, but readable only by inferring a Go zero value
// from source. That is the same silence S-3 exists to remove, one layer up: an
// operator reading the shipped template could not tell "spill is off" from "spill
// was never considered".
//
// The distinction this test turns on is why it reads the raw bytes as well as the
// parsed struct: an ABSENT block and `enabled: false` both unmarshal to
// Enabled=false, so only the raw file can say whether the posture was written
// down. That equivalence is also the point — stating it changes documentation,
// never behaviour.
func TestShippedConfigs_SpillPostureIsStated(t *testing.T) {
	cases := []struct {
		path        string
		wantEnabled bool
		why         string
	}{
		{"../../ai-gateway.config.yaml", false,
			"the prod-shaped template must not default to s3: that needs a bucket only the " +
				"operator can create, and a backend pointing at a missing bucket fails every capture"},
		{"../../ai-gateway.dev.yaml", true,
			"local dev spills to localfs so the spill read path is exercised by ordinary runs"},
	}
	for _, c := range cases {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if !bytes.Contains(raw, []byte("\nspill:")) {
			t.Errorf("%s does not state a top-level spill: block. An absent block and "+
				"`enabled: false` behave identically, which is exactly why the posture has to be "+
				"written down: otherwise a reader cannot tell a decision from an omission", c.path)
			continue
		}
		var cfg Config
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", c.path, err)
		}
		if cfg.Spill.Enabled != c.wantEnabled {
			t.Errorf("%s has spill.enabled=%v, want %v — %s",
				c.path, cfg.Spill.Enabled, c.wantEnabled, c.why)
		}
	}
}
