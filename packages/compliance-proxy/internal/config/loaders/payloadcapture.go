package loaders

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// systemMetadataPayloadCaptureKey is the row key holding the admin-editable
// payload capture configuration. Mirrors system_metadata["observability.config"].
const systemMetadataPayloadCaptureKey = "payload_capture.config"

// LoadPayloadCaptureConfig reads system_metadata["payload_capture.config"]
// and returns the decoded payload-capture Config. A missing row or a
// malformed JSON blob yields the default (capture flags off, 64 KiB
// audit truncation cap, 10 MiB network read caps) so a fresh deployment
// never captures traffic by surprise and never silently truncates
// forwarded bodies. Decoding + coercion are delegated to
// payloadcapture.DecodeConfigJSON so this path stays consistent with
// the shadow reducer and the AI gateway loader.
//
// The post-query branching (missing row → default, transient err →
// default + wrapped err, decode err → default + wrapped err, success →
// decoded config) is split into decodePayloadCaptureResult so unit
// tests can exercise every branch without a live *sql.DB.
func LoadPayloadCaptureConfig(ctx context.Context, db *sql.DB) (payloadcapture.Config, error) {
	if db == nil {
		return payloadcapture.DefaultConfig(), nil
	}
	var val []byte
	err := db.QueryRowContext(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`,
		systemMetadataPayloadCaptureKey,
	).Scan(&val)
	return decodePayloadCaptureResult(val, err)
}

// decodePayloadCaptureResult applies the four-way decision over a
// system_metadata["payload_capture.config"] query outcome. Branches:
//   - queryErr == sql.ErrNoRows → DefaultConfig() + nil err (fresh deploy).
//   - queryErr != nil           → DefaultConfig() + wrapped query err.
//   - decode err                → DefaultConfig() + wrapped decode err.
//   - success                   → coerced Config + nil err.
func decodePayloadCaptureResult(val []byte, queryErr error) (payloadcapture.Config, error) {
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return payloadcapture.DefaultConfig(), nil
		}
		return payloadcapture.DefaultConfig(), fmt.Errorf("payload capture: query system_metadata: %w", queryErr)
	}
	cfg, err := payloadcapture.DecodeConfigJSON(val)
	if err != nil {
		return payloadcapture.DefaultConfig(), fmt.Errorf("payload capture: %w", err)
	}
	return cfg, nil
}
