package kit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDash(t *testing.T) {
	if Dash("") != "—" {
		t.Fatalf("empty string should render an em dash, got %q", Dash(""))
	}
	if Dash("x") != "x" {
		t.Fatalf("a non-empty value passes through, got %q", Dash("x"))
	}
}

func TestMs(t *testing.T) {
	if got := Ms(250); got != "250ms" {
		t.Fatalf("sub-second should be ms, got %q", got)
	}
	if got := Ms(1500); got != "1.5s" {
		t.Fatalf("a second or more should be s, got %q", got)
	}
	if got := Ms(1000); got != "1.0s" {
		t.Fatalf("exactly 1000ms is the s boundary, got %q", got)
	}
}

func TestKtok(t *testing.T) {
	if got := Ktok(512); got != "512" {
		t.Fatalf("under 1k is the raw count, got %q", got)
	}
	if got := Ktok(200000); got != "200k" {
		t.Fatalf("thousands abbreviate to k, got %q", got)
	}
	if got := Ktok(1500000); got != "1.5M" {
		t.Fatalf("millions abbreviate to M, got %q", got)
	}
}

func TestOptSession(t *testing.T) {
	got := OptSession([]Session{{Model: "m1"}, {Model: "m2"}})
	if got.Model != "m1" {
		t.Fatalf("should return the first session, got %q", got.Model)
	}
	if OptSession(nil).Model != "" {
		t.Fatal("empty input should return the zero Session")
	}
}

func TestPrettyJSON(t *testing.T) {
	if got := PrettyJSON(json.RawMessage(`{"a":1}`)); !strings.Contains(got, "\n  \"a\": 1") {
		t.Fatalf("valid JSON should be indented, got %q", got)
	}
	if got := PrettyJSON(json.RawMessage(`not json`)); got != "not json" {
		t.Fatalf("invalid JSON should pass through verbatim, got %q", got)
	}
	if PrettyJSON(nil) != "" {
		t.Fatal("empty input should render an empty string")
	}
}
