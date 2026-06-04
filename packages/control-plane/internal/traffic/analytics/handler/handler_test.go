package analytics

import (
	"io"
	"log/slog"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

func TestNew_NilPool(t *testing.T) {
	t.Parallel()
	h := New(Deps{Pool: nil, Logger: nil})
	if h == nil {
		t.Fatal("expected non-nil Handler")
		return
	}
	if h.pool != nil {
		t.Errorf("pool should be nil when Pool nil; got %T", h.pool)
	}
}

func TestNew_WithPoolSetsPool(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(Deps{Pool: mock, Logger: logger})
	if h.pool == nil {
		t.Error("pool not wired")
	}
	if h.logger != logger {
		t.Error("logger not wired")
	}
}

func TestErrJSON_Shape(t *testing.T) {
	t.Parallel()
	out := errJSON("boom", "server_error", "E1234")
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %#v", out)
	}
	if e["message"] != "boom" || e["type"] != "server_error" || e["code"] != "E1234" {
		t.Errorf("unexpected payload: %#v", e)
	}
}

func TestInternalServerError_Status(t *testing.T) {
	t.Parallel()
	e := echo.New()
	c, rec := echoCtx("GET", "/")
	_ = e // keep e referenced
	if err := internalServerError(c, "bad"); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, 500)
}

func TestParsePagination_All(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"", 50, 0},
		{"limit=10&offset=5", 10, 5},
		{"limit=abc&offset=xyz", 50, 0},
		{"limit=0&offset=-3", 50, 0},
		{"limit=99999", 1000, 0},
	}
	for _, tc := range tests {
		c, _ := echoCtx("GET", "/?"+tc.query)
		got := parsePagination(c)
		if got.Limit != tc.wantLimit || got.Offset != tc.wantOffset {
			t.Errorf("parsePagination(%q) = %+v, want limit=%d offset=%d",
				tc.query, got, tc.wantLimit, tc.wantOffset)
		}
	}
}

func TestParseRFC3339Flexible_Variants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"2026-01-02T03:04:05Z", true},
		{"2026-01-02T03:04:05.123456789Z", true},
		{"not-a-time", false},
		{"", false},
	}
	for _, tc := range tests {
		_, ok := parseRFC3339Flexible(tc.in)
		if ok != tc.want {
			t.Errorf("parseRFC3339Flexible(%q) ok=%v, want %v", tc.in, ok, tc.want)
		}
	}
}
