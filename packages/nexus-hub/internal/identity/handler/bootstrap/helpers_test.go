package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// makeCtx builds an Echo context for a minimal GET request.
func makeCtx(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestHelpers_BadRequest(t *testing.T) {
	c, rec := makeCtx(t)
	if err := badRequest(c, "test error"); err != nil {
		t.Fatalf("badRequest returned err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d; want 400", rec.Code)
	}
}

func TestHelpers_Unauthorized(t *testing.T) {
	c, rec := makeCtx(t)
	if err := unauthorized(c, "unauth"); err != nil {
		t.Fatalf("unauthorized returned err: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d; want 401", rec.Code)
	}
}

func TestHelpers_Forbidden(t *testing.T) {
	c, rec := makeCtx(t)
	if err := forbidden(c, "forbidden"); err != nil {
		t.Fatalf("forbidden returned err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d; want 403", rec.Code)
	}
}

func TestHelpers_NotFound(t *testing.T) {
	c, rec := makeCtx(t)
	if err := notFound(c, "not found"); err != nil {
		t.Fatalf("notFound returned err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404", rec.Code)
	}
}

func TestHelpers_InternalError(t *testing.T) {
	c, rec := makeCtx(t)
	if err := internalError(c, "boom"); err != nil {
		t.Fatalf("internalError returned err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d; want 500", rec.Code)
	}
}

func TestHelpers_ServiceUnavailable(t *testing.T) {
	c, rec := makeCtx(t)
	if err := serviceUnavailable(c, "svc down"); err != nil {
		t.Fatalf("serviceUnavailable returned err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503", rec.Code)
	}
}

func TestHelpers_HandleErr_NotFound(t *testing.T) {
	c, rec := makeCtx(t)
	if err := handleErr(c, store.ErrNotFound); err != nil {
		t.Fatalf("handleErr(ErrNotFound) returned err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404 for ErrNotFound", rec.Code)
	}
}

func TestHelpers_HandleErr_InternalFallback(t *testing.T) {
	c, rec := makeCtx(t)
	if err := handleErr(c, errSomethingElse); err != nil {
		t.Fatalf("handleErr(other) returned err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d; want 500 for generic error", rec.Code)
	}
}

func TestHelpers_ParseIntDefault(t *testing.T) {
	cases := []struct {
		s    string
		def  int
		want int
	}{
		{"", 10, 10},
		{"5", 10, 5},
		{"abc", 10, 10},
		{"0", 10, 10}, // zero treated as invalid (< 1)
		{"-1", 10, 10},
	}
	for _, tc := range cases {
		got := parseIntDefault(tc.s, tc.def)
		if got != tc.want {
			t.Errorf("parseIntDefault(%q, %d) = %d; want %d", tc.s, tc.def, got, tc.want)
		}
	}
}

func TestHelpers_Clamp(t *testing.T) {
	if clamp(5, 1, 10) != 5 {
		t.Error("clamp(5,1,10) should return 5")
	}
	if clamp(0, 1, 10) != 1 {
		t.Error("clamp(0,1,10) should return 1 (min)")
	}
	if clamp(15, 1, 10) != 10 {
		t.Error("clamp(15,1,10) should return 10 (max)")
	}
}

func TestHelpers_ParseTimeOrNil(t *testing.T) {
	if parseTimeOrNil("") != nil {
		t.Error("empty string must return nil")
	}
	if parseTimeOrNil("not-a-time") != nil {
		t.Error("invalid RFC3339 must return nil")
	}
	ts := "2026-01-15T10:00:00Z"
	got := parseTimeOrNil(ts)
	if got == nil {
		t.Errorf("valid RFC3339 %q must return non-nil time", ts)
	}
}

// errSomethingElse is a non-sentinel error for handleErr testing.
var errSomethingElse = errHelper("something went wrong")

type errHelper string

func (e errHelper) Error() string { return string(e) }
