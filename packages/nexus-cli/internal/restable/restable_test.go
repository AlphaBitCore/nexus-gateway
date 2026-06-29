package restable

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestExtractRows(t *testing.T) {
	// top-level array
	rows, ok := ExtractRows(json.RawMessage(`[{"id":"a"},{"id":"b"}]`))
	if !ok || len(rows) != 2 || rows[0]["id"] != "a" {
		t.Fatalf("top-level array: %v %v", rows, ok)
	}
	// {data:[…]} wrapper
	rows, ok = ExtractRows(json.RawMessage(`{"data":[{"id":"x"}],"total":1}`))
	if !ok || len(rows) != 1 {
		t.Fatalf("data wrapper: %v %v", rows, ok)
	}
	// {items:[…]} and {results:[…]}
	if _, ok := ExtractRows(json.RawMessage(`{"items":[{"id":"x"}]}`)); !ok {
		t.Fatal("items wrapper not detected")
	}
	if _, ok := ExtractRows(json.RawMessage(`{"results":[{"id":"x"}]}`)); !ok {
		t.Fatal("results wrapper not detected")
	}
	// a single record is NOT a collection
	if _, ok := ExtractRows(json.RawMessage(`{"id":"only","name":"n"}`)); ok {
		t.Fatal("a single record must not be a collection")
	}
	// a scalar / null / empty is not a collection
	for _, body := range []string{`42`, `"hi"`, `null`, ``, `{}`} {
		if _, ok := ExtractRows(json.RawMessage(body)); ok {
			t.Fatalf("%q must not be a collection", body)
		}
	}
	// an empty array IS a (renderable, empty) collection
	if rows, ok := ExtractRows(json.RawMessage(`[]`)); !ok || len(rows) != 0 {
		t.Fatalf("empty array must be an empty collection, got %v %v", rows, ok)
	}
}

func TestInferColumns(t *testing.T) {
	rows := []Row{
		{"id": "1", "name": "alpha", "secretField": "z", "status": "active"},
		{"id": "2", "name": "beta", "extra": 9},
	}
	cols := InferColumns(rows, 6)
	// name leads (priority), id present, all keys covered within the cap.
	if cols[0] != "name" {
		t.Fatalf("name must lead the columns, got %v", cols)
	}
	if !containsCol(cols, "id") || !containsCol(cols, "status") {
		t.Fatalf("priority columns missing: %v", cols)
	}
	// cap respected
	if got := InferColumns(rows, 2); len(got) != 2 {
		t.Fatalf("column cap not respected: %v", got)
	}
	if InferColumns(nil, 6) != nil {
		t.Fatal("no rows → no columns")
	}
}

func TestCellString(t *testing.T) {
	cases := map[string]any{
		"—":    nil,
		"x":    "x",
		"true": true,
		"42":   float64(42),
		"3.5":  float64(3.5),
		"[2]":  []any{1, 2},
		"{…}":  map[string]any{"a": 1},
	}
	for want, v := range cases {
		if got := CellString(v); got != want {
			t.Errorf("CellString(%v) = %q, want %q", v, got, want)
		}
	}
	if got := CellString(json.Number("9007199254740993")); got != "9007199254740993" {
		t.Errorf("json.Number must render exactly, got %q", got)
	}
}

func TestIDAndLabel(t *testing.T) {
	r := Row{"id": "vk-1", "name": "ci-key"}
	if ID(r) != "vk-1" {
		t.Fatalf("ID = %q", ID(r))
	}
	if Label(r) != "ci-key" {
		t.Fatalf("Label = %q", Label(r))
	}
	// id falls back across id-like fields but never to name (a name is not a path id)
	if got := ID(Row{"uuid": "u1"}); got != "u1" {
		t.Fatalf("ID uuid fallback = %q", got)
	}
	if got := ID(Row{"name": "nm"}); got != "" {
		t.Fatalf("ID must not use name, got %q", got)
	}
	// numeric id renders as a string
	if got := ID(Row{"id": float64(7)}); got != "7" {
		t.Fatalf("numeric id = %q", got)
	}
	// label falls back to id, then em dash
	if Label(Row{"id": "only"}) != "only" {
		t.Fatal("label must fall back to id")
	}
	if Label(Row{"k": "v"}) != "—" {
		t.Fatal("label with no identity field is an em dash")
	}
}

