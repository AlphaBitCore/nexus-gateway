package provtarget

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// errProviderStore returns a fixed error from GetProviderByID.
type errProviderStore struct{ err error }

func (s *errProviderStore) GetProviderByID(_ context.Context, _ string) (ProviderRow, error) {
	return ProviderRow{}, s.err
}

// errModelStore returns a fixed error from GetModelByID.
type errModelStore struct{ err error }

func (s *errModelStore) GetModelByID(_ context.Context, _ string) (ModelRow, error) {
	return ModelRow{}, s.err
}

// configurableProviderStore lets tests override the returned row per-call.
type configurableProviderStore struct{ row ProviderRow }

func (s *configurableProviderStore) GetProviderByID(_ context.Context, _ string) (ProviderRow, error) {
	return s.row, nil
}

// configurableModelStore lets tests override the returned row per-call.
type configurableModelStore struct{ row ModelRow }

func (s *configurableModelStore) GetModelByID(_ context.Context, _ string) (ModelRow, error) {
	return s.row, nil
}

// credStore lets tests control what ResolveForProvider / ListForProvider return.
type credStore struct {
	resolveKey   string
	resolveID    string
	resolveName  string
	resolveErr   error
	candidates   []CredentialCandidate
	listErr      error
	lastCredID   string
	resolveCalls int
}

func (c *credStore) ResolveForProvider(_ context.Context, _ string, credID string) (string, string, string, error) {
	c.lastCredID = credID
	c.resolveCalls++
	if c.resolveErr != nil {
		return "", "", "", c.resolveErr
	}
	id := c.resolveID
	if id == "" {
		id = credID
	}
	return c.resolveKey, id, c.resolveName, nil
}

func (c *credStore) ListForProvider(_ context.Context, _ string) ([]CredentialCandidate, error) {
	return c.candidates, c.listErr
}

// TestResolve_NilReceiver_ReturnsWiringError ensures Resolve guards against
// callers that forgot to fully wire dependencies.
func TestResolve_NilReceiver_ReturnsWiringError(t *testing.T) {
	var r *PgResolver
	_, err := r.Resolve(context.Background(), "p", "m", ResolveHints{})
	if err == nil || !strings.Contains(err.Error(), "not fully wired") {
		t.Fatalf("nil receiver err = %v, want 'not fully wired'", err)
	}
}

// TestResolve_PartiallyWired_ReturnsError covers each nil-field branch.
func TestResolve_PartiallyWired_ReturnsError(t *testing.T) {
	full := &PgResolver{
		Providers:   &fakeProviderStore{},
		Models:      &fakeModelStore{},
		Credentials: &fakeCredentialStore{},
	}
	cases := []struct {
		name string
		mut  func(*PgResolver)
	}{
		{"nil providers", func(r *PgResolver) { r.Providers = nil }},
		{"nil models", func(r *PgResolver) { r.Models = nil }},
		{"nil credentials", func(r *PgResolver) { r.Credentials = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := *full // copy
			tc.mut(&r)
			_, err := r.Resolve(context.Background(), "p", "m", ResolveHints{})
			if err == nil || !strings.Contains(err.Error(), "not fully wired") {
				t.Fatalf("err = %v, want 'not fully wired'", err)
			}
		})
	}
}

// TestResolve_ProviderLookupError wraps the store error with provider id context.
func TestResolve_ProviderLookupError(t *testing.T) {
	r := NewPgResolver(
		&errProviderStore{err: errors.New("db down")},
		&fakeModelStore{},
		&fakeCredentialStore{},
	)
	_, err := r.Resolve(context.Background(), "p-x", "m", ResolveHints{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `provider "p-x"`) || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want wrap with provider id + underlying", err)
	}
	if !errors.Is(err, r.Providers.(*errProviderStore).err) {
		t.Fatalf("err chain broken; %%w must propagate underlying err")
	}
}

// TestResolve_ProviderDisabled returns the dedicated 'disabled' error.
func TestResolve_ProviderDisabled(t *testing.T) {
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai", Disabled: true}},
		&fakeModelStore{},
		&fakeCredentialStore{},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m", ResolveHints{})
	if err == nil || !strings.Contains(err.Error(), `provider "p-1" disabled`) {
		t.Fatalf("err = %v, want 'provider \"p-1\" disabled'", err)
	}
}

// TestResolve_ModelLookupError wraps the model-store error with the model id.
func TestResolve_ModelLookupError(t *testing.T) {
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&errModelStore{err: errors.New("no rows")},
		&fakeCredentialStore{},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-x", ResolveHints{})
	if err == nil || !strings.Contains(err.Error(), `model "m-x"`) || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("err = %v, want wrap with model id + underlying", err)
	}
}

