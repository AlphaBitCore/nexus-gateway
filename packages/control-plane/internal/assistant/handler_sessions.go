package assistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/labstack/echo/v4"
)

// allowedModels is the explicit-mode allow-list a client may pick from: the configured
// allow-list, or just the default when none is configured. It is the STRICT enforcement
// surface used only when an operator pins NEXUS_ASSISTANT_MODELS. Auto mode (empty
// cfg.Models) bypasses this entirely in resolveModel/ListModels — there the system VK's
// own allowed-models at the AI Gateway is the enforcement point, not a static list.
func (h *Handler) allowedModels() []string {
	if len(h.cfg.Models) > 0 {
		return h.cfg.Models
	}
	if h.cfg.Model != "" {
		return []string{h.cfg.Model}
	}
	return nil
}

// resolveModel maps a client-requested model to the model used for a turn.
//   - blank request → the configured default (cfg.Model), unchanged.
//   - explicit mode (cfg.Models non-empty) → STRICT: the request must be in the
//     allow-list, else fall back to cfg.Model. The operator pinned the set on purpose.
//   - auto mode (cfg.Models empty) → pass the requested code through as-is (trimmed).
//     The system VK's own allowed-models at the AI Gateway is the REAL enforcement
//     point: an unroutable or forged code simply 400s at inference and cannot widen
//     access or escalate cost beyond what the VK already permits, so a per-turn gateway
//     round-trip just to pre-validate is unnecessary. resolveModel never calls the gateway.
func (h *Handler) resolveModel(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return h.cfg.Model
	}
	if len(h.cfg.Models) > 0 {
		// Explicit mode: the allowedModels() strict surface is the only selectable set.
		for _, m := range h.allowedModels() {
			if m == requested {
				return requested
			}
		}
		return h.cfg.Model
	}
	// Auto mode: honor the client's pick; the VK enforces routability at inference.
	return requested
}

// chatModelInfo is the per-code catalog enrichment the picker carries: the display
// label (Model.name) and the owning provider's display name (Provider.name). Both are
// best-effort — a code with no catalog row simply has none, and the picker falls back
// to the bare code as the label and "" as the provider.
type chatModelInfo struct {
	Label    string
	Provider string
}

