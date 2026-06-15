package vkauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/jackc/pgx/v5"
)

// fakeLookup is an in-memory VKLookup. The Authenticator depends on
// VKLookup as an interface — a fake satisfying just GetVirtualKeyByHash
// is sufficient and avoids dragging pgxmock into vkauth tests.
type fakeLookup struct {
	byHash map[string]*store.VirtualKey
	err    error
}

func (f *fakeLookup) GetVirtualKeyByHash(_ context.Context, h string) (*store.VirtualKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	vk, ok := f.byHash[h]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return vk, nil
}

// quietLogger returns a slog.Logger that discards output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewAuthenticator_WithSecret(t *testing.T) {
	lookup := &fakeLookup{}
	a := NewAuthenticator(lookup, mustKeyring("real-secret"), quietLogger())
	if a == nil {
		t.Fatal("nil authenticator")
	}
	// SEC-W2-01 Layer A: the authenticator stores the HKDF-derived VK-domain
	// sub-key PER keyring version (current first), not the raw master. A
	// single-version keyring yields exactly one stored sub-key.
	wantSub := keyderive.DeriveSubkey([]byte("real-secret"), keyderive.ClassAPIKeyVirtualKey)
	if len(a.vkHashKeys) != 1 {
		t.Fatalf("vkHashKeys len = %d, want 1 for a single-version keyring", len(a.vkHashKeys))
	}
	if string(a.vkHashKeys[0]) != string(wantSub[:]) {
		t.Error("vkHashKeys[0] is not the derived VK sub-key of the master")
	}
	if a.db == nil {
		t.Error("db not wired")
	}
}

// TestAuthenticate_TryAllVersions_NoLazy is the SEC-W2-01 Layer A core: a VK
// whose stored hash was sealed under an OLDER keyring version still admits
// (try-all, current-first). VKs are NOT lazy-migrated (the ai-gw admission path
// is read-only), so the stored hash is unchanged after admission — the VK is
// pruned by re-issue/expiry, not migrated in place.
func TestAuthenticate_TryAllVersions_NoLazy(t *testing.T) {
	kr := mustKeyringMap("v1:old-secret,*v2:current-secret")
	const token = "nvk_rotated_key_aaaaaaaa"
	// The VK was issued under v1 (old) and never re-issued; current is v2.
	oldHash := vkHashFor("old-secret", token)
	curHash := vkHashFor("current-secret", token)
	if oldHash == curHash {
		t.Fatal("old and current hashes must differ for this test to be meaningful")
	}
	lookup := &fakeLookup{byHash: map[string]*store.VirtualKey{
		oldHash: {ID: "vk-old", Enabled: true},
	}}
	a := NewAuthenticator(lookup, kr, quietLogger())

	meta, err := a.Authenticate(context.Background(), reqWithBearer(token))
	if err != nil {
		t.Fatalf("Authenticate under an old keyring version: %v", err)
	}
	if meta.ID != "vk-old" {
		t.Errorf("admitted VK ID=%q, want vk-old", meta.ID)
	}
	// No lazy migration: the stored map is untouched (still keyed by oldHash, and
	// the current-version hash was never written).
	if _, ok := lookup.byHash[oldHash]; !ok {
		t.Error("VK must NOT be removed/migrated on admission (read-only path)")
	}
	if _, ok := lookup.byHash[curHash]; ok {
		t.Error("VK must NOT be re-hashed to the current version (no lazy migrate)")
	}
}

// Authenticate — security-critical

// authTestHelper builds an authenticator wired to fakeLookup and a
// request carrying the given Bearer token.
func newAuthTestRig(t *testing.T, vk *store.VirtualKey, lookupErr error) (*Authenticator, *fakeLookup) {
	t.Helper()
	a := NewAuthenticator(&fakeLookup{}, mustKeyring("test-secret"), quietLogger())
	hash := vkHashFor("test-secret", "nvk_testkey_aaaaaaaaaaaaaa") // length > 4, prefix nvk_
	lookup := &fakeLookup{
		byHash: map[string]*store.VirtualKey{},
		err:    lookupErr,
	}
	if vk != nil {
		lookup.byHash[hash] = vk
	}
	a.db = lookup
	return a, lookup
}

func reqWithBearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestAuthenticate_MissingHeader(t *testing.T) {
	a, _ := newAuthTestRig(t, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrMissing) {
		t.Errorf("err = %v, want ErrMissing", err)
	}
}

