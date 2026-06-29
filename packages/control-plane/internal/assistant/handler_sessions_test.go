package assistant

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// ctxWithUser builds an echo context carrying an authenticated admin principal, the
// way AdminAuth middleware would. userID == "" leaves the context anonymous.
func ctxWithUser(e *echo.Echo, method, target, userID string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if userID != "" {
		c.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	}
	return c, rec
}

func TestListSessions(t *testing.T) {
	e := echo.New()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	h := New(Config{Pool: mock, Spill: &fakeSpill{}})

	// No userId (non-bearer principal) → empty list, not an error.
	c, rec := ctxWithUser(e, http.MethodGet, "/s", "")
	if err := h.ListSessions(c); err != nil || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sessions") {
		t.Fatalf("anon list: err=%v code=%d body=%s", err, rec.Code, rec.Body.String())
	}

	// With a userId → query scoped to that user.
	mock.ExpectQuery(`SELECT id, title, "updatedAt" FROM "AssistantSession" WHERE "userId" = \$1`).
		WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "updatedAt"}).AddRow("s1", "hi", time.Now()))
	c2, rec2 := ctxWithUser(e, http.MethodGet, "/s", "alice")
	if err := h.ListSessions(c2); err != nil || rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "s1") {
		t.Fatalf("list: err=%v code=%d body=%s", err, rec2.Code, rec2.Body.String())
	}

	// List failure → 500.
	mock.ExpectQuery(`SELECT id, title`).WithArgs("alice").WillReturnError(errors.New("boom"))
	c3, rec3 := ctxWithUser(e, http.MethodGet, "/s", "alice")
	_ = h.ListSessions(c3)
	if rec3.Code != http.StatusInternalServerError {
		t.Fatalf("list error code=%d", rec3.Code)
	}
}

func TestListSessionsNoPool(t *testing.T) {
	e := echo.New()
	h := New(Config{}) // no pool → persistence unavailable
	c, rec := ctxWithUser(e, http.MethodGet, "/s", "alice")
	if err := h.ListSessions(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("no-pool list: err=%v code=%d", err, rec.Code)
	}
}