// TestResolve_ModelDisabled returns the dedicated 'disabled' error.
func TestResolve_ModelDisabled(t *testing.T) {
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", Disabled: true}},
		&fakeCredentialStore{},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err == nil || !strings.Contains(err.Error(), `model "m-1" disabled`) {
		t.Fatalf("err = %v, want 'model \"m-1\" disabled'", err)
	}
}

// TestResolve_ModelProviderMismatch surfaces the mismatch with both IDs.
func TestResolve_ModelProviderMismatch(t *testing.T) {
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-other", ProviderModelID: "x"}},
		&fakeCredentialStore{},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `belongs to provider "p-other"`) ||
		!strings.Contains(err.Error(), `not "p-1"`) {
		t.Fatalf("err = %v, want both provider ids in message", err)
	}
}

// TestResolve_CredentialError surfaces credential store errors wrapped with
// the 'credential' prefix from resolveCredential.
func TestResolve_CredentialError(t *testing.T) {
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		&credStore{resolveErr: errors.New("vault locked")},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err == nil || !strings.Contains(err.Error(), "credential") || !strings.Contains(err.Error(), "vault locked") {
		t.Fatalf("err = %v, want credential wrap + underlying", err)
	}
}

// TestResolve_CopiesExtrasAndAddsDeploymentHint covers the Extras-copy loop
// plus the hints.Deployment branch (both the nil-Extras-init path and the
// merge-with-existing path).
func TestResolve_CopiesExtrasAndAddsDeploymentHint(t *testing.T) {
	t.Run("extras copied and deployment merged", func(t *testing.T) {
		r := NewPgResolver(
			&configurableProviderStore{row: ProviderRow{
				ID:          "p-1",
				Name:        "azure",
				AdapterType: "azure-openai",
				BaseURL:     "https://example.openai.azure.com",
				Extras:      map[string]string{"azure.apiVersion": "2024-06-01"},
			}},
			&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "gpt-4o"}},
			&credStore{resolveKey: "sk-azure", resolveID: "c-1", resolveName: "primary"},
		)
		target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{Deployment: "prod-gpt-4o"})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if target.Extras["azure.apiVersion"] != "2024-06-01" {
			t.Fatalf("extras lost: %v", target.Extras)
		}
		if target.Extras["azure.deployment"] != "prod-gpt-4o" {
			t.Fatalf("deployment hint missing: %v", target.Extras)
		}
		if target.CredentialID != "c-1" || target.CredentialName != "primary" {
			t.Fatalf("credential identity not propagated: id=%q name=%q", target.CredentialID, target.CredentialName)
		}
	})

	t.Run("deployment with nil extras allocates map", func(t *testing.T) {
		r := NewPgResolver(
			&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "azure-openai"}},
			&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "gpt-4o"}},
			&credStore{resolveKey: "sk", resolveID: "c-1"},
		)
		target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{Deployment: "dep-x"})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if target.Extras == nil || target.Extras["azure.deployment"] != "dep-x" {
			t.Fatalf("deployment hint not applied to nil-extras provider: %+v", target.Extras)
		}
	})
}

// TestResolveCredential_HintCredentialIDForwarded covers the early-return
// branch when ResolveHints.CredentialID is set: ListForProvider must NOT
// be called.
func TestResolveCredential_HintCredentialIDForwarded(t *testing.T) {
	cs := &credStore{resolveKey: "sk", resolveID: "c-explicit"}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "gpt-4o-mini"}},
		cs,
	)
	target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{CredentialID: "c-explicit"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cs.lastCredID != "c-explicit" {
		t.Fatalf("ResolveForProvider got credID=%q, want forwarded hint c-explicit", cs.lastCredID)
	}
	if target.CredentialID != "c-explicit" {
		t.Fatalf("target.CredentialID = %q", target.CredentialID)
	}
}

