package exemption

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestStringPtr(t *testing.T) {
	if stringPtr("") != nil {
		t.Error("empty → expected nil")
	}
	if p := stringPtr("x"); p == nil || *p != "x" {
		t.Errorf("non-empty → %v", p)
	}
}

func TestErrJSON_EnvelopeShape(t *testing.T) {
	env := errJSON("msg", "type", "CODE")
	inner, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope missing 'error': %+v", env)
	}
	if inner["message"] != "msg" || inner["type"] != "type" || inner["code"] != "CODE" {
		t.Errorf("envelope inner = %+v", inner)
	}
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		s    string
		want int
		ok   bool
	}{
		{"0", 0, true},
		{"42", 42, true},
		{"-7", -7, true},
		{"", 0, true},
		{"abc", 0, false},
		{"12a", 0, false},
	}
	for _, tc := range cases {
		got, err := parseInt(tc.s)
		if tc.ok && err != nil {
			t.Errorf("parseInt(%q) err = %v, want nil", tc.s, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseInt(%q) err = nil, want non-nil", tc.s)
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseInt(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestParsePagination(t *testing.T) {
	e := echo.New()
	cases := []struct {
		name   string
		query  string
		limit  int
		offset int
	}{
		{"defaults", "", 50, 0},
		{"explicit", "?limit=200&offset=20", 200, 20},
		{"clamped", "?limit=9999", 1000, 0},
		{"garbage", "?limit=abc&offset=-1", 50, 0},
		{"zero-limit", "?limit=0", 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x"+tc.query, nil)
			c := e.NewContext(req, httptest.NewRecorder())
			pg := parsePagination(c)
			if pg.Limit != tc.limit || pg.Offset != tc.offset {
				t.Errorf("got %+v, want {Limit:%d Offset:%d}", pg, tc.limit, tc.offset)
			}
		})
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("no-auth → %+v", a)
	}
}

// TestRegisterRoutes_MountsAll confirms the exemption group wires
// every endpoint at its canonical path.
func TestRegisterRoutes_MountsAll(t *testing.T) {
	h := New(Deps{})
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, noop)
	want := []string{
		"GET /api/admin/compliance/exemption-grants",
		"POST /api/admin/compliance/exemption-grants",
		"PATCH /api/admin/compliance/exemption-grants/:id",
		"DELETE /api/admin/compliance/exemption-grants/:id",
		"GET /api/admin/compliance/exemptions/:id",
		"POST /api/admin/compliance/exemptions/:id/approve",
		"POST /api/admin/compliance/exemptions/:id/reject",
		"POST /api/admin/exemption-requests",
	}
	got := map[string]bool{}
	for _, r := range e.Routes() {
		got[r.Method+" "+r.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing route: %s", w)
		}
	}
}