func TestDeleteSession(t *testing.T) {
	e := echo.New()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	h := New(Config{Pool: mock, Spill: &fakeSpill{}})

	// Missing id → 400.
	c, rec := ctxWithUser(e, http.MethodDelete, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("   ")
	_ = h.DeleteSession(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank id code=%d", rec.Code)
	}

	// Success → 204 (the deletion also lands a tombstone on the audit chain).
	mock.ExpectQuery(`DELETE FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2 RETURNING "spillRef"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow([]byte("null")))
	expectChatChainAppend(mock, "s1", "alice")
	c2, rec2 := ctxWithUser(e, http.MethodDelete, "/s", "alice")
	c2.SetParamNames("id")
	c2.SetParamValues("s1")
	if err := h.DeleteSession(c2); err != nil || rec2.Code != http.StatusNoContent {
		t.Fatalf("delete: err=%v code=%d", err, rec2.Code)
	}

	// Missing / non-owned id → 404.
	mock.ExpectQuery(`DELETE FROM "AssistantSession"`).WithArgs("s2", "alice").WillReturnError(pgx.ErrNoRows)
	c3, rec3 := ctxWithUser(e, http.MethodDelete, "/s", "alice")
	c3.SetParamNames("id")
	c3.SetParamValues("s2")
	_ = h.DeleteSession(c3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("delete missing code=%d", rec3.Code)
	}
}

// TestChatStreamMultiTurnContinuesOwnedSession proves multi-turn reuse: when the
// client passes a sessionId it owns, ChatStream Loads the prior transcript (ref from
// DB, content from spill) and continues THAT session — the done event carries the
// same id back, and the turn persists via Save.
func TestChatStreamMultiTurnContinuesOwnedSession(t *testing.T) {
	mockGW := mockUpstream(t)
	defer mockGW.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.MatchExpectationsInOrder(false)

	prior, _ := json.Marshal([]agent.Message{agent.TextMessage(agent.RoleUser, "earlier question")})
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": prior}}
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})

	h := New(Config{AIGatewayURL: mockGW.URL, CPBaseURL: mockGW.URL, SystemVK: "nvk_test", Model: "m", Pool: mock, Spill: spill})

	// The three DB ops a turn issues with DB stores: memory index (system prompt),
	// session load (this test's continuation), session save (post-turn).
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 ORDER BY name`).
		WithArgs("alice").WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}))
	mock.ExpectQuery(`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("s1", "alice").WillReturnRows(pgxmock.NewRows([]string{"spillRef", "createdAt", "updatedAt"}).AddRow(ref, rowT, rowT))
	expectChatChainAppend(mock, "s1", "alice")
	mock.ExpectExec(`INSERT INTO "AssistantSession"`).
		WithArgs("s1", "alice", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// The session id is now a path param; continuing an owned id Loads its transcript.
	_, out := driveTurnSID(t, h, "alice", "s1", `{"message":"follow up"}`)
	if !strings.Contains(out, `"sessionId":"s1"`) {
		t.Fatalf("done must carry the continued session id s1, got:\n%s", out)
	}
	if !strings.Contains(out, "All healthy.") {
		t.Fatalf("expected the streamed reply, got:\n%s", out)
	}
}

// TestStartChatSessionIDIsUserScoped is the isolation assertion: the
// client-supplied session id is resolved ONLY within the caller's own userId
// namespace, so two different users picking the SAME id ("shared-id") can never
// reach each other's session. We drive a turn as "bob" continuing id "shared-id"
// and assert every DB op is scoped WHERE "userId" = 'bob' — a query bound to any
// other user (e.g. a prior "alice" session with the same id) would fail the
// pgxmock arg match. This is why a server-generated UUID is unnecessary for
// safety: collision across users is structurally impossible.
func TestStartChatSessionIDIsUserScoped(t *testing.T) {
	mockGW := mockUpstream(t)
	defer mockGW.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.MatchExpectationsInOrder(false)

	prior, _ := json.Marshal([]agent.Message{agent.TextMessage(agent.RoleUser, "bob's earlier question")})
	spill := &fakeSpill{objs: map[string][]byte{"shared-id:transcript": prior}}
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "shared-id:transcript"})

	h := New(Config{AIGatewayURL: mockGW.URL, CPBaseURL: mockGW.URL, SystemVK: "nvk_test", Model: "m", Pool: mock, Spill: spill})

	// All three turn DB ops MUST bind userId='bob'. The session load+save bind
	// (id='shared-id', userId='bob') — never 'alice', proving cross-user
	// isolation despite the identical client-supplied id.
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 ORDER BY name`).
		WithArgs("bob").WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}))
	mock.ExpectQuery(`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("shared-id", "bob").WillReturnRows(pgxmock.NewRows([]string{"spillRef", "createdAt", "updatedAt"}).AddRow(ref, rowT, rowT))
	expectChatChainAppend(mock, "shared-id", "bob")
	mock.ExpectExec(`INSERT INTO "AssistantSession"`).
		WithArgs("shared-id", "bob", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	_, out := driveTurnSID(t, h, "bob", "shared-id", `{"message":"is it mine?"}`)
	if !strings.Contains(out, `"sessionId":"shared-id"`) {
		t.Fatalf("done must echo the user-scoped session id, got:\n%s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB ops were not all userId='bob'-scoped (cross-user isolation broken): %v", err)
	}
}

func TestModelAllowList(t *testing.T) {
	h := New(Config{Model: "default-m", Models: []string{"default-m", "big-m"}})
	if h.resolveModel("big-m") != "big-m" {
		t.Fatal("an allow-listed model must be honored")
	}
	if h.resolveModel("evil-m") != "default-m" {
		t.Fatal("a non-allow-listed model must fall back to the default (client cannot inject)")
	}
	if h.resolveModel("") != "default-m" {
		t.Fatal("a blank request must use the default")
	}
	if h.resolveModel("  big-m  ") != "big-m" {
		t.Fatal("a request is trimmed before the allow-list match")
	}

	// allowedModels collapses to just the default when no allow-list is configured (the
	// strict-mode surface). Auto-mode resolveModel pass-through is covered separately by
	// TestResolveModelAutoMode — it deliberately bypasses allowedModels.
	h2 := New(Config{Model: "only-m"})
	if len(h2.allowedModels()) != 1 || h2.allowedModels()[0] != "only-m" {
		t.Fatal("with no allow-list, allowedModels collapses to the default")
	}

	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("ListModels: %v %d", err, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "big-m") || !strings.Contains(rec.Body.String(), "default-m") {
		t.Fatalf("models not listed: %s", rec.Body.String())
	}
}

func TestIntersectModels(t *testing.T) {
	got := intersectModels([]string{"a", "b", "c"}, []string{"c", "a", "x"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("intersectModels = %v, want [a c] preserving want-order", got)
	}
	if n := len(intersectModels([]string{"a"}, []string{"z"})); n != 0 {
		t.Fatalf("no overlap must yield empty, got %d", n)
	}
}

// TestListModels_FiltersToReachable: a configured model the system VK
// cannot reach (per the gateway's VK-scoped /v1/models) is dropped from the picker.
func TestListModels_FiltersToReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m-a"},{"id":"m-c"}]}`))
	}))
	defer srv.Close()
	h := New(Config{Model: "m-a", Models: []string{"m-a", "m-b", "m-c"}, SystemVK: "nvk_sys", AIGatewayURL: srv.URL})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("ListModels: %v %d", err, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "m-a") || !strings.Contains(body, "m-c") {
		t.Fatalf("reachable models must remain: %s", body)
	}
	if strings.Contains(body, "m-b") {
		t.Fatalf("unreachable m-b must be dropped from the picker: %s", body)
	}
}

