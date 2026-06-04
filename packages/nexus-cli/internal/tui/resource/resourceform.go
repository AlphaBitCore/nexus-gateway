package resource

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	capres "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// resourceform.go is the ask-question style input form the /resource cascade uses
// to collect an operation's inputs from the OpenAPI schema: missing path
// parameters, a list's query filters, or a write's request body. Each field is a
// row — an enum field is a left/right choice picker, a free field a text input —
// so the operator fills a form instead of hand-writing JSON or a query string.

type formPurpose int

const (
	formParams formPurpose = iota // collect missing path placeholders
	formFilter                    // collect a list operation's query filters
	formBody                      // collect a write operation's request body
)

// formField is one input row. ti drives a free-text value; for an enum field the
// choice index selects among enum (ti is unused).
type formField struct {
	name     string
	in       string // "path" / "query" / "" (body)
	typ      string
	required bool
	enum     []string
	ti       textinput.Model
	choice   int
}

// opForm is a transient input overlay over a frame. submit turns the collected
// values into a tea.Cmd (run the read/write, or re-filter the list).
type opForm struct {
	title   string
	purpose formPurpose
	fields  []formField
	cur     int
	submit  func(values map[string]string) tea.Cmd
	note    string
}

// newOpForm builds a form from field specs. The first field is focused.
func newOpForm(title string, purpose formPurpose, specs []capres.FieldInfo, submit func(map[string]string) tea.Cmd) *opForm {
	f := &opForm{title: title, purpose: purpose, submit: submit}
	for _, s := range specs {
		fld := formField{name: s.Name, in: s.In, typ: s.Type, required: s.Required, enum: s.Enum}
		if len(s.Enum) == 0 {
			ti := textinput.New()
			ti.Prompt = ""
			ti.SetVirtualCursor(false)
			ti.Placeholder = s.Type
			fld.ti = ti
		}
		f.fields = append(f.fields, fld)
	}
	f.focusCurrent()
	return f
}

// paramForm builds a form for missing path placeholders (all free text, required).
func paramForm(title string, names []string, submit func(map[string]string) tea.Cmd) *opForm {
	specs := make([]capres.FieldInfo, 0, len(names))
	for _, n := range names {
		specs = append(specs, capres.FieldInfo{Name: n, In: "path", Required: true})
	}
	return newOpForm(title, formParams, specs, submit)
}

func (f *opForm) focusCurrent() {
	for i := range f.fields {
		if f.fields[i].enum != nil {
			continue
		}
		if i == f.cur {
			f.fields[i].ti.Focus()
		} else {
			f.fields[i].ti.Blur()
		}
	}
}

// update handles a keystroke while the form is open. done is true when the form
// resolved (submitted → cmd, or cancelled → nil cmd + cancel flag via note).
func (f *opForm) update(msg tea.KeyPressMsg) (cmd tea.Cmd, submitted bool) {
	switch msg.String() {
	case "up", "shift+tab":
		if f.cur > 0 {
			f.cur--
			f.focusCurrent()
		}
		return nil, false
	case "down", "tab":
		if f.cur < len(f.fields)-1 {
			f.cur++
			f.focusCurrent()
		}
		return nil, false
	case "enter":
		if miss := f.missingRequired(); miss != "" {
			f.note = "required: " + miss
			return nil, false
		}
		return f.submit(f.values()), true
	}
	// Field-local editing: enum cycles with left/right; a free field takes the key.
	if len(f.fields) == 0 {
		return nil, false
	}
	fld := &f.fields[f.cur]
	if fld.enum != nil {
		switch msg.String() {
		case "left", "h":
			if fld.choice > 0 {
				fld.choice--
			}
		case "right", "l":
			if fld.choice < len(fld.enum)-1 {
				fld.choice++
			}
		}
		return nil, false
	}
	var c tea.Cmd
	fld.ti, c = fld.ti.Update(msg)
	return c, false
}

// values collects the field values keyed by field name; empty free fields are
// omitted (an unset optional filter / body field).
func (f *opForm) values() map[string]string {
	out := make(map[string]string, len(f.fields))
	for _, fld := range f.fields {
		if fld.enum != nil {
			if fld.choice >= 0 && fld.choice < len(fld.enum) {
				out[fld.name] = fld.enum[fld.choice]
			}
			continue
		}
		if v := strings.TrimSpace(fld.ti.Value()); v != "" {
			out[fld.name] = v
		}
	}
	return out
}

// missingRequired returns the name of the first required field left blank, or "".
func (f *opForm) missingRequired() string {
	for _, fld := range f.fields {
		if !fld.required {
			continue
		}
		if fld.enum != nil {
			continue // an enum always has a value
		}
		if strings.TrimSpace(fld.ti.Value()) == "" {
			return fld.name
		}
	}
	return ""
}

// bodyJSON assembles the form values into a JSON object for a write body, coercing
// each value by its declared type (integer/number/boolean), else a string. Returns
// nil when no fields are set (a no-body write).
func (f *opForm) bodyJSON() json.RawMessage {
	obj := make(map[string]any, len(f.fields))
	for _, fld := range f.fields {
		var raw string
		if fld.enum != nil {
			if fld.choice >= 0 && fld.choice < len(fld.enum) {
				raw = fld.enum[fld.choice]
			}
		} else {
			raw = strings.TrimSpace(fld.ti.Value())
		}
		if raw == "" {
			continue
		}
		obj[fld.name] = coerce(raw, fld.typ)
	}
	if len(obj) == 0 {
		return nil
	}
	b, _ := json.Marshal(obj)
	return b
}

// coerce converts a form string to the JSON type the field declares so a numeric
// or boolean body field is sent as a number/bool, not a quoted string. A value
// that does not parse for its declared type falls back to the string (the server
// 400 is then authoritative).
func coerce(raw, typ string) any {
	switch {
	case strings.Contains(typ, "integer"):
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	case strings.Contains(typ, "number"):
		if n, err := strconv.ParseFloat(raw, 64); err == nil {
			return n
		}
	case strings.Contains(typ, "boolean"):
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	}
	return raw
}

func (f *opForm) view() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(f.title))
	b.WriteString("\n")
	if f.note != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Amber).Render("  " + f.note))
		b.WriteString("\n")
	}
	for i, fld := range f.fields {
		cursor := "  "
		name := fld.name
		if fld.required {
			name += "*"
		}
		if i == f.cur {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			name = lipgloss.NewStyle().Bold(true).Render(name)
		}
		var val string
		if fld.enum != nil {
			val = enumPicker(fld.enum, fld.choice, i == f.cur)
		} else {
			val = fld.ti.View()
		}
		b.WriteString(fmt.Sprintf("%s%-22s %s\n", cursor, name, val))
	}
	b.WriteString(styles.TileLabel.Render("↑/↓ field · ←/→ choose · type to edit · enter submit · esc cancel"))
	return b.String()
}

// enumPicker renders an enum field as a "‹ value ›" chip, brightened when focused.
func enumPicker(enum []string, choice int, focused bool) string {
	v := "—"
	if choice >= 0 && choice < len(enum) {
		v = enum[choice]
	}
	s := "‹ " + v + " ›"
	if focused {
		return lipgloss.NewStyle().Foreground(styles.BrandHi).Bold(true).Render(s)
	}
	return lipgloss.NewStyle().Foreground(styles.Sub).Render(s)
}

func (f *opForm) Help() string {
	return "↑/↓ field · ←/→ choose · type to edit · enter submit · esc cancel"
}
