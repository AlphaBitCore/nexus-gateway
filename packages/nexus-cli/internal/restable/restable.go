// Package restable holds the shared, presentation-agnostic logic for rendering a
// resource operation's result: detecting a renderable collection, extracting its
// rows, inferring a bounded column set, paginating, and reducing a cell to text.
// The TUI (lipgloss) and the CLI (tabwriter) both consume it, so a table or a
// detail record renders from the SAME data-shape logic on either surface — only
// the styling differs. It does no styling and imports nothing beyond stdlib.
package restable

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Row is one collection item as a decoded JSON object.
type Row = map[string]any

// ExtractRows detects a renderable collection in a raw JSON body and returns its
// rows. It accepts a top-level JSON array, or an object wrapping the array under a
// common key (data | items | results). ok is false when the body is not a list of
// objects — the caller then renders it as a single record.
func ExtractRows(raw json.RawMessage) ([]Row, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '[' {
		if arr, ok := decodeRows(trimmed); ok {
			return arr, true
		}
		return nil, false
	}
	if trimmed[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			for _, key := range []string{"data", "items", "results"} {
				v, ok := obj[key]
				if !ok {
					continue
				}
				if bytes.TrimSpace(v)[0] != '[' {
					continue
				}
				if arr, ok := decodeRows(v); ok {
					return arr, true
				}
			}
		}
	}
	return nil, false
}

// decodeRows decodes a JSON array of objects with UseNumber, so a large integer
// id is preserved exactly (json.Number) instead of being rounded through float64
// — the id drives the next path placeholder when the operator drills a row.
func decodeRows(raw json.RawMessage) ([]Row, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var arr []Row
	if err := dec.Decode(&arr); err != nil {
		return nil, false
	}
	return arr, true
}

// priorityCols are the human-meaningful identity/status columns shown first when
// present, so a table leads with a name/code the operator recognizes rather than a
// bare id (and the id still appears, just not first).
var priorityCols = []string{"name", "displayName", "code", "title", "slug", "email", "status", "enabled", "type", "kind", "id"}

// InferColumns picks an ordered, bounded column set from the union of the rows'
// keys: the priority identity/status columns that are present (in priority order),
// then the remaining keys alphabetically, capped at max (default 6). Deterministic
// for a given set of rows.
func InferColumns(rows []Row, max int) []string {
	if max <= 0 {
		max = 6
	}
	present := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			present[k] = true
		}
	}
	var cols []string
	for _, p := range priorityCols {
		if present[p] {
			cols = append(cols, p)
			delete(present, p)
		}
	}
	rest := make([]string, 0, len(present))
	for k := range present {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	cols = append(cols, rest...)
	if len(cols) > max {
		cols = cols[:max]
	}
	return cols
}