// TestListModels_FailsOpenOnGatewayError: a gateway error must NOT empty the picker —
// it falls back to the static configured list.
func TestListModels_FailsOpenOnGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	h := New(Config{Model: "m-a", Models: []string{"m-a", "m-b"}, SystemVK: "nvk_sys", AIGatewayURL: srv.URL})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "m-a") || !strings.Contains(body, "m-b") {
		t.Fatalf("on gateway error the static list must be kept (fail open): %s", body)
	}
}

// TestListModels_ExplicitMode_EnrichesAndFallsBackForUnknownCode: explicit mode offers the
// configured allow-list (gateway down → static list). Each offered code is enriched from the
// catalog join; a configured code that has NO catalog row still appears, labelled by its own
// code with an empty provider (never dropped).
func TestListModels_ExplicitMode_EnrichesAndFallsBackForUnknownCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // gateway down → fail open to the static list
	}))
	defer srv.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Catalog knows "known-m" but NOT "ghost-m" (a configured code absent from the catalog).
	mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).
		WillReturnRows(chatCatalogRows([3]string{"known-m", "Known Model", "OpenAI"}))
	h := New(Config{Model: "known-m", Models: []string{"known-m", "ghost-m"}, SystemVK: "nvk_sys", AIGatewayURL: srv.URL, Pool: mock})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("ListModels: %v %d", err, rec.Code)
	}
	var resp struct {
		Default string         `json:"default"`
		Models  []offeredModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Models) != 2 {
		t.Fatalf("both configured codes must be offered, got %+v", resp.Models)
	}
	if resp.Models[0].Code != "known-m" || resp.Models[0].Label != "Known Model" || resp.Models[0].Provider != "OpenAI" {
		t.Fatalf("catalog-known code must be enriched, got %+v", resp.Models[0])
	}
	// ghost-m has no catalog row → label falls back to the code, provider is empty.
	if resp.Models[1].Code != "ghost-m" || resp.Models[1].Label != "ghost-m" || resp.Models[1].Provider != "" {
		t.Fatalf("catalog-unknown code must fall back to code-as-label, empty provider, got %+v", resp.Models[1])
	}
}

