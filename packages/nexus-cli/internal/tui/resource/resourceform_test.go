package resource

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	capres "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
)

func TestOpFormFreeAndEnumFields(t *testing.T) {
	specs := []capres.FieldInfo{
		{Name: "name", Type: "string", Required: true},
		{Name: "mode", Type: "string", Enum: []string{"fast", "slow"}},
	}
	var got map[string]string
	f := newOpForm("T", formBody, specs, func(v map[string]string) tea.Cmd {
		got = v
		return nil
	})

	// Submitting with the required free field blank is rejected (no submit).
	_, submitted := f.update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if submitted {
		t.Fatal("a blank required field must block submit")
	}
	if !strings.Contains(f.note, "required") {
		t.Fatalf("missing-required note expected, got %q", f.note)
	}
	// Type the name.
	for _, r := range "ci" {
		f.update(keyRunes(string(r)))
	}
	// Move to the enum field and cycle it to "slow".
	f.update(tea.KeyPressMsg{Code: tea.KeyDown})
	f.update(tea.KeyPressMsg{Code: tea.KeyRight})
	if f.fields[1].choice != 1 {
		t.Fatalf("right should advance the enum choice, got %d", f.fields[1].choice)
	}
	f.update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if f.fields[1].choice != 0 {
		t.Fatalf("left should step the enum choice back, got %d", f.fields[1].choice)
	}
	// Render covers the enum picker (focused) + free field rows.
	if out := f.view(); !strings.Contains(out, "‹") || !strings.Contains(out, "name") {
		t.Fatalf("form view should render the enum picker + fields:\n%s", out)
	}
	// Submit.
	_, submitted = f.update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !submitted {
		t.Fatal("a complete form should submit")
	}
	if got["name"] != "ci" || got["mode"] != "fast" {
		t.Fatalf("submitted values wrong: %v", got)
	}
}

func TestOpFormBodyJSONCoercion(t *testing.T) {
	specs := []capres.FieldInfo{
		{Name: "count", Type: "integer"},
		{Name: "ratio", Type: "number"},
		{Name: "on", Type: "boolean"},
		{Name: "label", Type: "string"},
		{Name: "blank", Type: "string"},
	}
	f := newOpForm("B", formBody, specs, func(map[string]string) tea.Cmd { return nil })
	set := func(i int, s string) {
		f.cur = i
		f.focusCurrent()
		for _, r := range s {
			f.update(keyRunes(string(r)))
		}
	}
	set(0, "5")
	set(1, "0.5")
	set(2, "true")
	set(3, "hi")
	// blank stays empty → omitted
	body := string(f.bodyJSON())
	for _, want := range []string{`"count":5`, `"ratio":0.5`, `"on":true`, `"label":"hi"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s missing %q", body, want)
		}
	}
	if strings.Contains(body, "blank") {
		t.Fatalf("a blank field must be omitted: %s", body)
	}
}

func TestOpFormBodyJSONEmpty(t *testing.T) {
	f := newOpForm("B", formBody, []capres.FieldInfo{{Name: "x"}}, func(map[string]string) tea.Cmd { return nil })
	if f.bodyJSON() != nil {
		t.Fatal("a form with no values must yield a nil body")
	}
}

func TestParamFormAllRequired(t *testing.T) {
	f := paramForm("P", []string{"id", "configKey"}, func(map[string]string) tea.Cmd { return nil })
	if len(f.fields) != 2 || !f.fields[0].required {
		t.Fatalf("paramForm fields wrong: %+v", f.fields)
	}
	// up at the top + down past the end clamp without panic.
	f.update(tea.KeyPressMsg{Code: tea.KeyUp})
	f.update(tea.KeyPressMsg{Code: tea.KeyDown})
	f.update(tea.KeyPressMsg{Code: tea.KeyDown})
	if f.cur != 1 {
		t.Fatalf("cursor should clamp at the last field, got %d", f.cur)
	}
	if !strings.Contains(f.view(), "id") || !strings.Contains(f.Help(), "submit") {
		t.Fatal("form view/help should render")
	}
}

func TestCoerce(t *testing.T) {
	if coerce("5", "integer") != int64(5) {
		t.Fatal("integer coercion")
	}
	if coerce("x", "integer") != "x" {
		t.Fatal("bad integer falls back to string")
	}
	if coerce("1.5", "number") != 1.5 {
		t.Fatal("number coercion")
	}
	if coerce("true", "boolean") != true {
		t.Fatal("boolean coercion")
	}
	if coerce("plain", "string") != "plain" {
		t.Fatal("string passthrough")
	}
}
