package instruments

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The link concatenation could not have.
//
// While the names were assembled — `"metric_rollup_" + string(g)` — nothing
// connected the Go side to the DDL. The schema creates `metric_rollup_1mo`;
// the enum's underlying string was "1mo"; renaming that constant's VALUE moved
// four tables at runtime with no compile error and no failing test. Declaring
// the names is only half the repair; this is the half that keeps them true.
//
// It asserts against the SHIPPED schema rather than a fixture, and refuses to
// run if it could not read one — a test that passes on an empty haystack
// reports "no drift" for the same reason it would report anything else.
func TestRollupTableConstantsExistInSchema(t *testing.T) {
	schemaDir := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "schema")
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatalf("cannot read the shipped schema at %s: %v — without it this test "+
			"could not report a missing table either", schemaDir, err)
	}

	var schema strings.Builder
	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".prisma") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(schemaDir, e.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", e.Name(), readErr)
		}
		schema.Write(b)
		files++
	}
	if files < 3 {
		t.Fatalf("only %d .prisma file(s) found under %s; the scan is not seeing the "+
			"schema, so a missing table could not be reported", files, schemaDir)
	}
	ddl := schema.String()

	// Every declared constant, and the granularity method that must return it.
	cases := []struct {
		name  string
		table string
		via   string
	}{
		{"TableRollup5m", TableRollup5m, Granularity5m.TableName()},
		{"TableRollup1h", TableRollup1h, Granularity1h.TableName()},
		{"TableRollup1d", TableRollup1d, Granularity1d.TableName()},
		{"TableRollup1mo", TableRollup1mo, Granularity1mo.TableName()},
		{"TableThingRollup5m", TableThingRollup5m, Granularity5m.ThingTableName()},
		{"TableThingRollup1h", TableThingRollup1h, Granularity1h.ThingTableName()},
		{"TableThingRollup1d", TableThingRollup1d, Granularity1d.ThingTableName()},
		{"TableThingRollup1mo", TableThingRollup1mo, Granularity1mo.ThingTableName()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(ddl, `@@map("`+tc.table+`")`) {
				t.Errorf("%s = %q, which no model in the shipped schema maps to — "+
					"the code would query a table that does not exist", tc.name, tc.table)
			}
			if tc.via != tc.table {
				t.Errorf("the granularity method returns %q but the constant is %q; "+
					"a caller reaching the table through the method and one reaching it "+
					"through the constant would disagree", tc.via, tc.table)
			}
		})
	}
}

// An unknown tier must name no table at all. Granularity's underlying type is
// string, so any string is a syntactically valid value; concatenation happily
// produced `metric_rollup_<anything>`, and nothing downstream would have known
// the name was invented. "" is rejected by every caller's identifier check.
func TestUnknownGranularityNamesNoTable(t *testing.T) {
	for _, g := range []Granularity{"", "1y", "5 m", "5m; DROP TABLE traffic_event--"} {
		if got := g.TableName(); got != "" {
			t.Errorf("Granularity(%q).TableName() = %q, want \"\"", string(g), got)
		}
		if got := g.ThingTableName(); got != "" {
			t.Errorf("Granularity(%q).ThingTableName() = %q, want \"\"", string(g), got)
		}
	}
}