// TestResolveModelAutoMode: in auto mode (no NEXUS_ASSISTANT_MODELS) the client's pick
// passes through verbatim — the system VK enforces routability at inference, not a static
// list. A blank request still uses the configured default.
func TestResolveModelAutoMode(t *testing.T) {
	h := New(Config{Model: "claude-opus-4-7"})
	if got := h.resolveModel("gpt-5"); got != "gpt-5" {
		t.Fatalf("auto mode must pass the requested code through, got %q", got)
	}
	if got := h.resolveModel("  gemini-2.5-pro  "); got != "gemini-2.5-pro" {
		t.Fatalf("auto mode trims then passes through, got %q", got)
	}
	if got := h.resolveModel("not-in-any-list"); got != "not-in-any-list" {
		t.Fatalf("auto mode does NOT restrict to a list, got %q", got)
	}
	if got := h.resolveModel(""); got != "claude-opus-4-7" {
		t.Fatalf("blank must use the configured default, got %q", got)
	}
}

// TestResolveModelExplicitModeRejects: in explicit mode an out-of-list pick is rejected
// to the configured default (the operator pinned the set on purpose).
func TestResolveModelExplicitModeRejects(t *testing.T) {
	h := New(Config{Model: "m1", Models: []string{"m1", "m2"}})
	if got := h.resolveModel("m2"); got != "m2" {
		t.Fatalf("an allow-listed model must be honored, got %q", got)
	}
	if got := h.resolveModel("evil"); got != "m1" {
		t.Fatalf("an out-of-list pick must fall back to the default, got %q", got)
	}
}

// chatCatalogRows builds a 3-column (code, name, provider) row set matching the
// Model⋈Provider join chatModelCatalog issues.
func chatCatalogRows(triples ...[3]string) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"code", "name", "provider"})
	for _, tr := range triples {
		rows.AddRow(tr[0], tr[1], tr[2])
	}
	return rows
}

// TestChatModelCatalog covers the catalog join query: rows → code→{label,provider} map;
// query error → false; scan error → false; nil pool → false; rows.Err → false.
func TestChatModelCatalog(t *testing.T) {
	t.Run("rows to map with label+provider", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model" m JOIN "Provider" p`).
			WillReturnRows(chatCatalogRows(
				[3]string{"claude-opus-4-7", "Claude Opus 4.7", "Anthropic"},
				[3]string{"gpt-5", "GPT-5", "OpenAI"}))
		h := New(Config{Pool: mock})
		cat, ok := h.chatModelCatalog(context.Background())
		if !ok {
			t.Fatal("expected ok=true on a clean query")
		}
		if cat["claude-opus-4-7"].Label != "Claude Opus 4.7" || cat["claude-opus-4-7"].Provider != "Anthropic" {
			t.Fatalf("opus enrichment wrong: %+v", cat["claude-opus-4-7"])
		}
		if cat["gpt-5"].Label != "GPT-5" || cat["gpt-5"].Provider != "OpenAI" {
			t.Fatalf("gpt-5 enrichment wrong: %+v", cat["gpt-5"])
		}
		if len(cat) != 2 {
			t.Fatalf("catalog size = %d, want 2", len(cat))
		}
	})

	t.Run("query error to false", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).WillReturnError(errors.New("db down"))
		h := New(Config{Pool: mock})
		if cat, ok := h.chatModelCatalog(context.Background()); ok || cat != nil {
			t.Fatalf("query error must fail open (nil,false), got (%v,%v)", cat, ok)
		}
	})

	t.Run("scan error to false", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// A row whose column count mismatches the three-column Scan → scan error.
		mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).
			WillReturnRows(pgxmock.NewRows([]string{"code", "name"}).AddRow("gpt-5", "GPT-5"))
		h := New(Config{Pool: mock})
		if cat, ok := h.chatModelCatalog(context.Background()); ok || cat != nil {
			t.Fatalf("scan error must fail open (nil,false), got (%v,%v)", cat, ok)
		}
	})

	t.Run("nil pool to false", func(t *testing.T) {
		h := New(Config{})
		if cat, ok := h.chatModelCatalog(context.Background()); ok || cat != nil {
			t.Fatalf("nil pool must be (nil,false), got (%v,%v)", cat, ok)
		}
	})

	t.Run("rows iteration error to false", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		// Row 0 scans clean; CloseError surfaces via rows.Err() after the loop ends — the
		// post-iteration failure path, distinct from a per-row Scan failure.
		mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).
			WillReturnRows(chatCatalogRows([3]string{"gpt-5", "GPT-5", "OpenAI"}).CloseError(errors.New("conn reset")))
		h := New(Config{Pool: mock})
		if cat, ok := h.chatModelCatalog(context.Background()); ok || cat != nil {
			t.Fatalf("rows.Err must fail open (nil,false), got (%v,%v)", cat, ok)
		}
	})
}

