package traffic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
)

func TestListBuiltinTrafficAdapters_OK(t *testing.T) {
	t.Parallel()
	e := echo.New()
	h := New(Deps{Logger: silentLogger()})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/traffic-adapters", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.ListBuiltinTrafficAdapters(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var out struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	want := adapters.BuiltinTrafficAdapterIDs()
	if len(out.Data) != len(want) {
		t.Fatalf("len got %d want %d", len(out.Data), len(want))
	}
	for i := range want {
		if out.Data[i] != want[i] {
			t.Errorf("[%d] = %q want %q", i, out.Data[i], want[i])
		}
	}
}
