// Package configdispatch: configdispatch_streaming.go — the streaming-compliance
// Type-B receiver.
//
// Split out because getting this contract wrong is invisible. Three things said
// the right thing and one inline comment said the wrong one, and the code
// followed the comment:
//
//   - configuration-architecture.md: "Type B = state is empty/null; the version
//     bump is a 'go reload' signal and the actual data lives in a dedicated DB
//     table or system_metadata key."
//   - this function's OWN doc comment, below, has always said "the handler
//     re-reads system_metadata['streaming_compliance.config']".
//   - registerPayloadCapture, its Type-B sibling thirty lines above in the file
//     this came from, ignores the pushed payload and re-reads. It is the
//     reference implementation.
//
// The inline comment that won said "Hub now pushes the raw blob … No DB re-read
// needed", and the handler passed the empty payload to ApplyShadowState, which
// decoded it into DEFAULTS. Measured on the compliance proxy: boot installed the
// admin's chunked_async at 10:30:50.045; this handler replaced it with the
// built-in passthrough 70 ms later. Every SSE stream was then relayed
// uninspected — passthrough neither accumulates nor can reject.
package configdispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	cfgloader "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/configloader"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// registerStreamingCompliance wires the `streaming_compliance` Type B
// shadow receiver. CP fires the invalidation with an empty state; the
// handler re-reads system_metadata['streaming_compliance.config'] via
// the canonical streampolicy.LoadGlobalDefault loader and pushes the
// resulting Policy onto the live ProxyServer via SetStreamingPolicyGlobal.
//
// ConfigDB may be nil in tests / minimal embeds (the handler is
// tolerant — a nil DB just means we skip the reload). ProxyServer may
// also be nil in unit-level loader tests; the handler tolerates that too so
// BuildConfigLoader can be invoked from a thin test harness.
func registerStreamingCompliance(l *cfgloader.Loader, d Deps) {
	cfgloader.RegisterRaw(l, configkey.StreamingCompliance, func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		if d.StreamingPolicyStore == nil {
			return nil, nil
		}
		// A trigger means RE-READ. The payload is applied only when it actually
		// carries state, so a Type-A push (should one ever arrive on this key)
		// still works.
		effective := raw
		if isEmptyRawPayload(raw) {
			reread, rrErr := loadStreamingPolicyRaw(ctx, d.ConfigDB)
			if rrErr != nil {
				// Keep the current policy rather than reverting to defaults: a
				// transient DB error must not silently disable stream inspection.
				d.Logger.Warn("streaming compliance re-read failed on invalidation; keeping the current policy",
					"error", rrErr)
				return nil, nil
			}
			if isEmptyRawPayload(reread) {
				// Nothing configured anywhere — leave the store as it is.
				return nil, nil
			}
			effective = reread
		}
		if err := d.StreamingPolicyStore.ApplyShadowState(ctx, effective); err != nil {
			return nil, fmt.Errorf("apply streaming compliance shadow state: %w", err)
		}
		policy := d.StreamingPolicyStore.Get()
		d.Logger.Info("streaming compliance policy reloaded",
			"mode", string(policy.Mode),
			"failBehavior", string(policy.FailBehavior),
			"chunkBytes", policy.ChunkBytes,
			"hookTimeoutMs", policy.HookTimeoutMs,
		)
		return nil, nil
	})
}

// isEmptyRawPayload reports whether a pushed shadow value carries no state.
// json.RawMessage("null") is four bytes, so a len()==0 check alone does not
// catch what the Hub actually sends for a Type-B key.
func isEmptyRawPayload(raw []byte) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null" || t == "{}"
}

// loadStreamingPolicyRaw re-reads the admin's streaming-policy blob from the
// same system_metadata key the boot path reads, so an invalidation trigger
// resolves to the same value a restart would.
func loadStreamingPolicyRaw(ctx context.Context, db *sql.DB) ([]byte, error) {
	if db == nil {
		return nil, nil
	}
	var raw []byte
	err := db.QueryRowContext(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`,
		streampolicy.SystemMetadataKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}