// TestAllowedModelsEmpty: with neither an allow-list nor a default, allowedModels is nil
// (the strict surface has nothing to offer; auto mode does not use it).
func TestAllowedModelsEmpty(t *testing.T) {
	h := New(Config{})
	if got := h.allowedModels(); got != nil {
		t.Fatalf("allowedModels with no config must be nil, got %v", got)
	}
}

// TestSortByRankAlphabeticalTieBreak: two codes with the SAME rank (both unmatched by any
// preference prefix) must tie-break alphabetically.
func TestSortByRankAlphabeticalTieBreak(t *testing.T) {
	got := []string{"mistral-large", "command-r"}
	sortByRank(got)
	if got[0] != "command-r" || got[1] != "mistral-large" {
		t.Fatalf("equal-rank codes must tie-break alphabetically, got %v", got)
	}
}

func TestRankModelAndRobustDefault(t *testing.T) {
	// rankModel: prefix-matched codes rank by first-match order; unmatched land last.
	if rankModel("claude-opus-4-7-20990101") >= rankModel("gpt-5") {
		t.Fatal("claude-opus-4-7 must outrank gpt-5")
	}
	if rankModel("something-unknown") != len(modelRankPrefixes) {
		t.Fatal("an unmatched code must rank last")
	}
	if rankModel("claude-opus-4-6") <= rankModel("claude-opus-4-7") {
		t.Fatal("opus-4-7 must be preferred over opus-4-6")
	}

	// robustDefault: configured default wins when offered.
	if got := robustDefault("gpt-5", []string{"claude-opus-4-7", "gpt-5"}); got != "gpt-5" {
		t.Fatalf("configured default must win when offered, got %q", got)
	}
	// configured default absent → preference-ranked best of offered.
	if got := robustDefault("absent", []string{"gpt-5", "claude-opus-4-7"}); got != "claude-opus-4-7" {
		t.Fatalf("absent default → ranked best, got %q", got)
	}
	// empty offered → configured verbatim.
	if got := robustDefault("only-m", nil); got != "only-m" {
		t.Fatalf("empty offered → configured default, got %q", got)
	}
	// no preferred prefix among offered → alphabetical first.
	if got := robustDefault("absent", []string{"zeta", "alpha"}); got != "alpha" {
		t.Fatalf("no-preference offered → alphabetical first, got %q", got)
	}
}

