package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// estimateModels serves a small two-provider catalog so a target can be
// resolved by (code, providerId) rather than by code alone.
type estimateModels struct{ rows []store.Model }

func (m estimateModels) GetModel(_ context.Context, id string) (*store.Model, error) {
	for i := range m.rows {
		if m.rows[i].ID == id {
			return &m.rows[i], nil
		}
	}
	return nil, errors.New("no such id")
}

func (m estimateModels) GetModelByCode(_ context.Context, code string) (*store.Model, error) {
	for i := range m.rows {
		if m.rows[i].Code == code {
			return &m.rows[i], nil
		}
	}
	return nil, errors.New("no such code")
}

func (m estimateModels) GetModelByCodeOrAlias(ctx context.Context, key string) (*store.Model, error) {
	return m.GetModelByCode(ctx, key)
}

func (m estimateModels) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return m.rows, nil
}

func (m estimateModels) FetchModelPricing(context.Context, []string) ([]store.ModelPricing, error) {
	return nil, nil
}

func estimateTestHandler() *Handler {
	maxOut := 8192
	return NewHandler(&Deps{VKAuth: &estimateStubAuth{}, Models: estimateModels{rows: []store.Model{
		{
			ID: "m-a", Code: "shared-code", ProviderID: "prov-a", ProviderName: "Provider A",
			Type: "chat", ProviderAdapterType: "openai", MaxOutputTokens: &maxOut,
			InputPricePM: ptrF(1), OutputPricePM: ptrF(2),
		},
		{
			ID: "m-b", Code: "shared-code", ProviderID: "prov-b", ProviderName: "Provider B",
			Type: "chat", ProviderAdapterType: "openai", MaxOutputTokens: &maxOut,
			InputPricePM: ptrF(10), OutputPricePM: ptrF(20),
		},
	}}})
}

func ptrF(f float64) *float64 { return &f }

func postEstimate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeEstimate(rr, httptest.NewRequest(http.MethodPost, "/v1/estimate", strings.NewReader(body)))
	return rr
}

// options.ingressFormat reached a Prometheus label value straight from the
// request body with no allowlist, so any authenticated caller could mint
// unbounded label cardinality on two counters. It also could not change a
// single number in the answer: it is threaded into ReadReasoningSignal, whose
// body never reads the parameter — that reader tries every dialect's keys
// unconditionally, which is the right design and leaves the field with no job.
func TestServeEstimate_RejectsAnIngressFormatItCannotHonour(t *testing.T) {
	h := estimateTestHandler()

	rr := postEstimate(t, h, `{"request":{"messages":[{"role":"user","content":"hi"}]},
		"compareTargets":[{"providerId":"prov-a","modelId":"shared-code"}],
		"options":{"ingressFormat":"arbitrary-caller-string"}}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an unrecognised value must be refused, not passed through to a metric label", rr.Code)
	}
	if got := gjson.GetBytes(rr.Body.Bytes(), "error.code").String(); got != "ESTIMATE_INVALID_INGRESS_FORMAT" {
		t.Errorf("error.code = %q, want ESTIMATE_INVALID_INGRESS_FORMAT (body %s)", got, rr.Body)
	}
}

func TestServeEstimate_AcceptsEveryFormatItNames(t *testing.T) {
	h := estimateTestHandler()
	for _, f := range []string{"openai", "anthropic", "gemini"} {
		rr := postEstimate(t, h, `{"request":{"messages":[{"role":"user","content":"hi"}]},
			"compareTargets":[{"providerId":"prov-a","modelId":"shared-code"}],
			"options":{"ingressFormat":"`+f+`"}}`)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (body %s)", f, rr.Code, rr.Body)
		}
	}
}

// compareTargets[].reasoningEffort was validated and then dropped. The
// estimator reads effort from the request body only, so a caller who set the
// per-target override got the default estimate back — and the validation made
// that look deliberate rather than broken.
func TestServeEstimate_ReasoningEffortOverrideChangesTheAnswer(t *testing.T) {
	h := estimateTestHandler()

	rr := postEstimate(t, h, `{"request":{"messages":[{"role":"user","content":"hi"}]},
		"compareTargets":[
			{"providerId":"prov-a","modelId":"shared-code","reasoningEffort":"minimal"},
			{"providerId":"prov-a","modelId":"shared-code","reasoningEffort":"high"}
		]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body)
	}
	low := gjson.GetBytes(rr.Body.Bytes(), "targets.0.reasoning.effortRequested").String()
	high := gjson.GetBytes(rr.Body.Bytes(), "targets.1.reasoning.effortRequested").String()
	if low != "minimal" || high != "high" {
		t.Fatalf("effortRequested = %q / %q, want minimal / high — the override never reached the estimator (body %s)", low, high, rr.Body)
	}
}

// compareTargets[].providerId was echoed back but took no part in resolving the
// model: the lookup is by code, so two providers serving the same code both
// resolved to whichever row the catalog returned first. A compare across two
// providers of one model — the endpoint's whole purpose — priced both at the
// same provider's rates.
func TestServeEstimate_ProviderIDPicksBetweenTwoProvidersOfOneCode(t *testing.T) {
	h := estimateTestHandler()

	rr := postEstimate(t, h, `{"request":{"messages":[{"role":"user","content":"hi"}]},
		"compareTargets":[
			{"providerId":"prov-a","modelId":"shared-code"},
			{"providerId":"prov-b","modelId":"shared-code"}
		]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body)
	}
	a := gjson.GetBytes(rr.Body.Bytes(), "targets.0.providerId").String()
	b := gjson.GetBytes(rr.Body.Bytes(), "targets.1.providerId").String()
	if a != "prov-a" || b != "prov-b" {
		t.Fatalf("resolved providers %q / %q, want prov-a / prov-b (body %s)", a, b, rr.Body)
	}
	costA := gjson.GetBytes(rr.Body.Bytes(), "targets.0.cost.expected.total").Float()
	costB := gjson.GetBytes(rr.Body.Bytes(), "targets.1.cost.expected.total").Float()
	if costA == costB {
		t.Errorf("both targets priced at %v; provider B is ten times provider A, so a compare that cannot tell them apart answers the wrong question", costA)
	}
}
