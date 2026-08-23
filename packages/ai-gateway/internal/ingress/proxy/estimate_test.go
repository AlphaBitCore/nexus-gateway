// estimate_test.go — unit tests for POST /v1/estimate.
//
// Covers VK auth gating, target validation (empty / oversize), per-target
// success + failure paths, and summary aggregation (cheapest expected
// total). Uses small stub implementations of VKAuthenticator and
// ModelLookup; no DB, no real estimator dependencies beyond what the
// package already pulls in.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

type stubVKAuth struct {
	err  error
	meta *vkauth.VKMeta
}

func (s *stubVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.meta != nil {
		return s.meta, nil
	}
	return &vkauth.VKMeta{ID: "vk_test"}, nil
}

type stubModels struct {
	byCode map[string]*store.Model
	byID   map[string]*store.Model
}

func (s *stubModels) GetModel(_ context.Context, id string) (*store.Model, error) {
	if m, ok := s.byID[id]; ok {
		return m, nil
	}
	return nil, errors.New("not found")
}
func (s *stubModels) GetModelByCode(_ context.Context, code string) (*store.Model, error) {
	if m, ok := s.byCode[code]; ok {
		return m, nil
	}
	return nil, errors.New("not found")
}
func (s *stubModels) GetModelByCodeOrAlias(_ context.Context, key string) (*store.Model, error) {
	if m, ok := s.byCode[key]; ok {
		return m, nil
	}
	return nil, errors.New("not found")
}
func (s *stubModels) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return nil, nil
}
func (s *stubModels) FetchModelPricing(_ context.Context, _ []string) ([]store.ModelPricing, error) {
	return nil, nil
}

func fPtr(v float64) *float64 { return &v }
func iPtr(v int) *int         { return &v }

func makeEstimateHandler(t *testing.T, auth VKAuthenticator, models ModelLookup) *Handler {
	t.Helper()
	return NewHandler(&Deps{VKAuth: auth, Models: models})
}

func doEstimate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/estimate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeEstimate(w, req)
	return w
}