// TestListModels_AutoMode_DerivesChatReachableRankedDefault: with no NEXUS_ASSISTANT_MODELS,
// the offered set is the VK-reachable ∩ chat catalog, sorted best-first, and the default is
// the preference-ranked best when the configured default is not itself reachable.
func TestListModels_AutoMode_DerivesChatReachableRankedDefault(t *testing.T) {
	// Gateway reports the VK can reach these (chat + an embedding + an unknown), in an
	// arbitrary order. Only chat-type, catalog-active codes should survive, ranked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5"},{"id":"text-embedding-3"},{"id":"claude-opus-4-7"},{"id":"gemini-2.5-pro"}]}`))
	}))
	defer srv.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Chat catalog: gpt-5, claude-opus-4-7, gemini-2.5-pro are chat; the embedding is NOT
	// returned by this query (type='chat' filter), so it is excluded from offered. The
	// join also carries each code's display label + provider for the grouped picker.
	mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model" m JOIN "Provider" p`).
		WillReturnRows(chatCatalogRows(
			[3]string{"gpt-5", "GPT-5", "OpenAI"},
			[3]string{"claude-opus-4-7", "Claude Opus 4.7", "Anthropic"},
			[3]string{"gemini-2.5-pro", "Gemini 2.5 Pro", "Google"}))

	// Configured default "claude-opus-4-9" is NOT reachable → robust default must pick the
	// ranked best of offered (claude-opus-4-7).
	h := New(Config{Model: "claude-opus-4-9", SystemVK: "nvk_sys", AIGatewayURL: srv.URL, Pool: mock})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("ListModels: %v %d", err, rec.Code)
	}

	var resp struct {
		Default string         `json:"default"`
		Models  []offeredModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	type want struct{ code, label, provider string }
	wants := []want{
		{"claude-opus-4-7", "Claude Opus 4.7", "Anthropic"},
		{"gpt-5", "GPT-5", "OpenAI"},
		{"gemini-2.5-pro", "Gemini 2.5 Pro", "Google"},
	}
	if len(resp.Models) != len(wants) {
		t.Fatalf("models = %v, want %v (chat∩reachable, ranked)", resp.Models, wants)
	}
	for i, w := range wants {
		got := resp.Models[i]
		if got.Code != w.code || got.Label != w.label || got.Provider != w.provider {
			t.Fatalf("models[%d] = %+v, want %+v (label/provider must come from the join)", i, got, w)
		}
	}
	for _, m := range resp.Models {
		if m.Code == "text-embedding-3" {
			t.Fatalf("non-chat embedding must be excluded, got %v", resp.Models)
		}
	}
	if resp.Default != "claude-opus-4-7" {
		t.Fatalf("default = %q, want claude-opus-4-7 (configured default unroutable → ranked best)", resp.Default)
	}
}

// offeredModel is the per-model object shape ListModels now returns: a bare code plus
// the catalog-derived display label and owning provider name.
type offeredModel struct {
	Code     string `json:"code"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
}

// TestListModels_AutoMode_DefaultReachableKept: when the configured default IS reachable,
// it stays the default even if a higher-ranked model is also offered.
func TestListModels_AutoMode_DefaultReachableKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-opus-4-7"},{"id":"gpt-5"}]}`))
	}))
	defer srv.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).
		WillReturnRows(chatCatalogRows(
			[3]string{"claude-opus-4-7", "Claude Opus 4.7", "Anthropic"},
			[3]string{"gpt-5", "GPT-5", "OpenAI"}))
	h := New(Config{Model: "gpt-5", SystemVK: "nvk_sys", AIGatewayURL: srv.URL, Pool: mock})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var resp struct {
		Default string `json:"default"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Default != "gpt-5" {
		t.Fatalf("a reachable configured default must be kept, got %q", resp.Default)
	}
}

// TestListModels_AutoMode_FailsOpenWhenGatewayDown: in auto mode, a gateway error must NOT
// empty the picker — it falls open to just the configured default.
func TestListModels_AutoMode_FailsOpenWhenGatewayDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// The chat catalog is always queried (it also carries label/provider); the gateway-down
	// branch fails open regardless of whether reachable is empty. The catalog returns a clean
	// row here, proving the fail-open is driven by the empty reachable set, not a DB error.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).
		WillReturnRows(chatCatalogRows([3]string{"claude-opus-4-7", "Claude Opus 4.7", "Anthropic"}))
	h := New(Config{Model: "claude-opus-4-7", SystemVK: "nvk_sys", AIGatewayURL: srv.URL, Pool: mock})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var resp struct {
		Default string         `json:"default"`
		Models  []offeredModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Gateway down → fail open to the configured default. The default code IS in the catalog,
	// so it is still enriched with its label + provider.
	if len(resp.Models) != 1 || resp.Models[0].Code != "claude-opus-4-7" || resp.Default != "claude-opus-4-7" {
		t.Fatalf("gateway-down auto mode must fail open to the default, got models=%+v default=%q", resp.Models, resp.Default)
	}
	if resp.Models[0].Label != "Claude Opus 4.7" || resp.Models[0].Provider != "Anthropic" {
		t.Fatalf("fail-open default must still be enriched from the catalog, got %+v", resp.Models[0])
	}
}

// TestListModels_AutoMode_FailsOpenWhenChatCatalogUnavailable: gateway is reachable but the
// chat catalog query errors → fail open to the configured default (never empty the picker).
func TestListModels_AutoMode_FailsOpenWhenChatCatalogUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5"}]}`))
	}))
	defer srv.Close()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT m.code, m.name, p.name FROM "Model"`).WillReturnError(errors.New("catalog down"))
	h := New(Config{Model: "gpt-5", SystemVK: "nvk_sys", AIGatewayURL: srv.URL, Pool: mock})
	e := echo.New()
	c, rec := ctxWithUser(e, http.MethodGet, "/m", "alice")
	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var resp struct {
		Default string         `json:"default"`
		Models  []offeredModel `json:"models"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// Catalog unavailable → fail open to the bare default with the code as its own label
	// (no provider, since the join could not run).
	if len(resp.Models) != 1 || resp.Models[0].Code != "gpt-5" || resp.Models[0].Label != "gpt-5" || resp.Models[0].Provider != "" {
		t.Fatalf("chat-catalog-down must fail open to the default code-as-label, got %+v", resp.Models)
	}
}

