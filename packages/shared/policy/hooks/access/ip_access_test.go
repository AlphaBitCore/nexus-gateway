package access

import (
	"context"
	"strings"
	"testing"
)

func newIPHook(t *testing.T, cfg map[string]any) Hook {
	t.Helper()
	h, err := NewIPAccessFilter(&HookConfig{
		ID:               "ip-1",
		ImplementationID: "ip-access-filter",
		Name:             "ip-test",
		Stage:            "request",
		Config:           cfg,
	})
	if err != nil {
		t.Fatalf("NewIPAccessFilter: %v", err)
	}
	return h
}

func runIPHook(t *testing.T, h Hook, sourceIP string) *HookResult {
	t.Helper()
	res, err := h.Execute(context.Background(), &HookInput{
		RequestID: "req-x",
		Stage:     "request",
		SourceIP:  sourceIP,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// --- Construction error cases ---------------------------------------------

func TestNewIPAccessFilter_DefaultsToBlocklistMode(t *testing.T) {
	// Unset mode must default to blocklist (the safer choice — opt-in
	// allowlist requires explicit operator action).
	h := newIPHook(t, map[string]any{})
	iaf := h.(*IPAccessFilter)
	if iaf.mode != "blocklist" {
		t.Errorf("default mode: %q, want blocklist", iaf.mode)
	}
}

func TestNewIPAccessFilter_RejectsUnknownMode(t *testing.T) {
	_, err := NewIPAccessFilter(&HookConfig{Config: map[string]any{"mode": "purge"}})
	if err == nil {
		t.Fatal("unknown mode should error")
	}
	if !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("error wording: %v", err)
	}
}

func TestNewIPAccessFilter_RejectsMalformedAllowlist(t *testing.T) {
	_, err := NewIPAccessFilter(&HookConfig{Config: map[string]any{
		"mode":      "allowlist",
		"allowlist": "not-an-array",
	}})
	if err == nil {
		t.Fatal("non-array allowlist should error")
	}
	if !strings.Contains(err.Error(), "array of CIDR strings") {
		t.Errorf("error should mention array shape: %v", err)
	}
}

func TestNewIPAccessFilter_RejectsBadCIDR(t *testing.T) {
	_, err := NewIPAccessFilter(&HookConfig{Config: map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"not-a-cidr"},
	}})
	if err == nil {
		t.Fatal("malformed CIDR should error")
	}
}

func TestNewIPAccessFilter_RejectsEmptyCIDREntry(t *testing.T) {
	_, err := NewIPAccessFilter(&HookConfig{Config: map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{""},
	}})
	if err == nil {
		t.Fatal("empty CIDR entry should error")
	}
}

func TestNewIPAccessFilter_CaseInsensitiveMode(t *testing.T) {
	// "ALLOWLIST" must be accepted — operators sometimes write enums in caps.
	h, err := NewIPAccessFilter(&HookConfig{Config: map[string]any{
		"mode":      "ALLOWLIST",
		"allowlist": []any{"10.0.0.0/8"},
	}})
	if err != nil {
		t.Fatalf("uppercase mode: %v", err)
	}
	iaf := h.(*IPAccessFilter)
	if iaf.mode != "allowlist" {
		t.Errorf("mode normalisation: %q", iaf.mode)
	}
}

// --- Execute: blocklist mode -----------------------------------------------

func TestExecute_BlocklistApprovesUnmatchedIP(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"10.0.0.0/8"},
	})
	res := runIPHook(t, h, "8.8.8.8")
	if res.Decision != Approve {
		t.Errorf("got %q, want Approve", res.Decision)
	}
}

func TestExecute_BlocklistRejectsMatchedIP(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"10.0.0.0/8"},
	})
	res := runIPHook(t, h, "10.5.5.5")
	if res.Decision != RejectHard {
		t.Errorf("got %q, want RejectHard", res.Decision)
	}
	if res.ReasonCode != "IP_ACCESS_DENIED" {
		t.Errorf("reason code: %q", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "blocklisted") {
		t.Errorf("reason should mention blocklisted: %q", res.Reason)
	}
}

// --- Execute: allowlist mode -----------------------------------------------

