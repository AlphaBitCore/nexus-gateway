package config

import (
	"bytes"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spill posture must be STATED, not merely defaulted (finding S-4).
//
// None of the four server services' *.config.yaml carried a `spill:` block at all.
// The factory then returned (nil, nil) and every captured body stayed inline —
// correct behaviour, readable only by inferring a Go zero value from source. That
// is the same silence S-3 exists to remove, one layer up: an operator reading the
// shipped template could not tell "spill is off" from "spill was never considered".
//
// The raw bytes are read as well as the parsed struct for the reason that makes
// this finding subtle: an ABSENT block and `enabled: false` both unmarshal to
// Enabled=false, so only the file itself can say whether the posture was written
// down. That equivalence is also the point — stating it changes documentation,
// never behaviour.
func TestShippedConfigs_SpillPostureIsStated(t *testing.T) {
	cases := []struct {
		path        string
		wantEnabled bool
		why         string
	}{
		{"../../../compliance-proxy.config.yaml", false,
			"the prod-shaped template must not default to s3: that needs a bucket only the " +
				"operator can create, and a backend pointing at a missing bucket fails every capture"},
		{"../../../compliance-proxy.dev.yaml", true,
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
