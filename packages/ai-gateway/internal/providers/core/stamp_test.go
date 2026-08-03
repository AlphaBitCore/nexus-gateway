package core

import (
	"bytes"
	"strings"
	"testing"
)

func TestSurgicalModelStamp_RewritesAlias(t *testing.T) {
	out, err := SurgicalModelStamp([]byte(`{"model":"alias","messages":[]}`), "real-model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"real-model"`)) {
		t.Fatalf("model not stamped: %s", out)
	}
}

func TestSurgicalModelStamp_SameModelZeroCopy(t *testing.T) {
	body := []byte(`{"model":"real-model","messages":[]}`)
	out, err := SurgicalModelStamp(body, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	if &out[0] != &body[0] {
		t.Fatal("matching model must return the same slice (zero copy)")
	}
}

func TestSurgicalModelStamp_EmptyInputsVerbatim(t *testing.T) {
	if out, err := SurgicalModelStamp(nil, "m"); err != nil || out != nil {
		t.Fatalf("nil body must pass through: out=%v err=%v", out, err)
	}
	body := []byte(`{"model":"x"}`)
	out, err := SurgicalModelStamp(body, "")
	if err != nil || !bytes.Equal(out, body) {
		t.Fatalf("empty model must pass through: out=%s err=%v", out, err)
	}
}

// The dup-key precondition: sjson edits the FIRST occurrence while parsers
// take last-wins — the decode path must win so the routed model cannot be
// silently wrong.
func TestSurgicalModelStamp_DuplicateModelCollapsesLastWins(t *testing.T) {
	out, err := SurgicalModelStamp([]byte(`{"model":"a","messages":[],"model":"b"}`), "real-model")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), `"model"`) != 1 {
		t.Fatalf("duplicate model keys must collapse to one: %s", out)
	}
	if !bytes.Contains(out, []byte(`"model":"real-model"`)) {
		t.Fatalf("stamp lost on the dup-key path: %s", out)
	}
}

func TestSurgicalModelStamp_DuplicateModelMalformedErrors(t *testing.T) {
	if _, err := SurgicalModelStamp([]byte(`{"model":"a","model":`), "m"); err == nil {
		t.Fatal("malformed dup-key body must surface the decode error")
	}
}