func TestGetSession(t *testing.T) {
	e := echo.New()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	prior, _ := json.Marshal([]agent.Message{
		agent.TextMessage(agent.RoleUser, "is it healthy?"),
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "All healthy."}}},
	})
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": prior}}
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	h := New(Config{Pool: mock, Spill: spill})

	mock.ExpectQuery(`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef", "createdAt", "updatedAt"}).AddRow(ref, rowT, rowT))
	c, rec := ctxWithUser(e, http.MethodGet, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("s1")
	if err := h.GetSession(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GetSession: err=%v code=%d", err, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "is it healthy?") || !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("transcript not rendered: %s", body)
	}
	// A non-owned / missing id → 404.
	mock.ExpectQuery(`SELECT "spillRef"`).WithArgs("s2", "alice").WillReturnError(pgx.ErrNoRows)
	c2, rec2 := ctxWithUser(e, http.MethodGet, "/s", "alice")
	c2.SetParamNames("id")
	c2.SetParamValues("s2")
	_ = h.GetSession(c2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("missing session code=%d", rec2.Code)
	}

	// No pool → 503.
	c3, rec3 := ctxWithUser(echo.New(), http.MethodGet, "/s", "alice")
	c3.SetParamNames("id")
	c3.SetParamValues("s1")
	_ = New(Config{}).GetSession(c3)
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-pool get code=%d", rec3.Code)
	}
}

func TestDeleteSessionNoPool(t *testing.T) {
	e := echo.New()
	h := New(Config{}) // no pool
	c, rec := ctxWithUser(e, http.MethodDelete, "/s", "alice")
	c.SetParamNames("id")
	c.SetParamValues("s1")
	_ = h.DeleteSession(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-pool delete code=%d", rec.Code)
	}
}

// TestGetSessionCarriesWorkflowArtifactIDs pins the card-survival contract:
// runs and freeze reviews stamped on the assistant message at turn
// end ride the reload payload, so the web re-mounts their cards without
// scraping prose — including a card-only turn whose text is empty.
// TestTurnArtifacts_NoteRunDedupes: OnRunStarted notes the run id the moment
// it exists and lift() notes it again at tool end — the persisted stamp must
// carry it once.
// TestLiftFileArtifact: a successful write_file surfaces a `file` event with
// the download path; an errored call and other tools surface nothing.
func TestLiftFileArtifact(t *testing.T) {
	events := map[string]int{}
	pub := func(ev string, _ any) { events[ev]++ }
	liftFileArtifact("write_file", []byte("Saved file f-1 ("+assistantFilesPath+"f-1)"), false, pub)
	liftFileArtifact("write_file", []byte("Saved file f-2"), true, pub)
	liftFileArtifact("observe_health", []byte("ok"), false, pub)
	if events["file"] != 1 {
		t.Fatalf("events = %v, want exactly one file event", events)
	}
}