// SanitizeTerminal strips terminal-control sequences and control characters from
// a server-supplied string before it is printed to the operator's terminal. It
// is the defense against terminal-injection: a malicious admin record (a node
// name, an error body) could otherwise embed ANSI escapes to rewrite the title
// bar, set the clipboard (OSC 52), move the cursor, or hide/forge text.
//
// It removes:
//   - ESC-introduced sequences: CSI (ESC [ … final-byte), OSC (ESC ] … BEL or
//     ESC \\ String Terminator), and any other ESC + single-char two-byte escape
//     (so a lone ESC or a charset-select escape cannot leak through);
//   - C0 control characters (0x00–0x1f) EXCEPT tab (\t) and newline (\n);
//   - the DEL character (0x7f);
//   - C1 control characters (0x80–0x9f), which some terminals treat as escapes.
//
// Tab and newline are preserved because they are legitimate layout characters in
// rendered output. Ordinary printable runes (including non-control Unicode) pass
// through unchanged.
func SanitizeTerminal(s string) string {
	if s == "" {
		return s
	}
	rb := []byte(s)
	var b strings.Builder
	b.Grow(len(rb))
	for i := 0; i < len(rb); {
		c := rb[i]
		if c == 0x1b { // ESC — start of an escape sequence
			i++
			if i >= len(rb) {
				break // trailing lone ESC: drop it
			}
			switch rb[i] {
			case '[': // CSI: ESC [ params/intermediates... final byte (0x40–0x7e)
				i++
				for i < len(rb) && !(rb[i] >= 0x40 && rb[i] <= 0x7e) {
					i++
				}
				if i < len(rb) {
					i++ // consume the final byte
				}
			case ']': // OSC: ESC ] ... terminated by BEL (0x07) or ST (ESC \)
				i++
				for i < len(rb) {
					if rb[i] == 0x07 { // BEL terminator
						i++
						break
					}
					if rb[i] == 0x1b && i+1 < len(rb) && rb[i+1] == '\\' { // ST terminator
						i += 2
						break
					}
					i++
				}
			default:
				// nF escapes (charset select etc.): ESC + intermediate bytes (0x20–0x2f)
				// + a final byte (0x30–0x7e). Drop ESC, the intermediates, and the final.
				// Any other byte after ESC is not part of a recognized sequence — drop
				// only the ESC and let the byte be processed normally next iteration.
				if rb[i] >= 0x20 && rb[i] <= 0x2f {
					for i < len(rb) && rb[i] >= 0x20 && rb[i] <= 0x2f {
						i++
					}
					if i < len(rb) {
						i++ // consume the final byte
					}
				}
			}
			continue
		}
		// C0 controls except tab/newline, and DEL, are dropped.
		if (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			i++
			continue
		}
		// C1 controls (0x80–0x9f) — when they appear as a raw byte (invalid UTF-8) the
		// decoder yields RuneError with width 1; strip that single byte. Valid
		// multi-byte UTF-8 runes decode normally and are written through.
		r, size := utf8.DecodeRune(rb[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // a stray byte (incl. raw C1 like 0x9b) — drop it
			continue
		}
		b.Write(rb[i : i+size])
		i += size
	}
	return b.String()
}

// CellString reduces one cell value to compact text: strings as-is, integers
// without a decimal point, other numbers via %g, bools as true/false, and nested
// objects/arrays as a short placeholder so a cell never explodes a row. A missing
// value renders as an em dash.
func CellString(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case string:
		// Server-supplied strings are sanitized so an embedded ANSI/OSC escape or
		// control character cannot inject terminal commands when the cell prints.
		// Numbers/bools below are machine-formatted and need no scrub.
		return SanitizeTerminal(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		return fmt.Sprintf("[%d]", len(t))
	case map[string]any:
		return "{…}"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// ID returns the row's resource id — the value used to fill a path placeholder
// when drilling into the row. It looks only at id-like fields (never name), so a
// drill never substitutes a display label for an id and 404s.
func ID(r Row) string {
	for _, k := range []string{"id", "ID", "uuid", "uid"} {
		if s := stringish(r[k]); s != "" {
			return s
		}
	}
	return ""
}

// Label is a human-friendly name for a row: the first present identity field,
// falling back to the id, then an em dash.
func Label(r Row) string {
	for _, k := range []string{"name", "displayName", "code", "title", "email", "slug"} {
		if s, ok := r[k].(string); ok && s != "" {
			return s
		}
	}
	if id := ID(r); id != "" {
		return id
	}
	return "—"
}

// stringish renders a string or numeric id field as a string ("" for anything
// else, including a missing key or a non-scalar).
func stringish(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// Page is a window over a row set: the rows on the current page plus the paging
// metadata a viewport shows ("page 2/5", 41 total).
type Page struct {
	Rows      []Row
	PageIndex int // 0-based
	PageCount int
	Total     int
	Start     int // 0-based index of the first row on this page
}

// Paginate returns page `index` of rows at pageSize per page, clamping index into
// [0, pageCount). pageSize <= 0 yields a single page holding every row.
func Paginate(rows []Row, index, pageSize int) Page {
	total := len(rows)
	if pageSize <= 0 {
		return Page{Rows: rows, PageIndex: 0, PageCount: 1, Total: total, Start: 0}
	}
	count := (total + pageSize - 1) / pageSize
	if count == 0 {
		count = 1
	}
	if index < 0 {
		index = 0
	}
	if index >= count {
		index = count - 1
	}
	start := index * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return Page{Rows: rows[start:end], PageIndex: index, PageCount: count, Total: total, Start: start}
}
