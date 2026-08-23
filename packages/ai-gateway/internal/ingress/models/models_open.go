package models

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/redis/go-redis/v9"
)

const (
	catalogCacheKey = "models:catalog:v1"
	catalogCacheTTL = 5 * time.Minute
)

// OpenModelsHandler handles GET /api/v1/open/models. It is a PUBLIC endpoint
// (no virtual key required) that returns the full enabled-model catalog in the
// enriched "catalog" shape, with limit/offset pagination. It is deliberately
// separate from the authenticated /v1/models endpoint (which keeps its OpenAI/
// Anthropic provider-parity shapes untouched): this one exists so external
// callers can browse the catalog — id, provider, pricing, context window,
// modalities, capabilities, status — without a credential.
//
// The catalog is served through a 5-minute Redis cache (see catalogEntries),
// so each request is cheap.
//
// ponytail: no per-endpoint rate limit is added here. The route is wrapped by
// the shared per-VK read limiter (pass-through for keyless callers), and the
// response is cache-served, so each hit is cheap. Add an IP/global cap only if
// abuse is observed.
func OpenModelsHandler(models ModelLookup, rdb redis.UniversalClient, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, r, http.StatusInternalServerError, "CATALOG_UNAVAILABLE", "database not available")
			return
		}
		limit, offset := parsePage(r)
		entries, err := catalogEntries(r.Context(), models, rdb, logger)
		if err != nil {
			logger.Error("list models failed", "error", err)
			writeJSONError(w, r, http.StatusInternalServerError, "CATALOG_QUERY_FAILED", "failed to list models")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(buildCatalogResponse(entries, limit, offset))
		_, _ = w.Write(b)
	}
}

// parsePage reads limit/offset query params. limit defaults to 50, is clamped
// to [1,200]; offset defaults to 0, floored at 0. Malformed values fall back
// to the defaults.
func parsePage(r *http.Request) (limit, offset int) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// catalogEntries returns the full (unpaginated) catalog entry slice, served
// through a nil-safe Redis read-through cache (key catalogCacheKey, TTL
// catalogCacheTTL). A nil client or ANY Redis error is treated as a cache miss
// and never surfaces to the caller — the slice is rebuilt from the DB.
func catalogEntries(ctx context.Context, models ModelLookup, rdb redis.UniversalClient, logger *slog.Logger) ([]catalogEntry, error) {
	if rdb != nil {
		if b, err := rdb.Get(ctx, catalogCacheKey).Bytes(); err == nil {
			var cached []catalogEntry
			if json.Unmarshal(b, &cached) == nil {
				return cached, nil
			}
		}
	}

	rows, err := models.ListEnabledModels(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]catalogEntry, 0, len(rows))
	for _, m := range rows {
		entries = append(entries, buildCatalogEntry(m))
	}

	if rdb != nil {
		if b, err := json.Marshal(entries); err == nil {
			if err := rdb.Set(ctx, catalogCacheKey, b, catalogCacheTTL).Err(); err != nil {
				logger.Warn("catalog cache set failed", "error", err)
			}
		}
	}
	return entries, nil
}

// OpenModelDetailHandler handles GET /api/v1/open/models/{model_id}. Public
// companion to OpenModelsHandler: returns ONE model's full detail — the list
// entry plus pricing_detail (cached + audio rates), capability_matrix,
// parameter_constraints (max_tokens + temperature ranges), and family.
//
// {model_id} accepts both the bare code ("gpt-4o") and the list's
// provider-prefixed id ("openai/gpt-4o"); the provider segment is stripped
// before lookup because Model.code is globally unique and carries no "/".
// Unknown or disabled models return 404 in the shared error envelope.
//
// ponytail: no per-detail cache. One unique-indexed GetModelByCode read is well
// under the 100ms budget; add a read-through cache only if profiled over it.
func OpenModelDetailHandler(models ModelLookup, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if models == nil {
			writeJSONError(w, r, http.StatusInternalServerError, "CATALOG_UNAVAILABLE", "database not available")
			return
		}
		id := r.PathValue("model_id")
		if id == "" {
			writeJSONError(w, r, http.StatusBadRequest, "MODEL_REQUIRED", "model id is required")
			return
		}
		code := id
		if _, after, ok := strings.Cut(id, "/"); ok {
			code = after // strip "provider/" prefix; Model.code is globally unique
		}
		model, err := models.GetModelByCode(r.Context(), code)
		if err != nil {
			logger.Warn("open model detail: not found", "modelId", id, "error", err)
			writeJSONError(w, r, http.StatusNotFound, "MODEL_NOT_FOUND", "model not found: "+id)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(buildCatalogDetail(*model))
		_, _ = w.Write(b)
	}
}