func TestEstimate_Unauthorized(t *testing.T) {
	h := makeEstimateHandler(t, &stubVKAuth{err: vkauth.ErrMissing}, &stubModels{})
	w := doEstimate(t, h, `{"request":{"model":"x"},"compareTargets":[{"providerId":"p","modelId":"m"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestEstimate_NoTargets(t *testing.T) {
	h := makeEstimateHandler(t, &stubVKAuth{}, &stubModels{})
	w := doEstimate(t, h, `{"request":{"model":"x"},"compareTargets":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	errMap, _ := resp["error"].(map[string]any)
	if code, _ := errMap["code"].(string); code != "ESTIMATE_NO_TARGETS" {
		t.Errorf("code=%q want estimate_no_targets", code)
	}
}

func TestEstimate_TooManyTargets(t *testing.T) {
	h := makeEstimateHandler(t, &stubVKAuth{}, &stubModels{})
	tgts := `[`
	for i := range 11 {
		if i > 0 {
			tgts += ","
		}
		tgts += `{"providerId":"p","modelId":"m"}`
	}
	tgts += `]`
	w := doEstimate(t, h, `{"request":{"model":"x"},"compareTargets":`+tgts+`}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestEstimate_InvalidJSON(t *testing.T) {
	h := makeEstimateHandler(t, &stubVKAuth{}, &stubModels{})
	w := doEstimate(t, h, `not json`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestEstimate_NoRequest(t *testing.T) {
	h := makeEstimateHandler(t, &stubVKAuth{}, &stubModels{})
	w := doEstimate(t, h, `{"compareTargets":[{"providerId":"p","modelId":"m"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestEstimate_HappyPath_CheapestSummary(t *testing.T) {
	models := &stubModels{
		byCode: map[string]*store.Model{
			"gpt-4o": {
				ID:                      "m_gpt4o",
				Code:                    "gpt-4o",
				ProviderID:              "p_oai",
				ProviderName:            "openai",
				ProviderAdapterType:     "openai",
				InputPricePM:            fPtr(2.50),
				OutputPricePM:           fPtr(10.0),
				CachedInputReadPricePM:  fPtr(1.25),
				CachedInputWritePricePM: nil,
				MaxOutputTokens:         iPtr(4096),
			},
			"claude-3-7-sonnet": {
				ID:                      "m_c37",
				Code:                    "claude-3-7-sonnet",
				ProviderID:              "p_ant",
				ProviderName:            "anthropic",
				ProviderAdapterType:     "anthropic",
				InputPricePM:            fPtr(3.0),
				OutputPricePM:           fPtr(15.0),
				CachedInputReadPricePM:  fPtr(0.30),
				CachedInputWritePricePM: fPtr(3.75),
				MaxOutputTokens:         iPtr(8192),
			},
		},
	}
	h := makeEstimateHandler(t, &stubVKAuth{}, models)

	body := `{
		"request": {"model":"gpt-4o","messages":[{"role":"user","content":"hello world"}]},
		"compareTargets": [
			{"providerId":"openai","modelId":"gpt-4o"},
			{"providerId":"anthropic","modelId":"claude-3-7-sonnet"}
		]
	}`
	w := doEstimate(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp EstimateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Targets) != 2 {
		t.Fatalf("targets=%d want 2", len(resp.Targets))
	}
	for _, tgt := range resp.Targets {
		if tgt.Error != nil {
			t.Errorf("unexpected error on %s: %+v", tgt.ModelCode, tgt.Error)
		}
		if tgt.Cost == nil {
			t.Errorf("missing cost on %s", tgt.ModelCode)
		}
	}
	if resp.Summary.SuccessCount != 2 {
		t.Errorf("successCount=%d want 2", resp.Summary.SuccessCount)
	}
	if resp.Summary.ErrorsCount != 0 {
		t.Errorf("errorsCount=%d want 0", resp.Summary.ErrorsCount)
	}
	if resp.Summary.CheapestExpectedTarget == nil {
		t.Fatal("cheapestExpectedTarget should be populated")
	}
	// gpt-4o has lower input + output prices than claude — should win.
	if *resp.Summary.CheapestExpectedTarget != "gpt-4o" {
		t.Errorf("cheapest=%q want gpt-4o", *resp.Summary.CheapestExpectedTarget)
	}
}

func TestEstimate_PerTargetFailure_OneBad(t *testing.T) {
	models := &stubModels{
		byCode: map[string]*store.Model{
			"gpt-4o": {
				ID:                  "m_gpt4o",
				Code:                "gpt-4o",
				ProviderID:          "p_oai",
				ProviderName:        "openai",
				ProviderAdapterType: "openai",
				InputPricePM:        fPtr(2.50),
				OutputPricePM:       fPtr(10.0),
				MaxOutputTokens:     iPtr(4096),
			},
		},
	}
	h := makeEstimateHandler(t, &stubVKAuth{}, models)

	body := `{
		"request": {"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]},
		"compareTargets": [
			{"providerId":"openai","modelId":"gpt-4o"},
			{"providerId":"openai","modelId":"does-not-exist"}
		]
	}`
	w := doEstimate(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp EstimateResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.SuccessCount != 1 || resp.Summary.ErrorsCount != 1 {
		t.Errorf("summary=%+v want success=1 errors=1", resp.Summary)
	}
	var foundErr bool
	for _, t2 := range resp.Targets {
		if t2.ModelCode == "does-not-exist" && t2.Error != nil && t2.Error.Code == "ESTIMATE_TARGET_NOT_FOUND" {
			foundErr = true
		}
	}
	if !foundErr {
		t.Errorf("expected per-target estimate_target_not_found error on missing model; targets=%+v", resp.Targets)
	}
}

// TestEstimate_RerankPricesBySearchUnitNotTokens covers the /v1/estimate rerank
// branch: a rerank target must preview via its per-search-unit rate, not the
// token estimator. 150 documents = ceil(150/100) = 2 search units × 2000 USD/1M
// = $0.004 — a figure the token path (thousands of document "tokens" × the same
// 2000 rate) could never produce.
func TestEstimate_RerankPricesBySearchUnitNotTokens(t *testing.T) {
	rr := &store.Model{
		ID: "m-rr", Code: "rerank-v3.5", Type: "rerank",
		ProviderID: "cohere", ProviderName: "cohere",
		InputPricePM: fPtr(2000), // USD per 1M search units
	}
	models := &stubModels{
		byCode: map[string]*store.Model{"rerank-v3.5": rr},
		byID:   map[string]*store.Model{"m-rr": rr},
	}
	h := makeEstimateHandler(t, &stubVKAuth{}, models)

	docs := make([]string, 150)
	for i := range docs {
		docs[i] = "d"
	}
	request, _ := json.Marshal(map[string]any{"model": "rerank-v3.5", "query": "q", "documents": docs})
	envelope, _ := json.Marshal(map[string]any{
		"request":        json.RawMessage(request),
		"compareTargets": []map[string]string{{"providerId": "cohere", "modelId": "rerank-v3.5"}},
	})
	w := doEstimate(t, h, string(envelope))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Targets []struct {
			Cost *struct {
				Expected struct {
					Total float64 `json:"total"`
				} `json:"expected"`
			} `json:"cost"`
			Assumptions []string `json:"assumptions"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Cost == nil {
		t.Fatalf("want 1 target with cost; body=%s", w.Body.String())
	}
	if got := resp.Targets[0].Cost.Expected.Total; got < 0.0039 || got > 0.0041 {
		t.Errorf("rerank /v1/estimate = $%.6f, want ~$0.004 (2 search units × 2000/1M), not a token figure", got)
	}
}

// estimateExpectedTotal runs one /v1/estimate call for a single target and
// returns the expected total cost, failing the test on any transport error.
func estimateExpectedTotal(t *testing.T, h *Handler, provider, modelID string, request []byte) float64 {
	t.Helper()
	envelope, _ := json.Marshal(map[string]any{
		"request":        json.RawMessage(request),
		"compareTargets": []map[string]string{{"providerId": provider, "modelId": modelID}},
	})
	w := doEstimate(t, h, string(envelope))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Targets []struct {
			Cost *struct {
				Expected struct {
					Total float64 `json:"total"`
				} `json:"expected"`
			} `json:"cost"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Cost == nil {
		t.Fatalf("want 1 target with cost; body=%s", w.Body.String())
	}
	return resp.Targets[0].Cost.Expected.Total
}

// TestEstimate_ImagePricesPerImageNotPromptTokens covers the /v1/estimate image
// path: a per-image model (InputPricePM = USD per 1M images) must price by
// request.n, not by tokenizing the prompt against the per-image rate — which
// would overstate the preview by orders of magnitude.
func TestEstimate_ImagePricesPerImageNotPromptTokens(t *testing.T) {
	img := &store.Model{
		ID: "m-img", Code: "dall-e-3", Type: "image",
		ProviderID: "openai", ProviderName: "openai",
		InputPricePM: fPtr(40000), // USD per 1M images → $0.04/image
	}
	models := &stubModels{
		byCode: map[string]*store.Model{"dall-e-3": img},
		byID:   map[string]*store.Model{"m-img": img},
	}
	h := makeEstimateHandler(t, &stubVKAuth{}, models)

	request, _ := json.Marshal(map[string]any{
		"model":  "dall-e-3",
		"prompt": "a photorealistic red bicycle on a beach at sunset, golden hour, high detail",
		"n":      2,
	})
	got := estimateExpectedTotal(t, h, "openai", "dall-e-3", request)
	// 2 images × 40000 / 1M = $0.08.
	if got < 0.0799 || got > 0.0801 {
		t.Errorf("image /v1/estimate = $%.6f, want ~$0.08 (2 images × 40000/1M), not a token figure", got)
	}
}

// TestEstimate_TTSPricesPerInputCharNotBodyTokens covers the /v1/estimate tts
// path: a per-character model must price by runes of request.input — the same
// count the post-call stamp bills — not by the whole body's token approximation.
func TestEstimate_TTSPricesPerInputCharNotBodyTokens(t *testing.T) {
	tts := &store.Model{
		ID: "m-tts", Code: "gpt-4o-mini-tts", Type: "tts",
		ProviderID: "openai", ProviderName: "openai",
		InputPricePM: fPtr(15000), // USD per 1M characters
	}
	models := &stubModels{
		byCode: map[string]*store.Model{"gpt-4o-mini-tts": tts},
		byID:   map[string]*store.Model{"m-tts": tts},
	}
	h := makeEstimateHandler(t, &stubVKAuth{}, models)

	input := "The quick brown fox jumps over the lazy dog." // 44 runes
	request, _ := json.Marshal(map[string]any{
		"model": "gpt-4o-mini-tts", "voice": "alloy", "input": input,
	})
	got := estimateExpectedTotal(t, h, "openai", "gpt-4o-mini-tts", request)
	// 44 chars × 15000 / 1M = $0.00066.
	want := 44.0 * 15000 / 1e6
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("tts /v1/estimate = $%.8f, want $%.8f (44 input chars × 15000/1M)", got, want)
	}
}