// TestResolveCredential_ListError_FallsBackToDefault — when ListForProvider
// errors, resolveCredential must fall back to ResolveForProvider("") (the
// single-credential default).
func TestResolveCredential_ListError_FallsBackToDefault(t *testing.T) {
	cs := &credStore{
		resolveKey: "sk-default",
		resolveID:  "c-default",
		listErr:    errors.New("redis miss"),
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cs.lastCredID != "" {
		t.Fatalf("fallback should call ResolveForProvider with empty credID; got %q", cs.lastCredID)
	}
	if target.CredentialID != "c-default" {
		t.Fatalf("target.CredentialID = %q", target.CredentialID)
	}
}

// TestResolveCredential_EmptyList_FallsBackToDefault is the sibling path:
// no error but no candidates.
func TestResolveCredential_EmptyList_FallsBackToDefault(t *testing.T) {
	cs := &credStore{resolveKey: "sk", resolveID: "c-default", candidates: nil}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cs.lastCredID != "" {
		t.Fatalf("empty-list fallback should pass empty credID; got %q", cs.lastCredID)
	}
}

// TestResolveCredential_SingleCandidate_UsesCandidateID — when ListForProvider
// returns exactly one candidate, resolveCredential forwards its ID directly.
func TestResolveCredential_SingleCandidate_UsesCandidateID(t *testing.T) {
	cs := &credStore{
		resolveKey: "sk",
		resolveID:  "c-only",
		candidates: []CredentialCandidate{{ID: "c-only", Name: "only", Weight: 100}},
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cs.lastCredID != "c-only" {
		t.Fatalf("single-candidate path should forward candidate ID; got %q", cs.lastCredID)
	}
}

// TestResolveCredential_MultipleCandidates_StickyKeyDeterministic — with
// two eligible candidates and a sticky key (no Redis wired), credpool.Select
// hashes the sticky key to pick one of them deterministically. Run twice
// and assert same winner.
func TestResolveCredential_MultipleCandidates_StickyKeyDeterministic(t *testing.T) {
	cs := &credStore{
		resolveKey: "sk",
		candidates: []CredentialCandidate{
			{ID: "c-a", Name: "a", Weight: 50},
			{ID: "c-b", Name: "b", Weight: 50},
		},
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	target1, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{StickyKey: "vk-123"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cs.lastCredID = ""
	target2, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{StickyKey: "vk-123"})
	if err != nil {
		t.Fatalf("Resolve (2): %v", err)
	}
	if target1.CredentialID == "" || target2.CredentialID == "" {
		t.Fatalf("targets missing credential ID: %+v / %+v", target1, target2)
	}
	if target1.CredentialID != target2.CredentialID {
		t.Fatalf("sticky-key selection not deterministic: %q vs %q",
			target1.CredentialID, target2.CredentialID)
	}
	if target1.CredentialID != "c-a" && target1.CredentialID != "c-b" {
		t.Fatalf("sticky winner not in candidate set: %q", target1.CredentialID)
	}
}

// TestResolveCredential_MultipleCandidates_NoStickyKey_WeightedRandom —
// without a sticky key, weighted random returns a non-nil pick. We assert
// only that the winner is one of the candidates so the test is
// non-deterministic-safe; the credpool package's own tests cover statistical
// distribution.
func TestResolveCredential_MultipleCandidates_NoStickyKey_WeightedRandom(t *testing.T) {
	cs := &credStore{
		resolveKey: "sk",
		candidates: []CredentialCandidate{
			{ID: "c-a", Weight: 50},
			{ID: "c-b", Weight: 50},
		},
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.CredentialID != "c-a" && target.CredentialID != "c-b" {
		t.Fatalf("weighted-random winner not in candidate set: %q", target.CredentialID)
	}
}

// TestResolveCredential_AllOpen_ReturnsError — with multiple candidates whose
// weights are all zero (so credpool.filterEligible drops them all),
// credpool.Select returns nil and resolveCredential must surface the
// 'all credentials circuit-open' error. We pass the Redis path via nil
// (no circuit awareness) and rely on weight=0 to mark all entries
// ineligible.
func TestResolveCredential_AllOpen_ReturnsError(t *testing.T) {
	cs := &credStore{
		candidates: []CredentialCandidate{
			{ID: "c-a", Weight: 0},
			{ID: "c-b", Weight: 0},
		},
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{StickyKey: "vk"})
	if err == nil || !strings.Contains(err.Error(), "circuit-open") {
		t.Fatalf("err = %v, want 'circuit-open' message", err)
	}
	// Make sure we never called ResolveForProvider in the all-ineligible path —
	// it returns before invoking the credential store.
	if cs.resolveCalls != 0 {
		t.Fatalf("ResolveForProvider should not be called when all candidates ineligible; called %d times",
			cs.resolveCalls)
	}
}

// TestResolveCredential_MultipleCandidates_WithRedis exercises the
// `if r.Redis != nil` branch in resolveCredential. We wire a nilCmdable-like
// stub that returns empty results so all credentials remain treated as
// closed; then verify Resolve picks one of the candidates without panic.
func TestResolveCredential_MultipleCandidates_WithRedis(t *testing.T) {
	cs := &credStore{
		resolveKey: "sk",
		candidates: []CredentialCandidate{
			{ID: "c-a", Weight: 50},
			{ID: "c-b", Weight: 50},
		},
	}
	r := NewPgResolver(
		&configurableProviderStore{row: ProviderRow{ID: "p-1", AdapterType: "openai"}},
		&configurableModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		cs,
	)
	// Wire a real (miniredis-backed) Redis client so the `if r.Redis != nil`
	// branch in resolveCredential runs the BulkCircuitStates pipeline. The
	// miniredis store is empty so every credential is reported as closed
	// (state == "") and both candidates remain eligible.
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mini.Close)
	r.Redis = redis.NewClient(&redis.Options{Addr: mini.Addr()})
	target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{StickyKey: "vk"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.CredentialID != "c-a" && target.CredentialID != "c-b" {
		t.Fatalf("winner not in candidate set: %q", target.CredentialID)
	}
}
