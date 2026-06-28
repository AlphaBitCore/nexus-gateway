package alerting

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// Severity is the typed enum that gates every channel-filter,
// rule-default, and admin-payload severity field. These tests pin the
// five accepted values, the strict / loose parser semantics, JSON
// round-trip stability, and the list-parser's per-index error reporting.

func TestSeverityIsValid(t *testing.T) {
	for _, s := range AllSeverities {
		if !s.IsValid() {
			t.Errorf("AllSeverities entry %q reported invalid", s)
		}
	}
	for _, bogus := range []Severity{"", "CRITICAL", "warning", "fatal", "Critical"} {
		if bogus.IsValid() {
			t.Errorf("bogus value %q reported valid; canonical form is lowercase only", bogus)
		}
	}
}

func TestSeverityString(t *testing.T) {
	// String() normalises any underlying cast back to lowercase so log
	// chips / metric labels are stable even if a caller smuggles a
	// raw-string cast through an old code path.
	if got := Severity("CRITICAL").String(); got != "critical" {
		t.Errorf("String() on uppercase cast = %q, want %q", got, "critical")
	}
	if got := SeverityInfo.String(); got != "info" {
		t.Errorf("String() on SeverityInfo = %q, want %q", got, "info")
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Severity
		wantErr bool
	}{
		{"critical", SeverityCritical, false},
		{"high", SeverityHigh, false},
		{"medium", SeverityMedium, false},
		{"low", SeverityLow, false},
		{"info", SeverityInfo, false},
		// Parse is strict (case-sensitive). Anything other than the
		// canonical lowercase form is rejected so admin POST bodies
		// fail fast with a 400 instead of silently persisting a typo.
		{"CRITICAL", "", true},
		{"Critical", "", true},
		{"warning", "", true},
		{"", "", true},
		{" critical ", "", true},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) want err; got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) unexpected err: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseLoose(t *testing.T) {
	// ParseLoose accepts any case — used at the DB-boundary scanner
	// where the Prisma AlertSeverity enum yields uppercase rows.
	for _, in := range []string{"CRITICAL", "Critical", "critical", "CriTIcal"} {
		got, err := ParseLoose(in)
		if err != nil {
			t.Errorf("ParseLoose(%q) unexpected err: %v", in, err)
		}
		if got != SeverityCritical {
			t.Errorf("ParseLoose(%q) = %q, want %q", in, got, SeverityCritical)
		}
	}
	if _, err := ParseLoose("warning"); err == nil {
		t.Errorf("ParseLoose(%q) want err on unknown value", "warning")
	}
}

func TestSeverityErrorMessage(t *testing.T) {
	// The error string must name the offending input and the valid set so
	// admin clients can fix a typo without reading the source.
	_, err := Parse("bogus")
	if err == nil {
		t.Fatal("want err")
	}
	msg := err.Error()
	for _, needle := range []string{"bogus", "critical", "high", "medium", "low", "info"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("Parse error %q missing %q", msg, needle)
		}
	}
}

func TestSeverityJSONRoundTrip(t *testing.T) {
	for _, s := range AllSeverities {
		buf, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", s, err)
		}
		// Always lowercase — see types.go.
		want := `"` + s.String() + `"`
		if string(buf) != want {
			t.Errorf("Marshal(%q) = %s, want %s", s, buf, want)
		}
		var round Severity
		if err := json.Unmarshal(buf, &round); err != nil {
			t.Errorf("Unmarshal(%s): %v", buf, err)
		}
		if round != s {
			t.Errorf("round-trip(%q) = %q", s, round)
		}
	}
}

func TestSeverityUnmarshalRejectsUnknown(t *testing.T) {
	// UnmarshalJSON gates admin POST bodies — Channel.Severities is the
	// canonical caller. Unknown values must surface as a JSON decode
	// error so the handler can return 400.
	var s Severity
	if err := json.Unmarshal([]byte(`"warning"`), &s); err == nil {
		t.Error(`Unmarshal("warning") want err`)
	}
	if err := json.Unmarshal([]byte(`"CRITICAL"`), &s); err == nil {
		t.Error(`Unmarshal("CRITICAL") want err — UnmarshalJSON is strict`)
	}
	if err := json.Unmarshal([]byte(`123`), &s); err == nil {
		t.Error("Unmarshal(non-string) want err")
	}
}

func TestSeverityListInChannel(t *testing.T) {
	// Channel.Severities is []Severity, so encoding/json decoding a
	// channel payload runs each element through UnmarshalJSON; an array
	// containing a typo aborts the whole decode (no partial state).
	good := `{"id":"c1","name":"n","type":"webhook","enabled":true,"severities":["critical","high"],"sourceTypes":[],"config":{},"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`
	var ch Channel
	if err := json.Unmarshal([]byte(good), &ch); err != nil {
		t.Fatalf("decode good payload: %v", err)
	}
	if len(ch.Severities) != 2 || ch.Severities[0] != SeverityCritical || ch.Severities[1] != SeverityHigh {
		t.Errorf("decoded severities = %v, want [critical, high]", ch.Severities)
	}

	bad := `{"id":"c1","name":"n","type":"webhook","enabled":true,"severities":["critical","oops"],"sourceTypes":[],"config":{},"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}`
	var ch2 Channel
	if err := json.Unmarshal([]byte(bad), &ch2); err == nil {
		t.Error("decode bad payload: want err on unknown severity element")
	}
}

func TestParseSeverityList(t *testing.T) {
	got, err := ParseSeverityList([]string{"critical", "info"})
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if len(got) != 2 || got[0] != SeverityCritical || got[1] != SeverityInfo {
		t.Errorf("got %v", got)
	}

	// The error names the offending index so admin clients can pinpoint
	// the bad element in a multi-value query string.
	_, err = ParseSeverityList([]string{"critical", "warning", "info"})
	if err == nil {
		t.Fatal("want err on bad element")
	}
	if !strings.Contains(err.Error(), "[1]") {
		t.Errorf("err %q should pinpoint index 1", err)
	}
}

