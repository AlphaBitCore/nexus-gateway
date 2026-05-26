package interception

import (
	"encoding/json"
	"testing"
)

func TestKillswitch_JSONRoundTrip(t *testing.T) {
	in := Killswitch{Engaged: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"engaged":true}`
	if string(b) != want {
		t.Fatalf("marshal = %s, want %s", b, want)
	}
	var out Killswitch
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
}
