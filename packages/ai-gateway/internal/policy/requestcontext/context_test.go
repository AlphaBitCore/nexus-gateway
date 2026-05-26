package requestcontext

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestBuilder_FluentChain(t *testing.T) {
	id := &vkauth.VKMeta{ID: "vk-1", Name: "test-vk", OrganizationID: "org-1"}
	payload := &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "hi"}}},
		},
	}
	hdrs := http.Header{}
	hdrs.Set("X-Test", "v")
	raw := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)

	rc := NewBuilder().
		WithIdentity(id).
		WithNormalized(payload).
		WithEndpoint("chat/completions").
		WithHeaders(hdrs).
		WithRawBody(raw).
		Build()

	if rc == nil {
		t.Fatal("Build returned nil")
	}
	if rc.Identity() != id {
		t.Errorf("Identity() = %p, want %p", rc.Identity(), id)
	}
	if rc.Normalized() != payload {
		t.Errorf("Normalized() = %p, want %p", rc.Normalized(), payload)
	}
	if got := rc.Endpoint(); got != "chat/completions" {
		t.Errorf("Endpoint() = %q, want %q", got, "chat/completions")
	}
	if got := rc.Headers().Get("X-Test"); got != "v" {
		t.Errorf("Headers().Get(X-Test) = %q, want %q", got, "v")
	}
	if got := string(rc.RawBody()); got != string(raw) {
		t.Errorf("RawBody() = %q, want %q", got, string(raw))
	}
}

func TestBuilder_PartialPopulation_ReturnsZeroValuesForUnsetFields(t *testing.T) {
	rc := NewBuilder().WithEndpoint("embeddings").Build()

	if rc.Identity() != nil {
		t.Errorf("Identity() = %v, want nil", rc.Identity())
	}
	if rc.Normalized() != nil {
		t.Errorf("Normalized() = %v, want nil", rc.Normalized())
	}
	if got := rc.Endpoint(); got != "embeddings" {
		t.Errorf("Endpoint() = %q, want %q", got, "embeddings")
	}
	if rc.Headers() != nil {
		t.Errorf("Headers() = %v, want nil", rc.Headers())
	}
	if rc.RawBody() != nil {
		t.Errorf("RawBody() = %v, want nil", rc.RawBody())
	}
}

func TestBuilder_EmptyBuild_ReturnsNonNilWithAllZeroFields(t *testing.T) {
	rc := NewBuilder().Build()
	if rc == nil {
		t.Fatal("Build on empty Builder returned nil")
	}
	if rc.Identity() != nil || rc.Normalized() != nil || rc.Endpoint() != "" ||
		rc.Headers() != nil || rc.RawBody() != nil {
		t.Errorf("expected all zero-value getters; got identity=%v normalized=%v endpoint=%q headers=%v rawBody=%v",
			rc.Identity(), rc.Normalized(), rc.Endpoint(), rc.Headers(), rc.RawBody())
	}
}

func TestRequestContext_NilReceiverSafe(t *testing.T) {
	var rc *RequestContext // nil pointer

	if rc.Identity() != nil {
		t.Errorf("nil.Identity() = %v, want nil", rc.Identity())
	}
	if rc.Normalized() != nil {
		t.Errorf("nil.Normalized() = %v, want nil", rc.Normalized())
	}
	if got := rc.Endpoint(); got != "" {
		t.Errorf("nil.Endpoint() = %q, want %q", got, "")
	}
	if rc.Headers() != nil {
		t.Errorf("nil.Headers() = %v, want nil", rc.Headers())
	}
	if rc.RawBody() != nil {
		t.Errorf("nil.RawBody() = %v, want nil", rc.RawBody())
	}
}

func TestBuilder_OneShotSemantic_BuildTwiceReturnsSamePointer(t *testing.T) {
	b := NewBuilder().WithEndpoint("chat/completions")
	first := b.Build()
	second := b.Build()
	if first != second {
		t.Errorf("Build() called twice returned different pointers: %p vs %p — Builder is documented as one-shot",
			first, second)
	}
}

func TestBuilder_SeparateBuildersProduceIndependentContexts(t *testing.T) {
	id1 := &vkauth.VKMeta{ID: "vk-1"}
	id2 := &vkauth.VKMeta{ID: "vk-2"}

	rc1 := NewBuilder().WithIdentity(id1).Build()
	rc2 := NewBuilder().WithIdentity(id2).Build()

	if rc1 == rc2 {
		t.Fatal("two separate Builders returned the same pointer")
	}
	if rc1.Identity() != id1 {
		t.Errorf("rc1.Identity() = %v, want %v", rc1.Identity(), id1)
	}
	if rc2.Identity() != id2 {
		t.Errorf("rc2.Identity() = %v, want %v", rc2.Identity(), id2)
	}
}

func TestRequestContext_GetterReturnsSameReferenceAsBuilderInput(t *testing.T) {
	// The contract is "no defensive copying"; consumers must not mutate.
	// This test pins the contract so future changes to add defensive
	// copying have to revise the docs + tests together.
	hdrs := http.Header{}
	hdrs.Set("X-Original", "yes")
	raw := []byte("body-bytes")

	rc := NewBuilder().WithHeaders(hdrs).WithRawBody(raw).Build()

	if &rc.Headers()[http.CanonicalHeaderKey("X-Original")][0] != &hdrs[http.CanonicalHeaderKey("X-Original")][0] {
		t.Error("Headers() returned a different reference than supplied to WithHeaders — contract is no defensive copying")
	}
	if &rc.RawBody()[0] != &raw[0] {
		t.Error("RawBody() returned a different reference than supplied to WithRawBody — contract is no defensive copying")
	}
}
