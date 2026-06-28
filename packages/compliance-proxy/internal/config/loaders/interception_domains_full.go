// Package loaders — full InterceptionDomain + InterceptionPath
// loader. Returns the rich runtime view consumed by domain.Engine.
package loaders

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

// InterceptionDomainRow mirrors the column-by-column shape produced by
// LoadInterceptionDomainsFull's domain query. Exposed so the row-decode
// pipeline (decodeInterceptionDomainRow) can be unit-tested without a
// live *sql.DB. The enum columns arrive as raw strings; HostMatchType /
// NetworkZone / DefaultPathAction / OnAdapterError are upper-cased at
// decode time to match the domainpolicy enum literals.
type InterceptionDomainRow struct {
	ID                      string
	Name                    string
	HostPattern             string
	HostMatch               string
	AdapterID               string
	Zone                    string
	DefaultPathAction       string
	OnAdapterError          string
	Enabled                 bool
	Priority                int
	UpdatedAt               time.Time
	StreamingMode           *string
	StreamingChunkBytes     *int
	StreamingHookTimeoutMs  *int
	StreamingMaxBufferBytes *int
	StreamingFailBehavior   *string
	CaptureRequestBody      *bool
	CaptureResponseBody     *bool
	RawBodySpillEnabled     *bool
}

// InterceptionPathRow mirrors the path query output. PatternsJSON arrives
// as a JSON-encoded text[] because the loader's SELECT casts the column
// via to_jsonb so we do not have to pull in lib/pq for array decoding.
type InterceptionPathRow struct {
	ID           string
	DomainID     string
	PatternsJSON string
	MatchType    string
	Action       string
}