// chatModelCatalog returns the chat-type model catalog (enabled + active) as a map of
// code → {label, provider}, and true. The map's KEYS are the chat-modality filter set
// applied on top of the VK-reachable set in ListModels auto mode (a VK may route
// embedding/image models too; the chat assistant only offers chat models); the VALUES
// enrich each offered code with its catalog label + provider for the grouped picker.
// On a nil pool or any query/scan error it returns nil + false so the caller can fall
// open — the model picker must never be wrongly emptied by a transient DB hiccup.
func (h *Handler) chatModelCatalog(ctx context.Context) (map[string]chatModelInfo, bool) {
	if h.cfg.Pool == nil {
		return nil, false
	}
	rows, err := h.cfg.Pool.Query(ctx,
		`SELECT m.code, m.name, p.name FROM "Model" m JOIN "Provider" p ON p.id = m."providerId" `+
			`WHERE m.type = 'chat' AND m.enabled = true AND m.status = 'active'`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	cat := make(map[string]chatModelInfo)
	for rows.Next() {
		var code, name, provider string
		if err := rows.Scan(&code, &name, &provider); err != nil {
			return nil, false
		}
		cat[code] = chatModelInfo{Label: name, Provider: provider}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return cat, true
}

// modelRankPrefixes is a heuristic "latest good first" preference order used ONLY to
// (a) sort the auto-mode offered list and (b) pick a robust default when the configured
// default is absent or unroutable. First matching prefix wins; anything unmatched sorts
// after all matches, alphabetically. This is a convenience ranking for "best available",
// not an authorization or routing decision — the VK still governs what actually routes.
var modelRankPrefixes = []string{
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-opus",
	"claude-sonnet-4-6",
	"claude-sonnet",
	"gpt-5.5",
	"gpt-5",
	"gemini-2.5-pro",
	"gemini-2.5",
	"o3",
	"o1",
}

// rankModel returns a sortable rank index for a model code: the index of the first
// modelRankPrefixes entry it starts with, or len(modelRankPrefixes) for "no preferred
// prefix" (those then tie-break alphabetically by the caller). Lower = more preferred.
func rankModel(code string) int {
	for i, p := range modelRankPrefixes {
		if strings.HasPrefix(code, p) {
			return i
		}
	}
	return len(modelRankPrefixes)
}

// ListModels returns the default + client-selectable inference models (login-only).
//
// Two modes:
//   - Explicit mode (NEXUS_ASSISTANT_MODELS set → cfg.Models non-empty): the offered set
//     is the configured allow-list filtered (FR-17) to the models the system VK can
//     actually reach (the AI Gateway's VK-scoped GET /v1/models), so the picker never
//     lists a model that would 400 at inference. Fails OPEN to the static list if the
//     gateway can't be queried — a transient hiccup must not empty the picker.
//   - Auto mode (cfg.Models empty): the offered set is auto-derived from the system VK's
//     real reachable models, intersected with the chat-type catalog and ranked
//     "best available first". If the gateway is down or the chat catalog is unavailable,
//     it fails OPEN to just the configured default so the picker is never wrongly emptied.
//
// The default is robust: the configured cfg.Model when it is itself offered, otherwise
// the preference-ranked best of the offered set.
func (h *Handler) ListModels(c echo.Context) error {
	var reachable []string
	if h.cfg.SystemVK != "" && h.cfg.AIGatewayURL != "" {
		// Best-effort: bound it tightly so a hung gateway never stalls this login-only
		// picker endpoint (both modes fail open below).
		mctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		cl := core.NewClient(core.Env{AIGatewayBaseURL: h.cfg.AIGatewayURL}, newBearerTokenSource(""), nil)
		if got, err := cl.GatewayModels(mctx, h.cfg.SystemVK); err == nil {
			reachable = got
		}
	}

	// The catalog enriches each offered code with its display label + provider for the
	// grouped picker, and (in auto mode) its KEYS are the chat-modality filter. It is
	// best-effort: when unavailable, codes still appear with the code as the label and
	// "" as the provider (and auto mode falls open below).
	catalog, catalogOK := h.chatModelCatalog(c.Request().Context())

	var offered []string
	if len(h.cfg.Models) > 0 {
		// Explicit mode: keep today's behavior — filter the static allow-list to the
		// reachable set, or fail open to the full static list when the gateway is down.
		if len(reachable) > 0 {
			offered = intersectModels(h.cfg.Models, reachable)
		} else {
			offered = h.cfg.Models
		}
	} else {
		// Auto mode: derive from the VK's reachable chat models, ranked best-first.
		if catalogOK && len(reachable) > 0 {
			for _, code := range reachable {
				if _, isChat := catalog[code]; isChat {
					offered = append(offered, code)
				}
			}
			sortByRank(offered)
		} else if h.cfg.Model != "" {
			// Gateway down OR chat catalog unavailable → fail open to the default so the
			// picker is never wrongly emptied.
			offered = []string{h.cfg.Model}
		}
	}

	// Enrich each offered code with its catalog label + provider. A code with no catalog
	// row (e.g. an explicit-mode model absent from the catalog) still appears, labelled by
	// its own code with an empty provider.
	enriched := make([]map[string]string, 0, len(offered))
	for _, code := range offered {
		label, provider := code, ""
		if info, ok := catalog[code]; ok {
			if info.Label != "" {
				label = info.Label
			}
			provider = info.Provider
		}
		enriched = append(enriched, map[string]string{"code": code, "label": label, "provider": provider})
	}

	def := robustDefault(h.cfg.Model, offered)
	return c.JSON(http.StatusOK, map[string]any{"default": def, "models": enriched})
}

// sortByRank orders codes by the rankModel preference (lower rank first), tie-breaking
// alphabetically. Used for the auto-mode offered list.
func sortByRank(codes []string) {
	sort.SliceStable(codes, func(i, j int) bool {
		ri, rj := rankModel(codes[i]), rankModel(codes[j])
		if ri != rj {
			return ri < rj
		}
		return codes[i] < codes[j]
	})
}

// robustDefault picks the default model for the picker: the configured default when it
// is itself offered; otherwise the preference-ranked best of the offered set; or, when
// nothing is offered, the configured default verbatim (whatever was configured).
func robustDefault(configured string, offered []string) string {
	for _, m := range offered {
		if m == configured && configured != "" {
			return configured
		}
	}
	if len(offered) == 0 {
		return configured
	}
	best := offered[0]
	bestRank := rankModel(best)
	for _, m := range offered[1:] {
		if r := rankModel(m); r < bestRank || (r == bestRank && m < best) {
			best, bestRank = m, r
		}
	}
	return best
}

// intersectModels returns the elements of want that also appear in have, preserving
// want's order (the operator's configured ordering). Used to drop configured-but-
// unreachable models from the assistant's model picker (FR-17).
//
// INVARIANT: both sides must use the SAME model identifier — the model *code* (e.g.
// "claude-sonnet-4-6"). `NEXUS_ASSISTANT_MODELS` carries codes, the AI Gateway's
// /v1/models returns `id == m.Code`, and inference resolves strictly by code
// (GetModelByCode). If /v1/models ever emitted a different id (alias / UUID), this
// intersection would silently empty every picker WITHOUT tripping the fail-open path
// (the gateway call would still succeed). Keep the id spaces identical.
func intersectModels(want, have []string) []string {
	set := make(map[string]struct{}, len(have))
	for _, m := range have {
		set[m] = struct{}{}
	}
	out := make([]string, 0, len(want))
	for _, m := range want {
		if _, ok := set[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// callerDBStore builds a per-caller, userId-bound dbStore for the CRUD endpoints,
// or reports false when persistence is unavailable (no pool) or the principal has
// no userId (non-bearer). Spill may be nil — List does not need it and Delete
// tolerates it. The userId is always the authenticated principal, never client
// input (I3).
func (h *Handler) callerDBStore(c echo.Context) (*dbStore, bool) {
	if h.cfg.Pool == nil {
		return nil, false
	}
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.KeyID == "" {
		return nil, false
	}
	return newDBStore(c.Request().Context(), h.cfg.Pool, h.cfg.Spill, aa.KeyID), true
}

// ListSessions returns the caller's own conversation sessions (metadata only),
// newest first. Login-only + userId-scoped — no new IAM action (I1/I3).
func (h *Handler) ListSessions(c echo.Context) error {
	store, ok := h.callerDBStore(c)
	if !ok {
		return c.JSON(http.StatusOK, map[string]any{"sessions": []any{}})
	}
	metas, err := store.List()
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, "list_failed", "could not list sessions")
	}
	out := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		out = append(out, map[string]any{"id": m.ID, "title": m.Title, "updatedAt": m.Updated})
	}
	return c.JSON(http.StatusOK, map[string]any{"sessions": out})
}

// DeleteSession removes one of the caller's sessions (row + spilled transcript).
// A missing / non-owned id is a 404 (the userId scope makes them indistinguishable).
func (h *Handler) DeleteSession(c echo.Context) error {
	id := c.Param("id")
	if strings.TrimSpace(id) == "" {
		return errJSON(c, http.StatusBadRequest, "validation_error", "session id is required")
	}
	store, ok := h.callerDBStore(c)
	if !ok {
		return errJSON(c, http.StatusServiceUnavailable, "unavailable", "session persistence is not configured")
	}
	if err := store.Delete(id); err != nil {
		return errJSON(c, http.StatusNotFound, "not_found", "session not found")
	}
	// Release any live bus entry (cancels an in-flight turn for this session + drops
	// its replay ring) so a deleted session leaves no detached turn running.
	if aa := middleware.AdminAuthFromContext(c); aa != nil && aa.KeyID != "" {
		h.bus.drop(aa.KeyID + ":" + id)
	}
	return c.NoContent(http.StatusNoContent)
}

// DownloadFile streams one of the caller's sandbox files. CP-proxied with an owner
// re-check (WHERE "userId") — no transferable pre-signed URL is ever exposed (R8).
func (h *Handler) DownloadFile(c echo.Context) error {
	id := c.Param("id")
	if strings.TrimSpace(id) == "" {
		return errJSON(c, http.StatusBadRequest, "validation_error", "file id is required")
	}
	if h.cfg.Pool == nil || h.cfg.Spill == nil {
		return errJSON(c, http.StatusServiceUnavailable, "unavailable", "the file sandbox is not configured")
	}
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.KeyID == "" {
		return errJSON(c, http.StatusUnprocessableEntity, "unsupported_auth", "an interactive admin session is required")
	}
	fs := newWebFileStore(c.Request().Context(), h.cfg.Pool, h.cfg.Spill, aa.KeyID, "")
	rc, m, err := fs.Get(id)
	if errors.Is(err, errFileExpired) {
		return errJSON(c, http.StatusGone, "expired", "this file has expired and is no longer available")
	}
	if err != nil {
		return errJSON(c, http.StatusNotFound, "not_found", "file not found")
	}
	defer func() { _ = rc.Close() }()
	c.Response().Header().Set("Content-Length", strconv.Itoa(m.Size))
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", m.Name))
	return c.Stream(http.StatusOK, m.ContentType, rc)
}

// GetSession returns one of the caller's conversations as a flat role+text
// transcript for re-rendering in the widget. userId-scoped (a non-owned id is 404);
// the transcript content is fetched from spill via the DB ref.
func (h *Handler) GetSession(c echo.Context) error {
	id := c.Param("id")
	if strings.TrimSpace(id) == "" {
		return errJSON(c, http.StatusBadRequest, "validation_error", "session id is required")
	}
	store, ok := h.callerDBStore(c)
	if !ok {
		return errJSON(c, http.StatusServiceUnavailable, "unavailable", "session persistence is not configured")
	}
	sess, err := store.Load(id)
	if err != nil {
		return errJSON(c, http.StatusNotFound, "not_found", "session not found")
	}
	msgs := make([]map[string]string, 0, len(sess.Messages))
	for _, m := range sess.Messages {
		txt := strings.TrimSpace(m.Text())
		if txt == "" {
			continue // tool-only / empty turns are not rendered
		}
		role := "assistant"
		if m.Role == agent.RoleUser {
			role = "user"
		}
		msgs = append(msgs, map[string]string{"role": role, "text": txt})
	}
	return c.JSON(http.StatusOK, map[string]any{"id": sess.ID, "messages": msgs})
}
