package generic

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestDetectRequestMetaEmpty(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "http://example.test/custom", nil)
	got := a.DetectRequestMeta(r, nil)
	if got.Provider != "" {
		t.Errorf("provider = %q, want empty", got.Provider)
	}
	if got.Model != "" {
		t.Errorf("model = %q, want empty", got.Model)
	}
}

func TestDetectRequestMetaClassifiesKey(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "http://example.test/custom", nil)
	r.Header.Set("Authorization", "Bearer sk-proj-foo")
	got := a.DetectRequestMeta(r, nil)
	if got.ApiKeyClass != "sk-proj-" {
		t.Errorf("class = %q", got.ApiKeyClass)
	}
	if got.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectResponseUsageAlwaysNonLLM(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`{"data":"anything"}`))
	if um.Status != traffic.UsageStatusNonLLM {
		t.Errorf("status = %q, want non_llm", um.Status)
	}
}