func TestExecute_AllowlistApprovesListedIP(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "allowlist",
		"allowlist": []any{"10.0.0.0/8", "192.168.0.0/16"},
	})
	res := runIPHook(t, h, "192.168.5.5")
	if res.Decision != Approve {
		t.Errorf("got %q, want Approve", res.Decision)
	}
}

func TestExecute_AllowlistRejectsUnlistedIP(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "allowlist",
		"allowlist": []any{"10.0.0.0/8"},
	})
	res := runIPHook(t, h, "8.8.8.8")
	if res.Decision != RejectHard {
		t.Errorf("got %q, want RejectHard", res.Decision)
	}
}

// --- Execute: both mode (blocklist takes precedence) -----------------------

func TestExecute_BothBlocklistTakesPrecedence(t *testing.T) {
	// An IP listed in BOTH allow and block must be rejected. Without
	// blocklist precedence, a misconfigured allowlist entry could
	// silently re-admit a sanctioned source.
	h := newIPHook(t, map[string]any{
		"mode":      "both",
		"allowlist": []any{"10.0.0.0/8"},
		"blocklist": []any{"10.0.0.99/32"},
	})
	res := runIPHook(t, h, "10.0.0.99")
	if res.Decision != RejectHard {
		t.Errorf("dual-listed: got %q, want RejectHard", res.Decision)
	}
}

func TestExecute_BothApprovesAllowlistedButNotBlocked(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "both",
		"allowlist": []any{"10.0.0.0/8"},
		"blocklist": []any{"10.0.0.99/32"},
	})
	res := runIPHook(t, h, "10.5.5.5")
	if res.Decision != Approve {
		t.Errorf("allowed-but-not-blocked: got %q", res.Decision)
	}
}

func TestExecute_BothRejectsNeitherListed(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "both",
		"allowlist": []any{"10.0.0.0/8"},
		"blocklist": []any{"172.16.0.0/12"},
	})
	res := runIPHook(t, h, "8.8.8.8")
	if res.Decision != RejectHard {
		t.Errorf("unlisted in both: got %q, want RejectHard", res.Decision)
	}
}

// --- Execute: input validation ---------------------------------------------

func TestExecute_InvalidSourceIPRejected(t *testing.T) {
	// Malformed input IP must fail-closed (deny) — never silently
	// approve. A garbage SourceIP could indicate header spoofing.
	h := newIPHook(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"10.0.0.0/8"},
	})
	res := runIPHook(t, h, "not-an-ip")
	if res.Decision != RejectHard {
		t.Errorf("invalid IP: got %q, want RejectHard", res.Decision)
	}
	if res.ReasonCode != "IP_ACCESS_DENIED" {
		t.Errorf("reason code: %q", res.ReasonCode)
	}
}

func TestExecute_IPv6CIDR(t *testing.T) {
	// IPv6 routing must work — without it, dual-stack hosts could
	// bypass blocklists via their v6 address.
	h := newIPHook(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"2001:db8::/32"},
	})
	res := runIPHook(t, h, "2001:db8::1")
	if res.Decision != RejectHard {
		t.Errorf("v6 blocklist: got %q", res.Decision)
	}
}

func TestExecute_LatencyRecorded(t *testing.T) {
	h := newIPHook(t, map[string]any{
		"mode":      "blocklist",
		"blocklist": []any{"10.0.0.0/8"},
	})
	res := runIPHook(t, h, "8.8.8.8")
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs negative: %d", res.LatencyMs)
	}
}

func TestExecute_HookMetadataPropagated(t *testing.T) {
	h, _ := NewIPAccessFilter(&HookConfig{
		ID:               "ip-cfg-1",
		ImplementationID: "ip-access-filter",
		Name:             "named-hook",
		Stage:            "request",
		Config:           map[string]any{},
	})
	res, _ := h.Execute(context.Background(), &HookInput{SourceIP: "8.8.8.8"})
	if res.HookID != "ip-cfg-1" {
		t.Errorf("HookID: %q", res.HookID)
	}
	if res.ImplementationID != "ip-access-filter" {
		t.Errorf("ImplementationID: %q", res.ImplementationID)
	}
	if res.HookName != "named-hook" {
		t.Errorf("HookName: %q", res.HookName)
	}
}
