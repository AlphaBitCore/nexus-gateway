package analytics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func newContextWithQuery(query string) echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestParseTopN_Clamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		fallback int
		want     int
	}{
		{"no limit param uses fallback", "", 10, 10},
		{"empty limit uses fallback", "limit=", 10, 10},
		{"invalid limit uses fallback", "limit=abc", 10, 10},
		{"zero or negative uses fallback", "limit=0", 5, 5},
		{"negative uses fallback", "limit=-7", 5, 5},
		{"in range is honored", "limit=42", 10, 42},
		{"at cap is honored", "limit=100", 10, 100},
		{"above cap is clamped to maxTopN", "limit=99999", 10, maxTopN},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newContextWithQuery(tc.query)
			got := parseTopN(c, tc.fallback)
			if got != tc.want {
				t.Errorf("parseTopN(%q, %d) = %d, want %d", tc.query, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestParseGroupByParams_Clamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"no pagination params", "", 0, 0},
		{"limit in range", "limit=50", 50, 0},
		{"limit at cap", "limit=1000", 1000, 0},
		{"limit above cap clamps", "limit=999999", maxGroupByLimit, 0},
		{"offset in range", "offset=100", 0, 100},
		{"offset above cap clamps", "offset=999999999", 0, maxGroupByOffset},
		{"combined over cap", "limit=5000&offset=2000000", maxGroupByLimit, maxGroupByOffset},
		{"invalid values fall through", "limit=abc&offset=xyz", 0, 0},
		{"zero/negative are ignored", "limit=0&offset=-1", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newContextWithQuery(tc.query)
			p := parseGroupByParams(c)
			if p.Limit != tc.wantLimit {
				t.Errorf("parseGroupByParams(%q).Limit = %d, want %d", tc.query, p.Limit, tc.wantLimit)
			}
			if p.Offset != tc.wantOffset {
				t.Errorf("parseGroupByParams(%q).Offset = %d, want %d", tc.query, p.Offset, tc.wantOffset)
			}
		})
	}
}
