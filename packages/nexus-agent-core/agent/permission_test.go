package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// detailerTool is a confirm-tier tool that resolves a concrete confirm detail,
// so the gate can name the exact write (the ConfirmDetailer path).
type detailerTool struct {
	*stubTool
	detail string
}

func (d detailerTool) ConfirmDetail(json.RawMessage) string { return d.detail }

func TestGateDecideConfirmDetail(t *testing.T) {
	g := NewGate(nil, nil, false)
	// a confirm-tier tool with a detailer → the gate's Ask reason IS the detail,
	// so the operator authorizes the concrete write, not a generic "mitigation".
	d := detailerTool{stubTool: &stubTool{name: "resource_invoke", tier: TierConfirm}, detail: "PUT /providers/p1 (updateProvider)"}
	if dec, why := g.Decide(d, json.RawMessage(`{}`)); dec != Ask || why != "PUT /providers/p1 (updateProvider)" {
		t.Fatalf("a detailer's detail should be the Ask reason, got dec=%v why=%q", dec, why)
	}
	// an empty detail falls back to the generic reason (never an empty prompt).
	d2 := detailerTool{stubTool: &stubTool{name: "x", tier: TierConfirm}, detail: ""}
	if dec, why := g.Decide(d2, nil); dec != Ask || why != "mitigation requires authorization" {
		t.Fatalf("empty detail should fall back to the generic reason, got %q", why)
	}
	// a confirm tool WITHOUT a detailer → generic reason.
	plain := &stubTool{name: "y", tier: TierConfirm}
	if dec, why := g.Decide(plain, nil); dec != Ask || why != "mitigation requires authorization" {
		t.Fatalf("a non-detailer confirm tool should use the generic reason, got %q", why)
	}
}

func TestGateDecide(t *testing.T) {
	readTool := &stubTool{name: "observe_cost", tier: TierAuto}
	killTool := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	shell := &stubTool{name: "run_command", tier: TierAuto}

	g := NewGate(NewCommandClassifier(), nil, false)

	// Auto tool, benign → Allow.
	if d, _ := g.Decide(readTool, nil); d != Allow {
		t.Fatal("read tool should auto-allow")
	}
	// Confirm-tier tool → Ask regardless of input.
	if d, why := g.Decide(killTool, nil); d != Ask || why == "" {
		t.Fatalf("mitigation should ask, got %v %q", d, why)
	}
	// Auto shell tool, benign command → Allow.
	if d, _ := g.Decide(shell, json.RawMessage(`{"command":"ls -la"}`)); d != Allow {
		t.Fatal("benign shell should auto-allow")
	}
	// Auto shell tool, dangerous command → Ask (classifier escalates).
	if d, why := g.Decide(shell, json.RawMessage(`{"command":"rm -rf /var/data"}`)); d != Ask || why == "" {
		t.Fatalf("rm -rf must escalate to ask, got %v %q", d, why)
	}
}

func TestGateNilClassifier(t *testing.T) {
	// With no classifier, an auto tool always allows (only Tier + allowlist apply).
	g := NewGate(nil, nil, false)
	shell := &stubTool{name: "run_command", tier: TierAuto}
	if d, _ := g.Decide(shell, json.RawMessage(`{"command":"rm -rf /"}`)); d != Allow {
		t.Fatal("nil classifier means an auto tool is not escalated")
	}
	// But a confirm-tier tool still asks.
	kill := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	if d, _ := g.Decide(kill, nil); d != Ask {
		t.Fatal("confirm tier asks even without a classifier")
	}
}

func TestGateAllowlistAndYolo(t *testing.T) {
	shell := &stubTool{name: "run_command", tier: TierAuto}

	// Allowlisted pattern pre-approves an otherwise-dangerous command.
	g := NewGate(NewCommandClassifier(), []string{"git push --force"}, false)
	if d, _ := g.Decide(shell, json.RawMessage(`{"command":"git push --force origin main"}`)); d != Allow {
		t.Fatal("allowlisted dangerous command should be pre-approved")
	}
	// A still-dangerous, non-allowlisted command still asks.
	if d, _ := g.Decide(shell, json.RawMessage(`{"command":"rm -rf /"}`)); d != Ask {
		t.Fatal("non-allowlisted danger should ask")
	}

	// Allowlist also pre-approves a dangerous file write (extractSubject path arm).
	gp := NewGate(NewCommandClassifier(), []string{"/etc/hosts"}, false)
	wf := &stubTool{name: "write_file", tier: TierAuto}
	if d, _ := gp.Decide(wf, json.RawMessage(`{"path":"/etc/hosts","content":"x"}`)); d != Allow {
		t.Fatal("allowlisted system-path write should be pre-approved")
	}

	// YOLO bypasses everything, including confirm-tier mitigations.
	y := NewGate(NewCommandClassifier(), nil, true)
	kill := &stubTool{name: "mitigate_kill", tier: TierConfirm}
	if d, _ := y.Decide(kill, nil); d != Allow {
		t.Fatal("yolo should auto-allow even mitigations")
	}
}