func TestPaginate(t *testing.T) {
	rows := make([]Row, 25)
	for i := range rows {
		rows[i] = Row{"id": i}
	}
	p := Paginate(rows, 0, 10)
	if len(p.Rows) != 10 || p.PageCount != 3 || p.Total != 25 || p.Start != 0 {
		t.Fatalf("page 0: %+v", p)
	}
	p = Paginate(rows, 2, 10)
	if len(p.Rows) != 5 || p.PageIndex != 2 || p.Start != 20 {
		t.Fatalf("last page: %+v", p)
	}
	// index clamps into range
	if p := Paginate(rows, 99, 10); p.PageIndex != 2 {
		t.Fatalf("over-range index must clamp, got %d", p.PageIndex)
	}
	if p := Paginate(rows, -5, 10); p.PageIndex != 0 {
		t.Fatalf("negative index must clamp, got %d", p.PageIndex)
	}
	// pageSize <= 0 → single page
	if p := Paginate(rows, 3, 0); len(p.Rows) != 25 || p.PageCount != 1 {
		t.Fatalf("single page: %+v", p)
	}
	// empty rows → one (empty) page
	if p := Paginate(nil, 0, 10); p.PageCount != 1 || len(p.Rows) != 0 {
		t.Fatalf("empty: %+v", p)
	}
}

func TestEdgeBranches(t *testing.T) {
	// a wrapper key whose value is NOT an array is not a collection
	if _, ok := ExtractRows(json.RawMessage(`{"data":{"nested":1}}`)); ok {
		t.Fatal("a non-array wrapper value must not be a collection")
	}
	if _, ok := ExtractRows(json.RawMessage(`{"data":5}`)); ok {
		t.Fatal("a scalar wrapper value must not be a collection")
	}
	// CellString default branch (a Go type the switch does not name)
	if got := CellString(int(5)); got != "5" {
		t.Fatalf("CellString(int) = %q", got)
	}
	// stringish: non-scalar id → "", non-integer float → trimmed
	if got := ID(Row{"id": []any{1}}); got != "" {
		t.Fatalf("non-scalar id must be empty, got %q", got)
	}
	if got := ID(Row{"id": 3.5}); got != "3.5" {
		t.Fatalf("float id = %q", got)
	}
	// InferColumns with max<=0 uses the default cap (not zero columns)
	rows := []Row{{"name": "a", "id": "1", "x": 1, "y": 2, "z": 3, "w": 4, "v": 5}}
	if got := InferColumns(rows, 0); len(got) == 0 || len(got) > 6 {
		t.Fatalf("default cap wrong: %v", got)
	}
}

// TestExtractRowsPreservesBigIntID guards that a large integer id survives row
// extraction exactly (json.Number, not a rounded float64) — the id is fed straight
// into the next path placeholder when the operator drills the row.
func TestExtractRowsPreservesBigIntID(t *testing.T) {
	rows, ok := ExtractRows(json.RawMessage(`[{"id":9007199254740993,"name":"big"}]`))
	if !ok || len(rows) != 1 {
		t.Fatalf("extract: %v %v", rows, ok)
	}
	if got := ID(rows[0]); got != "9007199254740993" {
		t.Fatalf("big-int id must survive exactly, got %q (float64 rounding bug)", got)
	}
	if got := CellString(rows[0]["id"]); got != "9007199254740993" {
		t.Fatalf("big-int cell must render exactly, got %q", got)
	}
}

func containsCol(cols []string, c string) bool {
	for _, x := range cols {
		if x == c {
			return true
		}
	}
	return false
}