func TestAuthenticate_RejectsShortNonPrefixedToken(t *testing.T) {
	// "engineering-team" is 16 chars + no nvk_ prefix → looksLikeRealKey false.
	// Security guarantee: only the hashed-key path is permitted.
	a, _ := newAuthTestRig(t, nil, nil)
	r := reqWithBearer("short-token")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestAuthenticate_HashMissFromDB(t *testing.T) {
	// pgx.ErrNoRows from the lookup must surface as ErrInvalid (not
	// leak the underlying error class).
	a, _ := newAuthTestRig(t, nil, nil)
	r := reqWithBearer("nvk_unknown_key_aaaaaaaaa")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestAuthenticate_DBErrorWraps(t *testing.T) {
	// Non-ErrNoRows DB errors must wrap ErrInvalid AND keep the
	// underlying error chain (verified by errors.Is on both).
	dbErr := errors.New("connection refused")
	a, _ := newAuthTestRig(t, nil, dbErr)
	r := reqWithBearer("nvk_anything_aaaaaaaaaaaa")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err must wrap ErrInvalid; got %v", err)
	}
	if !errors.Is(err, dbErr) {
		t.Errorf("err must wrap dbErr; got %v", err)
	}
	if !strings.Contains(err.Error(), "hash lookup") {
		t.Errorf("err must mention hash lookup; got %v", err)
	}
}

func TestAuthenticate_DisabledKey(t *testing.T) {
	vk := &store.VirtualKey{ID: "vk-1", Name: "disabled", Enabled: false}
	a, _ := newAuthTestRig(t, vk, nil)
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

func TestAuthenticate_ExpiredKey(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	vk := &store.VirtualKey{ID: "vk-1", Enabled: true, ExpiresAt: &past}
	a, _ := newAuthTestRig(t, vk, nil)
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestAuthenticate_NonActiveStatusRejected(t *testing.T) {
	tests := []string{"pending", "rejected", "revoked", "expired"}
	for _, st := range tests {
		t.Run(st, func(t *testing.T) {
			status := st
			vk := &store.VirtualKey{ID: "vk-1", Enabled: true, VKStatus: &status}
			a, _ := newAuthTestRig(t, vk, nil)
			r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
			_, err := a.Authenticate(context.Background(), r)
			if !errors.Is(err, ErrDisabled) {
				t.Errorf("status=%s: err = %v, want ErrDisabled", st, err)
			}
			if !strings.Contains(err.Error(), "status "+st) {
				t.Errorf("status=%s: missing status in err: %v", st, err)
			}
		})
	}
}

func TestAuthenticate_EmptyStatusAllowed(t *testing.T) {
	// Empty *string status (no status set) must NOT trip the
	// status-disabled branch — that would lock out legacy rows.
	empty := ""
	vk := &store.VirtualKey{ID: "vk-1", Name: "n", Enabled: true, VKStatus: &empty}
	a, _ := newAuthTestRig(t, vk, nil)
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	meta, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if meta == nil || meta.ID != "vk-1" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestAuthenticate_SuccessPopulatesAllMetaFields(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	orgID := "org-1"
	orgName := "Acme"
	orgTZ := "America/Los_Angeles"
	projID := "proj-1"
	projName := "Proj"
	src := "my-app"
	owner := "user-1"
	user := "Alice"
	vkType := "application"
	status := "active"
	rpm := 100
	cmp := 60
	vk := &store.VirtualKey{
		ID:                          "vk-1",
		Name:                        "vk-name",
		Enabled:                     true,
		ExpiresAt:                   &exp,
		OrganizationID:              &orgID,
		OrganizationName:            &orgName,
		OrganizationTimezone:        &orgTZ,
		ProjectID:                   &projID,
		ProjectName:                 &projName,
		SourceApp:                   &src,
		OwnerID:                     &owner,
		UserDisplayName:             &user,
		VKType:                      &vkType,
		VKStatus:                    &status,
		RateLimitRpm:                &rpm,
		CompareEndpointRateLimitRpm: &cmp,
		AllowedModels:               []store.AllowedModelRef{{ProviderID: "openai", ModelID: "gpt-4o"}},
	}
	a, _ := newAuthTestRig(t, vk, nil)
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	meta, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Spot-check every populated pointer field surfaces in VKMeta.
	if meta.ID != "vk-1" || meta.Name != "vk-name" ||
		meta.OrganizationID != "org-1" || meta.OrganizationName != "Acme" ||
		meta.OrganizationTimezone != "America/Los_Angeles" ||
		meta.ProjectID != "proj-1" || meta.ProjectName != "Proj" ||
		meta.SourceApp != "my-app" || meta.OwnerID != "user-1" ||
		meta.UserDisplayName != "Alice" || meta.VKType != "application" ||
		meta.VKStatus != "active" {
		t.Errorf("meta strings mismatch: %+v", meta)
	}
	if meta.RateLimitRpm == nil || *meta.RateLimitRpm != 100 ||
		meta.CompareEndpointRateLimitRpm == nil || *meta.CompareEndpointRateLimitRpm != 60 {
		t.Errorf("meta ptrs mismatch: %+v", meta)
	}
	if len(meta.AllowedModels) != 1 || meta.AllowedModels[0].ModelID != "gpt-4o" {
		t.Errorf("allowed models: %+v", meta.AllowedModels)
	}
	// Fingerprint is deterministic & 16-hex-char (SHA256 prefix).
	if len(meta.Fingerprint) != 16 {
		t.Errorf("fingerprint len = %d, want 16", len(meta.Fingerprint))
	}
	if meta.Class != "nvk_" {
		t.Errorf("class = %q, want nvk_", meta.Class)
	}
}

func TestAuthenticate_NilPointersLeaveStringsEmpty(t *testing.T) {
	// Sanity: when VK pointer fields are nil, meta keeps the zero
	// values — the nil-check branches must NOT dereference and panic.
	vk := &store.VirtualKey{ID: "vk-bare", Name: "n", Enabled: true}
	a, _ := newAuthTestRig(t, vk, nil)
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	meta, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if meta.OrganizationID != "" || meta.ProjectID != "" || meta.SourceApp != "" ||
		meta.OwnerID != "" || meta.OrganizationName != "" ||
		meta.OrganizationTimezone != "" || meta.ProjectName != "" ||
		meta.UserDisplayName != "" || meta.VKType != "" || meta.VKStatus != "" {
		t.Errorf("nil-ptr fields should stay empty: %+v", meta)
	}
}

func TestAuthenticate_LookupReturnsNilNilTreatedAsInvalid(t *testing.T) {
	// A lookup that returns (nil, nil) without ErrNoRows — degraded
	// cache layer behaviour. The handler must still 401.
	lookup := &fakeLookupReturnsNilNil{}
	a := NewAuthenticator(lookup, mustKeyring("test-secret"), quietLogger())
	r := reqWithBearer("nvk_testkey_aaaaaaaaaaaaaa")
	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

type fakeLookupReturnsNilNil struct{}

func (f *fakeLookupReturnsNilNil) GetVirtualKeyByHash(_ context.Context, _ string) (*store.VirtualKey, error) {
	return nil, nil
}

// extractVKToken — additional coverage

func TestExtractVKToken_NilContextSafe(t *testing.T) {
	// ingressFormatFromContext must not panic on a nil-equivalent ctx —
	// callers like /v1/ai-guard handlers normally pass context.Background()
	// but the helper must also tolerate a context that carries no format
	// value. context.TODO is the canonical "context-as-placeholder".
	if got := ingressFormatFromContext(context.TODO()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractVKToken_BearerWithOnlySpaces(t *testing.T) {
	// "Bearer   " trims to "" → CutPrefix succeeds but TrimSpace empties.
	// Must fall through to format-carrier branch (or return "").
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer   ")
	if got := extractVKToken(context.Background(), r); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractVKToken_TrimsHeaderWhitespace(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-nexus-virtual-key", "  nvk_padded  ")
	if got := extractVKToken(context.Background(), r); got != "nvk_padded" {
		t.Errorf("got %q, want nvk_padded", got)
	}
}

func TestExtractVKToken_AuthorizationWithoutBearer(t *testing.T) {
	// Authorization header w/o Bearer prefix is ignored.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Basic abc123")
	if got := extractVKToken(context.Background(), r); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractVKToken_Gemini_HeaderBeatsQueryParam(t *testing.T) {
	ctx := WithIngressFormat(context.Background(), provcore.FormatGemini)
	req := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-1.5-pro:generateContent?key=nvk_query", nil)
	req.Header.Set("x-goog-api-key", "nvk_goog")
	if got := extractVKToken(ctx, req); got != "nvk_goog" {
		t.Errorf("got %q, want nvk_goog (header wins over query)", got)
	}
}

// looksLikeRealKey — boundary

func TestLooksLikeRealKey_LengthBoundary(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{strings.Repeat("a", 20), false}, // == 20, must be strictly > 20
		{strings.Repeat("a", 21), true},
		{"nvk_", true},              // prefix alone qualifies regardless of length
		{"NVK_xxxxxxxxxxxx", false}, // case-sensitive
		{"", false},
	}
	for _, tt := range tests {
		if got := looksLikeRealKey(tt.token); got != tt.want {
			t.Errorf("looksLikeRealKey(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}