func TestCommandClassifier(t *testing.T) {
	c := NewCommandClassifier()
	cases := []struct {
		name      string
		toolName  string
		input     string
		dangerous bool
	}{
		// Known-safe read/inspect/build commands auto-run.
		{"ls is safe", "run_command", `{"command":"ls -la"}`, false},
		{"go test is safe", "run_command", `{"command":"go test ./..."}`, false},
		{"git status is safe", "run_command", `{"command":"git status"}`, false},
		{"cat is safe", "run_command", `{"command":"cat go.mod"}`, false},
		// Destructive ops carry a precise reason.
		{"rm -rf is dangerous", "run_command", `{"command":"sudo rm -rf /etc"}`, true},
		{"rm split flags evade-proof", "run_command", `{"command":"rm -r -f /data"}`, true},
		{"rm long flags evade-proof", "run_command", `{"command":"rm --recursive --force /"}`, true},
		{"rm -r without force still dangerous", "run_command", `{"command":"rm -r /data"}`, true},
		{"force push -f is dangerous", "run_command", `{"command":"git push -f"}`, true},
		{"force push +refspec is dangerous", "run_command", `{"command":"git push origin +main"}`, true},
		{"drop table is dangerous", "run_command", `{"command":"psql -c 'drop table users'"}`, true},
		{"truncate is dangerous", "run_command", `{"command":"psql -c 'truncate table t'"}`, true},
		{"dd is dangerous", "run_command", `{"command":"dd if=/dev/zero of=/dev/sda"}`, true},
		{"mkfs is dangerous", "run_command", `{"command":"mkfs.ext4 /dev/sdb"}`, true},
		{"shutdown is dangerous", "run_command", `{"command":"shutdown -h now"}`, true},
		{"kill -9 is dangerous", "run_command", `{"command":"killall -9 nginx"}`, true},
		{"chmod -R is dangerous", "run_command", `{"command":"chmod -R 777 /"}`, true},
		{"chown -R is dangerous", "run_command", `{"command":"chown -R root /"}`, true},
		// find/fd are read-only ONLY without an action flag; -delete / -exec turn them
		// into a recursive delete/exec engine, so those forms must require review even
		// though the bare command is a safe-prefix read.
		{"find -delete is dangerous", "run_command", `{"command":"find / -delete"}`, true},
		{"find -exec is dangerous", "run_command", `{"command":"find . -name x -exec rm {} +"}`, true},
		{"find -exec rm -rf is dangerous", "run_command", `{"command":"find / -name x -exec rm -rf {} +"}`, true},
		{"find -execdir is dangerous", "run_command", `{"command":"find . -execdir cat {} +"}`, true},
		{"fd --exec is dangerous", "run_command", `{"command":"fd -x rm"}`, true},
		// …but a bare find/fd search is still a safe read (no action flag).
		{"benign find is still safe", "run_command", `{"command":"find . -name '*.go'"}`, false},
		{"benign fd is still safe", "run_command", `{"command":"fd nexus"}`, false},
		// Fail-safe: unrecognized + compound + privileged + mutating-git all require review.
		{"chmod on sensitive file not auto-run", "run_command", `{"command":"chmod 777 /etc/passwd"}`, true},
		{"git reset --hard requires review", "run_command", `{"command":"git reset --hard"}`, true},
		{"unrecognized command requires review", "run_command", `{"command":"frobnicate --all"}`, true},
		{"compound command requires review", "run_command", `{"command":"ls | grep foo"}`, true},
		{"sudo ls requires review", "run_command", `{"command":"sudo ls"}`, true},
		{"empty command is safe", "run_command", `{"command":"  "}`, false},
		// File writes.
		{"write to working path is safe", "write_file", `{"path":"./out.txt","content":"x"}`, false},
		{"write to /etc is dangerous", "write_file", `{"path":"/etc/hosts","content":"x"}`, true},
		{"edit /usr is dangerous", "edit_file", `{"path":"/usr/local/bin/x"}`, true},
		{"non-command tool is safe", "observe_cost", `{"window":"1h"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := c.Classify(tc.toolName, json.RawMessage(tc.input))
			if got != tc.dangerous {
				t.Fatalf("Classify(%s)=%v want %v (reason=%q)", tc.input, got, tc.dangerous, reason)
			}
			if got && reason == "" {
				t.Fatal("a dangerous verdict must carry a reason")
			}
		})
	}
}

// dynTool is a confirm-tier tool whose DynamicTier downgrades per input.
type dynTool struct{ Tool }

func (d dynTool) TierFor(input json.RawMessage) Tier {
	if strings.Contains(string(input), "readonly") {
		return TierAuto
	}
	return TierConfirm
}

// TestGateHonorsDynamicTier pins the V14 seam: a confirm-tier tool whose
// grounded per-input tier is auto runs without asking; any other input still
// asks. YOLO still bypasses everything.
func TestGateHonorsDynamicTier(t *testing.T) {
	g := NewGate(nil, nil, false)
	base := &stubTool{name: "workflow_run_start", tier: TierConfirm}
	d := dynTool{Tool: base}
	if dec, _ := g.Decide(d, json.RawMessage(`{"mode":"readonly"}`)); dec != Allow {
		t.Fatalf("a grounded-readonly call must run without asking, got %v", dec)
	}
	if dec, _ := g.Decide(d, json.RawMessage(`{"mode":"effectful"}`)); dec != Ask {
		t.Fatalf("an effectful call must still ask, got %v", dec)
	}
	// The static tool (no DynamicTier) keeps asking.
	if dec, _ := g.Decide(base, json.RawMessage(`{}`)); dec != Ask {
		t.Fatalf("a static confirm tool must ask, got %v", dec)
	}
}
