package proxy

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A request our own codec refuses still went through routing, and the traffic
// record must say which model routing chose.
//
// Driven END TO END through ServeProxy rather than by calling the helper: an
// earlier version of this test exercised stampRoutedTarget directly and stayed
// green when the call site was deleted, which is the same mistake — proving the
// helper works while saying nothing about whether the path uses it.
//
// Measured on production before this landed: a model:auto request carrying a
// markdown document recorded model_name "auto" with routed_model_name and
// routed_provider_name both null, while an upstream refusal on the same
// endpoint recorded gpt-4o-mini/openai. The rows where WE refused — the ones
// that most need attribution — were the only ones without it.
func TestServeProxy_CodecRefusal_StillRecordsTheRoutedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the upstream was called; this request should never have left the gateway")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	prod := &captureProducer{}
	writer := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	cacheOpt, cleanup := withCache(t)
	t.Cleanup(cleanup)

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt, func(d *Deps) {
		d.AuditWriter = writer
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-anthropic",
			ProviderName:    "anthropic",
			ProviderModelID: "claude-x",
			ModelID:         "claude-x",
			ModelName:       "Claude X",
			ModelCode:       "claude-x",
			AdapterType:     "anthropic",
		}}}
	})

	// A zip is neither a PDF nor text, so the Anthropic codec has no document
	// source for it and refuses in our own words — before a byte leaves here.
	zip := base64.StdEncoding.EncodeToString([]byte("PK\x03\x04not-a-document"))
	body := `{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"a.zip","file_data":"data:application/zip;base64,` +
		zip + `"}},{"type":"text","text":"what is this?"}]}]}`

	w := httptest.NewRecorder()
	NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s — expected our codec to refuse", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "application/zip") {
		t.Errorf("the refusal does not name the attachment: %s", w.Body.String())
	}

	// The audit path is a bounded queue drained by a worker, so the record
	// lands shortly after the response. Poll rather than sleeping a fixed
	// interval: a fixed sleep is either flaky or slow, and this assertion is
	// about content, not timing.
	var msgs [][]byte
	for range 200 {
		prod.mu.Lock()
		msgs = append([][]byte(nil), prod.messages...)
		prod.mu.Unlock()
		if len(msgs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatal("no audit message was emitted, so nothing could be asserted about attribution")
	}
	var m map[string]any
	if err := json.Unmarshal(msgs[len(msgs)-1], &m); err != nil {
		t.Fatalf("audit message is not JSON: %v", err)
	}
	flat, _ := json.Marshal(m)
	// Asserted on the SPECIFIC fields. A substring search over the whole record
	// matched "claude-x" in modelName and stayed green with the call site
	// deleted — the third assertion this session that could not see the
	// violation it was written for.
	if got := m["routedModelName"]; got != "claude-x" {
		t.Fatalf("routedModelName = %v, want claude-x — a refusal WE issued cannot be "+
			"attributed to the model routing picked.\n  record: %s", got, flat)
	}
	if got := m["routedProviderName"]; got != "anthropic" {
		t.Errorf("routedProviderName = %v, want anthropic.\n  record: %s", got, flat)
	}
	// And the requested model is still what the caller asked for, not the
	// routed one — the two answer different questions.
	if got := m["modelName"]; got != "auto" {
		t.Errorf("modelName = %v, want the caller's \"auto\"", got)
	}
}