// LoadInterceptionDomainsFull reads enabled `interception_domain` rows
// joined with their `interception_path` children. Domains come back
// ordered by priority DESC, created_at ASC so the engine's iteration
// order is deterministic and respects priority.
//
// text[] columns (interception_path.path_pattern) are converted to JSON
// in SQL via to_jsonb to keep the loader pgx-driver-friendly without
// pulling in lib/pq for array decoding.
//
// Decoding + path attachment live in decodeInterceptionDomainRow /
// attachInterceptionPaths so the interesting logic (enum upper-casing,
// malformed-pattern JSON surfaces, orphan path rows silently dropped)
// is unit-tested without a live database.
func LoadInterceptionDomainsFull(ctx context.Context, db *sql.DB) ([]domain.InterceptionDomain, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, host_pattern, host_match_type::text, adapter_id,
		       COALESCE(network_zone::text, 'PUBLIC'),
		       COALESCE(default_path_action::text, 'PROCESS'),
		       COALESCE(on_adapter_error::text, 'FAIL_OPEN'),
		       enabled, priority, updated_at,
		       streaming_mode, streaming_chunk_bytes, streaming_hook_timeout_ms,
		       streaming_max_buffer_bytes, streaming_fail_behavior,
		       capture_request_body, capture_response_body, raw_body_spill_enabled
		FROM "interception_domain"
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("load interception domains: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var scanned []InterceptionDomainRow
	for rows.Next() {
		var r InterceptionDomainRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.HostPattern, &r.HostMatch, &r.AdapterID,
			&r.Zone, &r.DefaultPathAction, &r.OnAdapterError,
			&r.Enabled, &r.Priority, &r.UpdatedAt,
			&r.StreamingMode, &r.StreamingChunkBytes, &r.StreamingHookTimeoutMs,
			&r.StreamingMaxBufferBytes, &r.StreamingFailBehavior,
			&r.CaptureRequestBody, &r.CaptureResponseBody, &r.RawBodySpillEnabled,
		); err != nil {
			return nil, fmt.Errorf("scan interception domain: %w", err)
		}
		scanned = append(scanned, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interception domains: %w", err)
	}

	out, domainsByID := decodeInterceptionDomainRows(scanned)
	if len(out) == 0 {
		return out, nil
	}

	pathRows, err := db.QueryContext(ctx, `
		SELECT id, domain_id, to_jsonb(path_pattern)::text,
		       match_type::text, action::text
		FROM "interception_path"
		ORDER BY domain_id ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("load interception paths: %w", err)
	}
	defer pathRows.Close() //nolint:errcheck

	var scannedPaths []InterceptionPathRow
	for pathRows.Next() {
		var p InterceptionPathRow
		if err := pathRows.Scan(&p.ID, &p.DomainID, &p.PatternsJSON, &p.MatchType, &p.Action); err != nil {
			return nil, fmt.Errorf("scan interception path: %w", err)
		}
		scannedPaths = append(scannedPaths, p)
	}
	if err := pathRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interception paths: %w", err)
	}

	return attachInterceptionPaths(out, domainsByID, scannedPaths)
}

// decodeInterceptionDomainRows converts the scanned rows into the
// runtime domainpolicy slice plus an id→index map for the path-join.
// Enum upper-casing matches the domainpolicy enum constants so the
// engine's switch statements stay deterministic regardless of how the
// admin UI capitalises the inputs.
func decodeInterceptionDomainRows(rows []InterceptionDomainRow) ([]domain.InterceptionDomain, map[string]int) {
	out := make([]domain.InterceptionDomain, 0, len(rows))
	domainsByID := make(map[string]int, len(rows))
	for _, r := range rows {
		d := domain.InterceptionDomain{
			ID:                      r.ID,
			Name:                    r.Name,
			HostPattern:             r.HostPattern,
			HostMatchType:           domain.HostMatchType(strings.ToUpper(r.HostMatch)),
			AdapterID:               r.AdapterID,
			NetworkZone:             domain.NetworkZone(strings.ToUpper(r.Zone)),
			DefaultPathAction:       domain.PathAction(strings.ToUpper(r.DefaultPathAction)),
			OnAdapterError:          domain.AdapterErrorBehavior(strings.ToUpper(r.OnAdapterError)),
			Enabled:                 r.Enabled,
			Priority:                r.Priority,
			UpdatedAt:               r.UpdatedAt,
			StreamingMode:           r.StreamingMode,
			StreamingChunkBytes:     r.StreamingChunkBytes,
			StreamingHookTimeoutMs:  r.StreamingHookTimeoutMs,
			StreamingMaxBufferBytes: r.StreamingMaxBufferBytes,
			StreamingFailBehavior:   r.StreamingFailBehavior,
			CaptureRequestBody:      r.CaptureRequestBody,
			CaptureResponseBody:     r.CaptureResponseBody,
			RawBodySpillEnabled:     r.RawBodySpillEnabled,
			Paths:                   nil,
		}
		domainsByID[d.ID] = len(out)
		out = append(out, d)
	}
	return out, domainsByID
}

// attachInterceptionPaths decodes path rows and stamps them onto the
// matching domain. A path row whose domain_id has no matching parent in
// the domain slice is silently dropped — this happens when a path
// references a disabled (and therefore not-loaded) domain. Malformed
// PatternsJSON aborts the whole load with a wrapped error so the
// compliance-proxy does not start with a domain whose path-rule set is
// silently truncated.
func attachInterceptionPaths(
	out []domain.InterceptionDomain,
	domainsByID map[string]int,
	rows []InterceptionPathRow,
) ([]domain.InterceptionDomain, error) {
	for _, pr := range rows {
		var patterns []string
		if err := json.Unmarshal([]byte(pr.PatternsJSON), &patterns); err != nil {
			return nil, fmt.Errorf("decode path_pattern (%q): %w", pr.PatternsJSON, err)
		}
		p := domain.InterceptionPath{
			ID:          pr.ID,
			PathPattern: patterns,
			MatchType:   domain.PathMatchType(strings.ToUpper(pr.MatchType)),
			Action:      domain.PathAction(strings.ToUpper(pr.Action)),
		}
		idx, ok := domainsByID[pr.DomainID]
		if !ok {
			continue
		}
		out[idx].Paths = append(out[idx].Paths, p)
	}
	return out, nil
}