func TestDBSeverityRoundTrip(t *testing.T) {
	// dbSeverity is the only writer to the AlertRule.defaultSeverity /
	// Alert.severity Prisma enum columns; it must uppercase every value.
	for _, s := range AllSeverities {
		got := dbSeverity(s)
		if got != strings.ToUpper(s.String()) {
			t.Errorf("dbSeverity(%q) = %q", s, got)
		}
		back := goSeverity(got)
		if back != s {
			t.Errorf("round-trip via DB form: %q -> %q -> %q", s, got, back)
		}
	}
	// goSeverity tolerates a future enum value that Go doesn't know
	// about — the dispatcher can still compare strings without panic.
	if got := goSeverity("FUTURE_TIER"); got != Severity("future_tier") {
		t.Errorf("goSeverity unknown = %q", got)
	}
}

func TestDBSeverityListRoundTrip(t *testing.T) {
	// dbSeverityList writes the AlertChannel.severities free-form
	// text[] column in lowercase canonical form (NOT uppercase — that
	// column is not the Prisma enum), and goSeverityList funnels every
	// DB row back through ParseLoose so mixed-case legacy rows
	// normalise to typed Severity values.
	in := []Severity{SeverityCritical, SeverityInfo}
	wire := dbSeverityList(in)
	if len(wire) != 2 || wire[0] != "critical" || wire[1] != "info" {
		t.Errorf("dbSeverityList = %v", wire)
	}
	round := goSeverityList(wire)
	if len(round) != 2 || round[0] != SeverityCritical || round[1] != SeverityInfo {
		t.Errorf("round-trip = %v", round)
	}

	// Empty input yields empty (not nil) slice to match the
	// initialise-on-create contract in CreateChannel.
	got := dbSeverityList(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("dbSeverityList(nil) = %v, want empty non-nil slice", got)
	}

	// Mixed-case legacy rows survive via ParseLoose's normalisation.
	legacy := goSeverityList([]string{"CRITICAL", "High", "info"})
	if len(legacy) != 3 || legacy[0] != SeverityCritical || legacy[1] != SeverityHigh || legacy[2] != SeverityInfo {
		t.Errorf("legacy mixed-case round-trip = %v", legacy)
	}
}

// adminHandler exercises the admin HTTP boundary on the typed enum:
// CreateChannel rejects unknown severity values with 400, ListAlerts
// rejects unknown ?severity= query params with 400, and UpdateRule
// rejects an invalid defaultSeverity body field with 400.

func TestAdminCreateChannelRejectsUnknownSeverity(t *testing.T) {
	h := &AdminHandlers{
		Store: &adminFakeStore{
			insertChannelFn: func(_ context.Context, _ Channel) (string, error) {
				t.Fatal("insertChannelFn should not be reached on validation error")
				return "", nil
			},
		},
		Logger: quietLogger(),
	}
	e := echo.New()
	body := []byte(`{"name":"n","type":"webhook","enabled":true,"severities":["critical","warning"],"sourceTypes":[],"config":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/alerts/channels", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.CreateChannel(c); err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "warning") {
		t.Errorf("error body %q should mention the offending value", rec.Body.String())
	}
}

func TestAdminCreateChannelAcceptsTypedSeverities(t *testing.T) {
	stored := Channel{}
	h := &AdminHandlers{
		Store: &adminFakeStore{
			insertChannelFn: func(_ context.Context, c Channel) (string, error) {
				stored = c
				return "c1", nil
			},
			getChannelFn: func(_ context.Context, id string) (*Channel, error) {
				cp := stored
				cp.ID = id
				return &cp, nil
			},
		},
		Logger: quietLogger(),
	}
	e := echo.New()
	body := []byte(`{"name":"n","type":"webhook","enabled":true,"severities":["critical","high"],"sourceTypes":[],"config":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/alerts/channels", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.CreateChannel(c); err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(stored.Severities) != 2 || stored.Severities[0] != SeverityCritical || stored.Severities[1] != SeverityHigh {
		t.Errorf("stored severities = %v", stored.Severities)
	}
}

func TestAdminListAlertsRejectsUnknownSeverityQueryParam(t *testing.T) {
	h := &AdminHandlers{
		Store: &adminFakeStore{
			listAlertsFn: func(_ context.Context, _ ListFilter) ([]Alert, int, error) {
				t.Fatal("listAlertsFn should not be reached")
				return nil, 0, nil
			},
		},
		Logger: quietLogger(),
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/alerts?severity=critical&severity=oops", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "oops") {
		t.Errorf("error body %q should pinpoint the bad value", rec.Body.String())
	}
}

func TestAdminUpdateRuleRejectsUnknownDefaultSeverity(t *testing.T) {
	existing := AlertRule{ID: "r1", DisplayName: "x", SourceType: "test", DefaultSeverity: SeverityHigh}
	h := &AdminHandlers{
		Store: &adminFakeStore{
			getRuleFn: func(_ context.Context, _ string) (*AlertRule, error) {
				cp := existing
				return &cp, nil
			},
			updateRuleFn: func(_ context.Context, _ AlertRule) error {
				t.Fatal("updateRuleFn should not be reached on validation error")
				return nil
			},
		},
		Logger: quietLogger(),
	}
	e := echo.New()
	body := []byte(`{"defaultSeverity":"fatal"}`)
	req := httptest.NewRequest(http.MethodPut, "/alerts/rules/r1", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("r1")

	if err := h.UpdateRule(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
